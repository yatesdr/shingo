//go:build sim

package simulator

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"sync"
	"time"

	"shingo/protocol/clock"
	"shingocore/config"
)

// defaultRetention is how long a terminal order lingers before the eviction
// sweep reaps it. Not currently exposed in SimConfig (add a yaml key if a plant
// needs to tune it); 10m is long enough to inspect a finished order in the UI.
const defaultRetention = 10 * time.Minute

// createdFraction is the share of a transit time an order spends in CREATED
// before going RUNNING (brief T2.3: "CREATED →(0.3×transit)→ RUNNING"). Flat,
// not jittered, so it draws no PRNG value and keeps the draw sequence simple.
const createdFraction = 0.3

// driverPhase tracks where the driver has taken an order.
type driverPhase int

const (
	phaseCreated driverPhase = iota // waiting to go RUNNING
	phaseRunning                    // RUNNING, completing blocks one at a time
	phaseDone                       // driver finished with it (FINISHED/FAILED/STOPPED)
)

// orderProgress is the driver's private per-order bookkeeping. Never shared with
// the engine — the driver owns it and only its single goroutine (or a test
// thread) touches it, so no lock is needed.
type orderProgress struct {
	phase       driverPhase
	blockIndex  int       // next block to complete
	deadline    time.Time // when the next transition is due
	robotID     string    // non-empty while holding a robot from the pool (G16)
	blockStart  time.Time // when the in-flight block began — the sim's startTime (8c)
	queuedSince time.Time // non-zero while waiting for a free robot (G16)
	staged      bool      // driven to WAITING (status "staged") at a wait dwell
	heldAt      string    // non-empty while stalled at an occupied position (log-once)
	heldSince   time.Time // when the current hold began — bounds an unresolvable hold
	heldWarned  bool      // the unresolvable-hold diagnostic is printed once per hold
	// pendingFault: the fault die said FAILED and the report deferred (Fix
	// A) — the retry re-reports exactly that FAILED, roll-free. A retry is
	// not a re-decision, and not re-rolling also keeps a deferral free of
	// extra PRNG draws (the fleet-full arm's "no PRNG while queued" rule,
	// generalized to every deferred transition).
	pendingFault bool
}

// Driver advances simulated orders through their lifecycle on a clock tick,
// emitting per-block completions (T2.2) and finishing or failing orders with
// jittered, seeded timing. It is the sim-mode replacement for the SEER RDS
// poller that would otherwise drive these transitions from real robot motion.
type Driver struct {
	sim        *SimulatorBackend
	clk        clock.Clock
	rng        *rand.Rand
	transit    time.Duration
	transitMin time.Duration // finite-fleet uniform-transit lower bound (G16); 0 = use transit±jitter
	transitMax time.Duration // finite-fleet uniform-transit upper bound (G16)
	jitter     float64       // fractional jitter, e.g. 0.2 → ±20%
	failRate   float64       // 0..1 probability a transition faults instead
	retention  time.Duration
	progress   map[string]*orderProgress

	// Finite-fleet state (G16). Touched only by the driver goroutine (or the
	// test thread), like progress — no locking. fleetSize 0 is the legacy
	// infinite fleet: robotsInUse/queuedCount stay 0 and Metrics() is empty.
	fleetSize   int
	robotsInUse int
	queuedCount int

	// Robot identity. Until 2026-07-25 the driver stamped "sim-bot-"+vendorID
	// on every RUNNING transition, so every order had its own one-mission
	// "robot" — which silently disabled the per-robot breakdown, the robot
	// filter, utilization and hourly concurrency. Every fleet-shaped metric in
	// store/telemetry was untestable.
	//
	// freeRobots is a FIFO of available names. FIFO, not LIFO, so a lightly
	// loaded sim rotates through the pool instead of pinning AMR-01 — which
	// also happens to be what a real dispatcher does (longest-idle first).
	//
	// Finite fleet: pre-minted at construction, exactly fleetSize names, never
	// grows (advance only acquires after confirming a robot is free).
	// Infinite fleet: starts empty and mints on demand, so the pool settles at
	// the run's peak concurrency. Identity is exclusive either way — two live
	// orders never share a name.
	freeRobots []string
	mintedBots int
	maxInUse   int
	maxQueued  int
	lastMetric time.Time     // instant of the last metric accrual
	robotBusy  time.Duration // ∫ robotsInUse dt
	queueWait  time.Duration // ∫ queuedCount dt
	elapsed    time.Duration // ∫ dt since the first step

	// ── THE FLEET, PUBLISHED FOR READERS ON OTHER GOROUTINES ─────────────
	//
	// Everything above is touched only by step(), which runs on one
	// goroutine — that is why none of it is locked, and the metric readout
	// says so explicitly. GetRobotsStatus is called from HTTP handlers and
	// from the confidence collector, so it cannot read freeRobots or
	// progress directly without a race.
	//
	// So step() PUBLISHES a snapshot at the end of each tick and readers take
	// a copy under this lock. One writer, many readers, and the driver stays
	// single-threaded.
	fleetMu   sync.RWMutex
	fleetSnap []FleetRobot
}

