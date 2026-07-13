//go:build sim

package simulator

import (
	"context"
	"log"
	"math/rand"
	"time"

	"shingo/shared/clock"
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
	hasRobot    bool      // holds a robot from the finite fleet (G16)
	queuedSince time.Time // non-zero while waiting for a free robot (G16)
	staged      bool      // driven to WAITING (status "staged") at a wait dwell
	heldAt      string    // non-empty while stalled at an occupied position (log-once)
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
	maxInUse    int
	maxQueued   int
	lastMetric  time.Time     // instant of the last metric accrual
	robotBusy   time.Duration // ∫ robotsInUse dt
	queueWait   time.Duration // ∫ queuedCount dt
	elapsed     time.Duration // ∫ dt since the first step
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
	return &Driver{
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
		// Finite fleet (G16): a move needs a free robot. If the pool is full
		// the order queues — it stays CREATED, retries next tick, and accrues
		// queue-wait. No PRNG is drawn while queued, so the seeded draw
		// sequence is identical for any order that never has to wait.
		if d.fleetSize > 0 && d.robotsInUse >= d.fleetSize {
			d.enqueue(now, p)
			p.deadline = now.Add(time.Second)
			return
		}
		d.dequeue(p) // leaving CREATED this tick, whether we fault or depart
		if d.maybeFault(vid) {
			p.phase = phaseDone
			return
		}
		d.acquireRobot(p)
		// Carry a robot ID on the first RUNNING transition. Core gates the
		// waybill — and thus the acknowledged→in_transit transition — on first
		// robot assignment (wiring_vendor_status.go). Real RDS reports a vehicle
		// ID here; the plain DriveState passes "" and the order stalls at
		// acknowledged (staged then can't apply). One bot per order suits the
		// infinite fleet; a finite pool of reused IDs is a G16 follow-up.
		d.sim.DriveStateWithRobot(vid, "RUNNING", "sim-bot-"+vid)
		p.phase = phaseRunning
		p.blockIndex = 0
		p.deadline = d.nextDeadline(now)

	case phaseRunning:
		blocks := ov.Blocks
		// No more released blocks to process.
		if p.blockIndex >= len(blocks) {
			if ov.Complete {
				if d.maybeFault(vid) {
					d.markDone(p)
					return
				}
				d.sim.DriveState(vid, "FINISHED")
				d.markDone(p)
				return
			}
			// Staged order dwelling at a wait point — more blocks arrive via
			// ReleaseOrder. Drive WAITING (→ status "staged") once: real RDS
			// reports WAITING here (material_orders.go), and Edge's swap-ready
			// auto-release keys on the "staged" transition. Without it the order
			// reads as a frozen in_transit and the swap never releases.
			if !p.staged {
				d.sim.DriveState(vid, "WAITING")
				p.staged = true
			}
			p.deadline = now.Add(time.Second)
			return
		}

		// Blocks were released after the wait — resume movement from staged.
		if p.staged {
			d.sim.DriveState(vid, "RUNNING")
			p.staged = false
		}

		if d.maybeFault(vid) {
			d.markDone(p)
			return
		}

		// The final block of a complete order is represented by FINISHED, whose
		// delivery the engine records via handleOrderDelivered (T2.2 rationale).
		// It is still a physical placement, so it takes the same occupancy hold.
		if ov.Complete && p.blockIndex == len(blocks)-1 {
			if d.holdForPosition(now, vid, blocks[p.blockIndex].Location, blocks[p.blockIndex].BinTask, p) {
				return
			}
			d.sim.DriveState(vid, "FINISHED")
			d.markDone(p)
			return
		}

		b := blocks[p.blockIndex]
		if d.holdForPosition(now, vid, b.Location, b.BinTask, p) {
			return
		}
		d.sim.CompleteBlock(vid, b.BlockID, b.Location, b.BinTask)
		p.blockIndex++
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

// acquireRobot takes a robot from the finite pool. Call only after confirming a
// robot is free; a no-op for the infinite fleet.
func (d *Driver) acquireRobot(p *orderProgress) {
	if d.fleetSize <= 0 {
		return
	}
	d.robotsInUse++
	if d.robotsInUse > d.maxInUse {
		d.maxInUse = d.robotsInUse
	}
	p.hasRobot = true
}

// releaseRobot returns a held robot to the pool.
func (d *Driver) releaseRobot(p *orderProgress) {
	if p.hasRobot {
		d.robotsInUse--
		p.hasRobot = false
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

// maybeFault rolls the seeded PRNG and, with probability failRate, drives the
// order to FAILED. Returns true if it faulted. Draws no value when failRate<=0
// so the zero-fault path keeps a clean (jitter-only) draw sequence.
func (d *Driver) maybeFault(vid string) bool {
	if d.failRate <= 0 {
		return false
	}
	if d.rng.Float64() < d.failRate {
		d.sim.DriveState(vid, "FAILED")
		return true
	}
	return false
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
func (d *Driver) holdForPosition(now time.Time, vid, location, binTask string, p *orderProgress) bool {
	g := d.sim.PositionGate()
	if g == nil || location == "" {
		return false
	}
	ok, blockedBy := g.CanEnterPosition(vid, location, binTask)
	if ok {
		if p.heldAt != "" {
			log.Printf("[sim] order %s resumed at %s (position cleared)", vid, p.heldAt)
			p.heldAt = ""
		}
		return false
	}
	if p.heldAt != location {
		log.Printf("[sim] order %s HOLDING at %s — %s (a robot cannot place onto an occupied position)",
			vid, location, blockedBy)
		p.heldAt = location
	}
	// Re-check on the next tick. Deliberately does NOT draw from the PRNG, so the
	// seeded draw sequence stays identical for any order that never has to hold.
	p.deadline = now.Add(time.Second)
	return true
}