// FleetRobot is one member of the simulated fleet as the driver knows it:
// a durable name from the pool, whether it is currently holding an order, and
// a rough idea of where that order started.
type FleetRobot struct {
	ID   string
	Busy bool
	// At is the first block location of the order this robot is running, or ""
	// when it is free. AN APPROXIMATION, and the only one available: the
	// simulator has no position model, so this is where the work is, not where
	// the robot is. It exists so a board renders somewhere rather than nowhere.
	// Nothing that decides anything may read it — see the note on
	// GetRobotsStatus about recovery tier 3.
	At string
}

// NewDriver builds a Driver from sim config. Exported so callers can construct
// one, store it, and defer the goroutine launch until engine wiring completes
// (see Item 3, sim startup race). Tests can call step() directly with a manual
// clock — fully synchronous and deterministic, no goroutine.
func NewDriver(sim *SimulatorBackend, cfg config.SimConfig, clk clock.Clock, rng *rand.Rand) *Driver {
	transit := cfg.TransitTime
	if transit <= 0 {
		transit = 5 * time.Second
	}
	transitMin, transitMax := cfg.TransitMin, cfg.TransitMax
	// Scale transit by speed when using a real clock (G4). When using SimClock
	// (fast-forward), the clock already scales time so transit stays at its
	// base value — double-scaling would be wrong.
	if _, ok := clk.(*clock.SimClock); !ok {
		transit = cfg.Scaled(transit)
		transitMin = cfg.Scaled(transitMin)
		transitMax = cfg.Scaled(transitMax)
	}
	d := &Driver{
		sim:        sim,
		clk:        clk,
		rng:        rng,
		transit:    transit,
		transitMin: transitMin,
		transitMax: transitMax,
		jitter:     cfg.JitterPct,
		failRate:   cfg.FailRate,
		retention:  defaultRetention,
		progress:   make(map[string]*orderProgress),
		fleetSize:  cfg.FleetSize,
	}
	// Mint the named fleet up front for a finite pool (sim.fleet_size in the
	// YAML — 20 on the dev plant, 7 at Springfield). The infinite fleet mints
	// on demand instead; see the freeRobots comment.
	for i := 0; i < cfg.FleetSize; i++ {
		d.mintedBots++
		d.freeRobots = append(d.freeRobots, robotName(d.mintedBots))
	}
	return d
}

// robotName renders a pool slot as a plant-style vehicle ID. Two digits keeps
// AMR-01..AMR-99 sorting lexically, which is how every per-robot breakdown
// orders its rows.
func robotName(n int) string {
	return fmt.Sprintf("AMR-%02d", n)
}

// StartDriver constructs the driver and runs its tick loop until ctx is done.
// DEPRECATED for engine wiring: prefer SimulatorBackend.NewDriverFromConfig +
// SimulatorBackend.StartDriver so the goroutine launch is deferred past engine
// event-handler wiring (Item 3, sim startup race). Retained for tests that
// need a one-shot create+start.
func StartDriver(ctx context.Context, sim *SimulatorBackend, cfg config.SimConfig, clk clock.Clock, rng *rand.Rand) *Driver {
	d := NewDriver(sim, cfg, clk, rng)
	go d.run(ctx)
	return d
}

func (d *Driver) run(ctx context.Context) {
	// The driver ticks at 1 sim-second intervals. When using SimClock
	// (fast-forward), the ticker fires at 1s/speed real time — so at
	// 100× speed it fires every 10ms, stepping once per sim-second.
	// This naturally sustains high speed without explicit batching.
	ticker := d.clk.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C():
			d.step(now)
			// Periodic finite-fleet readout for the robot-sizing loop (G16):
			// every ~10 simulated minutes when a finite fleet is configured,
			// silent for the infinite fleet. Same goroutine as step(), so the
			// metric read is race-free.
			ticks++
			if d.fleetSize > 0 && ticks%600 == 0 {
				m := d.Metrics()
				log.Printf("[sim] fleet size=%d util=%.0f%% peak_busy=%d peak_queue=%d queued_now=%d queue_wait_total=%s",
					m.FleetSize, m.Utilization*100, m.MaxRobotsInUse, m.MaxQueued,
					m.OrdersQueuedNow, m.QueueWaitTotal.Round(time.Second))
			}
		}
	}
}

// step advances every active order whose deadline has passed, then runs the
// eviction sweep and forgets bookkeeping for orders the sim no longer holds.
// Orders are visited in creation order (VendorOrderIDs) so the PRNG draw
// sequence is identical across runs with the same seed — the determinism the
// future DST suite depends on.
func (d *Driver) step(now time.Time) {
	d.accrue(now)
	defer d.publishFleet()
	for _, vid := range d.sim.VendorOrderIDs() {
		ov := d.sim.GetOrder(vid)
		if ov == nil {
			continue
		}
		p, tracked := d.progress[vid]
		if !tracked {
			// Don't pick up an order that's already terminal (e.g. cancelled
			// before the driver ever saw it).
			if isEvictableTerminal(ov.State) {
				continue
			}
			d.progress[vid] = &orderProgress{
				phase:    phaseCreated,
				deadline: now.Add(time.Duration(createdFraction * float64(d.transit))),
			}
			continue // scheduled this tick; first advance happens on a later tick
		}
		if p.phase == phaseDone {
			continue
		}
		// The engine cancelled (or otherwise terminated) the order out from
		// under us — stop driving it (releasing any robot it held).
		if isEvictableTerminal(ov.State) {
			d.markDone(p)
			continue
		}
		if now.Before(p.deadline) {
			continue
		}
		d.advance(now, vid, ov, p)
	}

	d.sim.EvictTerminalBefore(now.Add(-d.retention))
	d.gcProgress()
}

// advance performs one due transition for a single order.
func (d *Driver) advance(now time.Time, vid string, ov *OrderView, p *orderProgress) {
	switch p.phase {
	case phaseCreated:
		// A fault die already cast whose FAILED report deferred (Fix A) is
		// re-reported FIRST, before any other arm — the verdict is cast, and
		// holding its report behind a robot wait would invert the order.
		if d.retryPendingFault(vid, p, now) {
			return
		}
		// Finite fleet (G16): a move needs a free robot. If the pool is full
		// the order queues — it stays CREATED, retries next tick, and accrues
		// queue-wait. No PRNG is drawn while queued, so the seeded draw
		// sequence is identical for any order that never has to wait.
		// An order retrying a deferred RUNNING already holds its robot (Fix
		// A), so the full pool it counts toward is not full FOR IT.
		if d.fleetSize > 0 && p.robotID == "" && d.robotsInUse >= d.fleetSize {
			d.enqueue(now, p)
			p.deadline = now.Add(time.Second)
			return
		}
		d.dequeue(p) // leaving CREATED this tick, unless the RUNNING report defers
		switch d.faultOrDefer(vid, p, now) {
		case faultDone:
			p.phase = phaseDone
			return
		case faultDeferred:
			return
		}
		if p.robotID == "" {
			d.acquireRobot(p) // a retried attempt re-supplies the robot it holds
		}
		// Carry a robot ID on the first RUNNING transition. Core gates the
		// waybill — and thus the acknowledged→in_transit transition — on first
		// robot assignment (wiring_vendor_status.go). Real RDS reports a vehicle
		// ID here; the plain DriveState passes "" and the order stalls at
		// acknowledged (staged then can't apply).
		//
		// The ID comes from the pool, not from the order. It used to be
		// "sim-bot-"+vid — one robot per order, so the fleet had as many
		// members as the run had orders and every fleet-shaped metric read as
		// a population of one.
		//
		// If the RUNNING report defers (Fix A) the order keeps the robot it
		// acquired: releasing it would hand the slot to a co-queued order only
		// to have the next retry immediately re-take it, and re-acquiring on
		// the retry would corrupt the in-use count. Phase stays CREATED and
		// the transition retries on the re-armed deadline.
		if _, _, dfr := d.sim.DriveStateWithRobot(vid, "RUNNING", p.robotID); dfr {
			d.holdDeferred(p, now)
			return
		}
		p.phase = phaseRunning
		p.blockIndex = 0
		p.blockStart = now
		p.deadline = d.nextDeadline(now)

	case phaseRunning:
		blocks := ov.Blocks
		// A fault verdict already cast whose FAILED report deferred (Fix A)
		// is re-reported FIRST — see phaseCreated.
		if d.retryPendingFault(vid, p, now) {
			return
		}
		// No more released blocks to process.
		if p.blockIndex >= len(blocks) {
			if ov.Complete {
				switch d.faultOrDefer(vid, p, now) {
				case faultDone:
					d.markDone(p)
					return
				case faultDeferred:
					return
				}
				if _, _, dfr := d.sim.DriveState(vid, "FINISHED"); dfr {
					// HOLD THE TRANSITION (Fix A): no phase advance, no
					// markDone — markDone releases the robot and forgets the
					// slot, and a robot released behind a FINISHED that never
					// landed is the double-assign shape this fix exists to
					// kill. Retry on the re-armed deadline.
					d.holdDeferred(p, now)
					return
				}
				d.markDone(p)
				return
			}
			// Staged order dwelling at a wait point — more blocks arrive via
			// ReleaseOrder. Drive WAITING (→ status "staged") once: real RDS
			// reports WAITING here (material_orders.go), and Edge's swap-ready
			// auto-release keys on the "staged" transition. Without it the order
			// reads as a frozen in_transit and the swap never releases.
			if !p.staged {
				if _, _, dfr := d.sim.DriveState(vid, "WAITING"); dfr {
					// HOLD THE TRANSITION (Fix A): p.staged must NOT latch
					// here — the latch was exactly what made the old
					// dropped-emission loss permanent (the order then dwelled
					// as frozen in_transit with the WAITING never retried).
					// The retry re-enters this arm with p.staged still false
					// and drives WAITING again.
					d.holdDeferred(p, now)
					return
				}
				p.staged = true
			}
			p.deadline = now.Add(time.Second)
			return
		}

		// Blocks were released after the wait — resume movement from staged.
		if p.staged {
			if _, _, dfr := d.sim.DriveState(vid, "RUNNING"); dfr {
				// HOLD THE TRANSITION (Fix A): keep p.staged latched so the
				// retry re-takes the resume arm; clearing it before the
				// transition commits would strand the order in "staged"
				// status with no live transition to leave it.
				d.holdDeferred(p, now)
				return
			}
			p.staged = false
			// The staged dwell belongs to the WAIT, not to the block that
			// follows it, so the block clock restarts on resume.
			p.blockStart = now
		}

		switch d.faultOrDefer(vid, p, now) {
		case faultDone:
			d.markDone(p)
			return
		case faultDeferred:
			return
		}

		// The final block of a complete order is represented by FINISHED, whose
		// delivery the engine records via handleOrderDelivered (T2.2 rationale).
		// It is still a physical placement, so it takes the same occupancy hold.
		if ov.Complete && p.blockIndex == len(blocks)-1 {
			if d.holdForPosition(now, vid, blocks[p.blockIndex].Location, blocks[p.blockIndex].BinTask, p) {
				return
			}
			if _, _, dfr := d.sim.DriveState(vid, "FINISHED"); dfr {
				// HOLD THE TRANSITION (Fix A): the position hold was released
				// by holdForPosition returning false, so a retry re-takes the
				// hold check first — fine, it re-validates and a gate-passing
				// retry passes again. Do not markDone: the order's robot must
				// stay assigned until the FINISHED report lands, exactly as in
				// the drain arm above.
				d.holdDeferred(p, now)
				return
			}
			d.markDone(p)
			return
		}

		b := blocks[p.blockIndex]
		if d.holdForPosition(now, vid, b.Location, b.BinTask, p) {
			return
		}
		d.sim.CompleteBlock(vid, b.BlockID, b.Location, b.BinTask, p.blockStart.Unix(), now.Unix())
		p.blockIndex++
		p.blockStart = now
		p.deadline = d.nextDeadline(now)
	}
}

// --- Finite-fleet helpers (G16) ---------------------------------------------
//
// A robot is held from the moment an order departs CREATED (goes RUNNING) until
// it reaches a terminal driver phase. With fleetSize 0 these are all no-ops and
// the simulator behaves as the legacy infinite fleet.

// enqueue marks an order as waiting for a free robot. Idempotent across the
// ticks it spends queued (queuedSince is set once).
func (d *Driver) enqueue(now time.Time, p *orderProgress) {
	if p.queuedSince.IsZero() {
		p.queuedSince = now
		d.queuedCount++
		if d.queuedCount > d.maxQueued {
			d.maxQueued = d.queuedCount
		}
	}
}

// dequeue clears the waiting marker when an order leaves CREATED.
func (d *Driver) dequeue(p *orderProgress) {
	if !p.queuedSince.IsZero() {
		p.queuedSince = time.Time{}
		d.queuedCount--
	}
}

// publishFleet copies the pool into the snapshot readers see.
//
// BOTH HALVES, and that is the point. The free robots are as much a part of
// the fleet as the busy ones — a board that shows only robots currently
// holding an order shows a fleet that shrinks to nothing when the plant goes
// quiet, and every gate that asks "is there a robot that could take this"
// answers no.
func (d *Driver) publishFleet() {
	snap := make([]FleetRobot, 0, len(d.freeRobots)+len(d.progress))
	for vid, p := range d.progress {
		if p.robotID == "" {
			continue
		}
		at := ""
		if ov := d.sim.GetOrder(vid); ov != nil && len(ov.Blocks) > 0 {
			at = ov.Blocks[0].Location
		}
		snap = append(snap, FleetRobot{ID: p.robotID, Busy: true, At: at})
	}
	for _, id := range d.freeRobots {
		snap = append(snap, FleetRobot{ID: id, Busy: false})
	}
	sort.Slice(snap, func(i, j int) bool { return snap[i].ID < snap[j].ID })

	d.fleetMu.Lock()
	d.fleetSnap = snap
	d.fleetMu.Unlock()
}

// Fleet returns the simulated fleet as of the last tick. Safe from any
// goroutine; nil before the driver's first step.
func (d *Driver) Fleet() []FleetRobot {
	d.fleetMu.RLock()
	defer d.fleetMu.RUnlock()
	return append([]FleetRobot(nil), d.fleetSnap...)
}

// acquireRobot takes a robot from the pool and records its ID on the order.
// For the finite fleet the caller has already confirmed one is free, so the
// pre-minted free list is never empty here; for the infinite fleet the pool
// grows by one name when it runs dry.
func (d *Driver) acquireRobot(p *orderProgress) {
	if len(d.freeRobots) == 0 {
		d.mintedBots++
		d.freeRobots = append(d.freeRobots, robotName(d.mintedBots))
	}
	p.robotID = d.freeRobots[0]
	d.freeRobots = append(d.freeRobots[:0], d.freeRobots[1:]...)

	if d.fleetSize <= 0 {
		return
	}
	d.robotsInUse++
	if d.robotsInUse > d.maxInUse {
		d.maxInUse = d.robotsInUse
	}
}

// releaseRobot returns a held robot to the back of the pool.
func (d *Driver) releaseRobot(p *orderProgress) {
	if p.robotID == "" {
		return
	}
	d.freeRobots = append(d.freeRobots, p.robotID)
	p.robotID = ""
	if d.fleetSize > 0 {
		d.robotsInUse--
	}
}

// markDone moves an order to the terminal driver phase, releasing any robot it
// held and clearing any queue marker — so robot/queue accounting can never leak
// on a fault, finish, or engine-side cancellation.
func (d *Driver) markDone(p *orderProgress) {
	d.releaseRobot(p)
	d.dequeue(p)
	p.phase = phaseDone
}

// accrue integrates the finite-fleet metrics over the interval since the last
// step. robotsInUse/queuedCount are constant across [lastMetric, now] — they
// only change inside advance, after this call — so a left-Riemann sum is exact.
func (d *Driver) accrue(now time.Time) {
	if d.lastMetric.IsZero() {
		d.lastMetric = now
		return
	}
	dt := now.Sub(d.lastMetric)
	if dt <= 0 {
		return
	}
	d.robotBusy += time.Duration(d.robotsInUse) * dt
	d.queueWait += time.Duration(d.queuedCount) * dt
	d.elapsed += dt
	d.lastMetric = now
}

// FleetMetrics is a snapshot of finite-fleet utilization for the robot-sizing
// loop (§3.1). Zero-valued for the infinite fleet (fleet_size unset).
type FleetMetrics struct {
	FleetSize       int
	Elapsed         time.Duration // simulated time integrated since the first step
	RobotBusyTime   time.Duration // Σ robot-busy time (∫ robotsInUse dt)
	Utilization     float64       // RobotBusyTime / (FleetSize × Elapsed), 0..1
	QueueWaitTotal  time.Duration // Σ order-time spent waiting for a robot
	OrdersQueuedNow int           // orders currently waiting for a robot
	MaxRobotsInUse  int           // peak concurrent robots in use
	MaxQueued       int           // peak concurrent queue depth
}

// Metrics returns the current finite-fleet snapshot for the sizing loops. Call
// it from the driver goroutine or after the driver has stopped — the fields are
// not lock-protected (the driver is deliberately single-goroutine).
func (d *Driver) Metrics() FleetMetrics {
	m := FleetMetrics{
		FleetSize:       d.fleetSize,
		Elapsed:         d.elapsed,
		RobotBusyTime:   d.robotBusy,
		QueueWaitTotal:  d.queueWait,
		OrdersQueuedNow: d.queuedCount,
		MaxRobotsInUse:  d.maxInUse,
		MaxQueued:       d.maxQueued,
	}
	if d.fleetSize > 0 && d.elapsed > 0 {
		m.Utilization = float64(d.robotBusy) / (float64(d.fleetSize) * float64(d.elapsed))
	}
	return m
}

// faultResult is what a fault roll means for the attempt in flight (Fix A:
// the FAILED report is a DriveState like any other, so it can defer too).
type faultResult int

const (
	faultNone     faultResult = iota // no fault drawn — proceed with the transition
	faultDeferred                    // FAILED drawn, but the report deferred — hold and retry
	faultDone                        // FAILED committed — the order is done failing
)

// faultOrDefer rolls the fault die (no value drawn while failRate<=0, so the
// zero-fault path keeps a clean jitter-only draw sequence) and, when it says
// fail, reports FAILED to the backend. The FAILED report is a DriveState like
// any other, so it can defer too:
//
//   - faultNone: no fault drawn — the caller proceeds with its transition.
//   - faultDone: FAILED committed; the caller marks the order done.
//   - faultDeferred: FAILED drawn but its report did not land — the order is
//     latched on pendingFault and parked for this tick; the caller returns.
//
// The die is NOT re-rolled on the retry: retryPendingFault re-reports the
// already-drawn verdict, so a resolver outage cannot flip a drawn fault into
// a clean run or burn extra PRNG draws.
func (d *Driver) faultOrDefer(vid string, p *orderProgress, now time.Time) faultResult {
	if d.failRate <= 0 {
		return faultNone
	}
	if d.rng.Float64() >= d.failRate {
		return faultNone
	}
	if _, _, dfr := d.sim.DriveState(vid, "FAILED"); dfr {
		p.pendingFault = true
		d.holdDeferred(p, now)
		return faultDeferred
	}
	return faultDone
}

// retryPendingFault re-reports a FAILED whose report deferred (Fix A); true
// when the order is (still) in this routine's care and the caller must hold.
// When the report lands the order is done; the caller never learns which.
func (d *Driver) retryPendingFault(vid string, p *orderProgress, now time.Time) bool {
	if !p.pendingFault {
		return false
	}
	if _, _, dfr := d.sim.DriveState(vid, "FAILED"); dfr {
		d.holdDeferred(p, now)
		return true
	}
	p.pendingFault = false
	d.markDone(p)
	return true
}

// holdDeferred parks an order whose in-flight transition deferred (Fix A):
// no phase advance, no commit, and a retry when the re-armed deadline fires.
// The fleet-full arm in phaseCreated is the same shape. The re-arm
// deliberately does NOT draw from the PRNG — a retry is not a new move, and
// a deferral must stay as cheap to the draw sequence as a queued wait is.
func (d *Driver) holdDeferred(p *orderProgress, now time.Time) {
	p.deadline = now.Add(time.Second)
}

// nextDeadline returns now + the time for one move. With transit_min/_max set
// (finite-fleet realism, G16) it draws a uniform time in [min,max) — one PRNG
// draw. Otherwise it falls back to base transit × (1 ± jitter): one draw when
// jitter>0, none when jitter==0 (preserving the legacy seeded draw sequence).
func (d *Driver) nextDeadline(now time.Time) time.Time {
	if d.transitMin > 0 && d.transitMax > d.transitMin {
		span := d.transitMax - d.transitMin
		return now.Add(d.transitMin + time.Duration(d.rng.Float64()*float64(span)))
	}
	factor := 1.0
	if d.jitter > 0 {
		factor += d.jitter * (2*d.rng.Float64() - 1) // [1-jitter, 1+jitter)
	}
	return now.Add(time.Duration(float64(d.transit) * factor))
}

// gcProgress drops bookkeeping for orders the simulator has evicted, so the
// progress map stays bounded over long soaks.
func (d *Driver) gcProgress() {
	for vid, p := range d.progress {
		if !d.sim.HasOrder(vid) {
			// Defensive: a terminal order already released via markDone, but
			// never leak a robot or queue slot if one is reaped mid-flight.
			d.releaseRobot(p)
			d.dequeue(p)
			delete(d.progress, vid)
		}
	}
}

// holdForPosition enforces the plant's one-bin-per-node invariant: a robot cannot
// complete a block at a position already holding a bin its order does not own — it
// STALLS there until the position clears. Returns true if the order is held this
// tick (caller must not advance it).
//
// Without this the driver completes every block on a timer, so a two-robot swap
// "delivers" the empty onto the press before the other robot has lifted the full
// bin out. That is physically impossible in a plant (the robot cannot lower a bin
// onto an occupied position, so the block never FINISHes), but Core has no way to
// know the fleet lied: a completed delivery is proof the slot was empty, so the bin
// still recorded there must be a stale ghost — and Core evicts a perfectly good bin.
// Chased at length on 2026-07-13; the bug was here, not in Core.
//
// The stall is the POINT, not a side effect: a robot parked at an occupied position
// is exactly the real failure class (the Hopkinsville swap deadlock), which the
// timer-only driver could never reproduce. If an order holds forever, the sim has
// found a genuine deadlock — surface it, don't paper over it.
//
// No gate installed (unit tests, non-engine callers) = old timer-only behaviour.
// unresolvableHoldAfter is how long a position hold may run before the driver
// says out loud that it is not a queue. In SIMULATED time, like every other
// duration the driver reasons in, so it means the same thing at every speed.
//
// Five minutes is chosen against the thing it has to out-wait: a real queue
// behind a bin another order owns, which is bounded by that order's remaining
// transit. Transit is 15-20 simulated seconds and the longest observed genuine
// hold cleared well inside a minute, so five minutes cannot fire on a queue and
// still names a deadlock long before a human would think to look.
const unresolvableHoldAfter = 5 * time.Minute

func (d *Driver) holdForPosition(now time.Time, vid, location, binTask string, p *orderProgress) bool {
	g := d.sim.PositionGate()
	if g == nil || location == "" {
		return false
	}
	hold := g.CanEnterPosition(vid, location, binTask)
	if !hold.Held {
		if p.heldAt != "" {
			log.Printf("[sim] order %s resumed at %s (position cleared)", vid, p.heldAt)
			p.heldAt = ""
		}
		return false
	}
	if p.heldAt != location {
		log.Printf("[sim] order %s HOLDING at %s — %s (a robot cannot place onto an occupied position)",
			vid, location, hold.Reason)
		p.heldAt = location
		p.heldSince = now
		p.heldWarned = false
	}
	// ── A HOLD THAT CANNOT RESOLVE IS NOT A QUEUE, AND IT USED TO LOOK LIKE ONE ──
	//
	// Every hold this gate was written for is a queue: the position is occupied by
	// a bin some OTHER order owns, that order finishes, the bin leaves, and the
	// wait ends. Those clear in seconds and log "resumed".
	//
	// A hold behind a bin claimed by NOBODY has no such end. Nothing is scheduled
	// to move it, so the robot waits for the life of the process — and because
	// this function silently re-arms a one-second deadline, it did so with no
	// further output after the single line above. Two robots and two lineside
	// positions were lost about two and a half minutes into every seeded run of
	// the demo plant, and the only evidence was one log line from twenty minutes
	// earlier that read like an ordinary wait.
	//
	// THIS DOES NOT UNWEDGE ANYTHING and must not be mistaken for the fix. The
	// cause is upstream — a bare move dispatched into a single_robot cell whose
	// incumbent only that same leg was ever going to lift — and it is being
	// chased separately (ISSUE-sim-position-hold-deadlock-2026-09-06.md). What it
	// does is stop the sim lying about the shape of the failure: past the bound,
	// the hold says once, loudly, that it is not a queue and names what it is
	// waiting on. Behaviour is unchanged deliberately — releasing the robot here
	// would invent a recovery the plant does not have, and this gate exists to
	// stop the sim inventing things.
	//
	// THE ROBOT IS THE SMALLEST PART OF THE COST, and naming only the robot is
	// what made this failure look survivable. The order never reaches a terminal
	// phase, so: the CELL it was serving never swaps again (its runtime slot keeps
	// pointing at a live order and every admission surface refuses), its LINESIDE
	// POSITION is gone for the run, and every downstream order that waits on this
	// one cascades. The robot is held too — releaseRobot is reached only from
	// markDone and from gcProgress once the simulator has evicted the order, and a
	// permanently-held order reaches neither — but a fleet is elastic and a cell
	// is not.
	//
	// WHEN the diagnostic fires now splits on who owns the blocker, which the
	// gate reports as data (hold.BlockerClaimedBy) instead of prose. A blocker
	// claimed by NOBODY fires IMMEDIATELY: the case above is not a "could this
	// still be a queue?" question but a settled one, and waiting five minutes to
	// say so only delays the loudest signal the run produces. The five-minute
	// bound is kept for blockers an order owns or whose ownership is unknown
	// (the fail-closed arm cannot name an owner) — there the hold still might be
	// a genuine queue, and the bound exists to out-wait one.
	if !p.heldWarned && (hold.BlockerClaimedBy == nil || now.Sub(p.heldSince) >= unresolvableHoldAfter) {
		p.heldWarned = true
		log.Printf("[sim] order %s HAS BEEN HOLDING AT %s FOR %s AND IS NOT A QUEUE — %s. "+
			"Nothing is scheduled to move that bin, so this order never terminates: the cell it is "+
			"serving never swaps again, its lineside position is lost for the run, and the robot stays "+
			"assigned to it. See ISSUE-sim-position-hold-deadlock-2026-09-06.md",
			vid, location, now.Sub(p.heldSince).Round(time.Second), hold.Reason)
	}
	// Re-check on the next tick. Deliberately does NOT draw from the PRNG, so the
	// seeded draw sequence stays identical for any order that never has to hold.
	p.deadline = now.Add(time.Second)
	return true
}
