//go:build sim

package simulator

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingocore/config"
	"shingocore/fleet"
)

var driverStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seqResolver maps "sim-N" → N so the driver can resolve distinct order IDs.
type seqResolver struct{}

func (seqResolver) ResolveVendorOrderID(vid string) (int64, error) {
	var n int64
	fmt.Sscanf(vid, "sim-%d", &n)
	return n, nil
}

// runTicks advances the manual clock one second at a time and steps the driver
// each tick — the same cadence StartDriver's goroutine uses, but synchronous.
func runTicks(d *Driver, m *clock.Manual, n int) {
	for i := 0; i < n; i++ {
		m.Advance(time.Second)
		d.step(m.Now())
	}
}

func newTestDriver(t *testing.T, cfg config.SimConfig, seed int64) (*Driver, *SimulatorBackend, *clock.Manual, *captureEmitter) {
	t.Helper()
	m := clock.NewManual(driverStart)
	em := &captureEmitter{}
	s := New(WithClock(m))
	s.InitTracker(em, seqResolver{})
	d := NewDriver(s, cfg, m, rand.New(rand.NewSource(seed)))
	return d, s, m, em
}

// T2.3 / Gate 2: an order advances CREATED → RUNNING → (block) → FINISHED, and
// the intermediate pickup block fires EmitBlockCompleted while the final
// delivery is represented by FINISHED (no block-completed for it).
func TestDriverAdvancesToFinished(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}
	d, s, m, em := newTestDriver(t, cfg, 1)

	vid := mkTransport(t, s, "o1") // JackLoad@A, JackUnload@B
	runTicks(d, m, 20)

	if got := s.GetOrder(vid).State; got != "FINISHED" {
		t.Fatalf("want FINISHED, got %s", got)
	}
	if len(em.blocks) != 1 {
		t.Fatalf("want 1 block-completed (the intermediate pickup), got %d: %+v", len(em.blocks), em.blocks)
	}
	if em.blocks[0].binTask != "JackLoad" || em.blocks[0].location != "A" {
		t.Fatalf("intermediate block wrong: %+v", em.blocks[0])
	}
	// Status sequence should reach RUNNING then FINISHED.
	if !contains(em.status, vid+":RUNNING") || !contains(em.status, vid+":FINISHED") {
		t.Fatalf("status sequence missing RUNNING/FINISHED: %v", em.status)
	}
}

// T2.3 / Gate 2: fail_rate=1.0 always faults — the order never finishes.
func TestDriverFailRateOneAlwaysFails(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 1.0}
	d, s, m, _ := newTestDriver(t, cfg, 7)

	vid := mkTransport(t, s, "o1")
	runTicks(d, m, 20)

	if got := s.GetOrder(vid).State; got != "FAILED" {
		t.Fatalf("want FAILED, got %s", got)
	}
}

// T2.3 / Gate 2: a staged order does not advance past its released blocks until
// ReleaseOrder marks it complete.
func TestDriverStagedOrderWaitsForRelease(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}
	d, s, m, em := newTestDriver(t, cfg, 3)

	res, err := s.CreateOrder(fleet.CreateOrderRequest{
		ExternalID: "staged",
		Blocks:     []fleet.OrderBlock{{BlockID: "b0", Location: "P", BinTask: "JackLoad"}},
		Complete:   false,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	vid := res.VendorOrderID

	runTicks(d, m, 20)
	if got := s.GetOrder(vid).State; got == "FINISHED" {
		t.Fatalf("staged order must not finish before release; state=%s", got)
	}
	if len(em.blocks) != 1 || em.blocks[0].binTask != "JackLoad" {
		t.Fatalf("expected the one released block to complete, got %+v", em.blocks)
	}

	// Release the final leg.
	if err := s.ReleaseOrder(vid, []fleet.OrderBlock{{BlockID: "b1", Location: "Q", BinTask: "JackUnload"}}, true); err != nil {
		t.Fatalf("ReleaseOrder: %v", err)
	}
	runTicks(d, m, 20)
	if got := s.GetOrder(vid).State; got != "FINISHED" {
		t.Fatalf("want FINISHED after release, got %s", got)
	}
}

// A release for an order that already reached a terminal state is idempotent —
// ReleaseOrder returns nil, not a hard error. This stops a late/duplicate release
// (e.g. Core's complex auto-release racing a downtime FAILED) from cascading a
// spurious fleet_failed that fails the order twice on the Edge — the
// "simulator: order ... not found for release" noise.
//
// SCOPED TO SETTLED ORDERS, which is what the name says. A release for an ID this
// backend NEVER issued is the opposite case and is deliberately an ERROR — the
// tombstone set exists precisely so a map miss can tell "settled and reaped (moot)"
// from "never issued (a lie somewhere upstream)"; see SimulatorBackend.settled and
// ReleaseOrder. That case is owned by TestReleaseOrderNeverIssuedIsAnError in
// release_honesty_test.go, and this test asserted the reverse of it until 2026-08-24:
// both live in this package, so the pair contradicted each other and only the
// sim-tagged one ran — in neither gate.
func TestReleaseOrderIdempotentForSettledOrder(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}
	_, s, _, _ := newTestDriver(t, cfg, 7)

	// Settled (terminal) order still in the map → no-op. Create, cancel (→ STOPPED),
	// then release.
	res, err := s.CreateOrder(fleet.CreateOrderRequest{
		ExternalID: "settled", Blocks: []fleet.OrderBlock{{BlockID: "b0", Location: "P", BinTask: "JackLoad"}},
		Complete: false,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := s.CancelOrder(res.VendorOrderID); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if err := s.ReleaseOrder(res.VendorOrderID, []fleet.OrderBlock{{BlockID: "b1", Location: "Q", BinTask: "JackUnload"}}, true); err != nil {
		t.Errorf("release of a settled (STOPPED) order should be a no-op, got %v", err)
	}
}

// T2.3 / Gate 2: two runs with the same seed and config produce an identical
// transition sequence — the determinism the future DST suite relies on.
func TestDriverDeterministicWithSeed(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0.2, FailRate: 0.15}
	run := func() []string {
		d, s, m, em := newTestDriver(t, cfg, 99)
		for i := 0; i < 6; i++ {
			mkTransport(t, s, fmt.Sprintf("o%d", i))
		}
		runTicks(d, m, 120)
		return em.status
	}
	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("expected some transitions")
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic transition sequence:\n a=%v\n b=%v", a, b)
	}
	// And with fail_rate>0 over six orders, expect a mix (at least one of each).
	if !contains(a, "sim-1:FINISHED") && !contains(a, "sim-2:FINISHED") {
		t.Logf("no FINISHED in %v (unusual but not necessarily wrong)", a)
	}
}

// T2.3: eviction runs on the tick — a finished order is reaped after the
// retention window without leaking progress bookkeeping.
func TestDriverEvictsFinishedOrders(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}
	d, s, m, _ := newTestDriver(t, cfg, 5)

	vid := mkTransport(t, s, "o1")
	runTicks(d, m, 20) // drive to FINISHED (terminalAt stamped)
	if s.GetOrder(vid).State != "FINISHED" {
		t.Fatalf("setup: order should be FINISHED")
	}
	// Advance past the retention window; the next tick's sweep reaps it.
	runTicks(d, m, int(defaultRetention/time.Second)+2)
	if s.HasOrder(vid) {
		t.Fatalf("order should have been evicted after retention")
	}
	if _, leaked := d.progress[vid]; leaked {
		t.Fatalf("driver leaked progress bookkeeping for evicted order")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ── Fix A (2026-09-07): a deferred transition is held, not dropped ──────────
//
// The simulator used to commit order.state before the resolver ran and drop
// the emission on a miss — the one drop path that left no trace. Now the
// state does not advance and the driver retries, mirroring the real RDS
// poller's retry-on-resolver-miss. These tests pin the driver side.

// flakyResolver misses until flipped, then maps "sim-N" → N like seqResolver.
type flakyResolver struct {
	seq    seqResolver
	resist bool
}

func (r *flakyResolver) ResolveVendorOrderID(vid string) (int64, error) {
	if r.resist {
		return 0, errors.New("UpdateOrderVendor has not landed yet")
	}
	return r.seq.ResolveVendorOrderID(vid)
}

// The headline scenario: the first RUNNING defers once (the CreateOrder→
// UpdateOrderVendor race), the driver must not advance its phase or latch
// anything, and on a later tick — once the resolver resolves — the order must
// still reach in_transit and finish normally. Before Fix A this deferral
// didn't exist: the state committed anyway and the emission was dropped,
// which stranded the order at acknowledged on Edge for the life of the run
// (§R.98 / the acceptance Families post-mortem).
func TestDriverDeferredRunningRetriesToInTransit(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}
	m := clock.NewManual(driverStart)
	em := &captureEmitter{}
	s := New(WithClock(m))
	res := &flakyResolver{resist: true}
	s.InitTracker(em, res)
	d := NewDriver(s, cfg, m, rand.New(rand.NewSource(3)))

	vid := mkTransport(t, s, "o1")
	// First tick (t=1): the driver schedules the advance (deadline =
	// createdFraction × transit = 1.5 s). The t=2 tick is still before the
	// deadline; the t=3 tick fires the first RUNNING attempt, whose report
	// defers while resist holds.
	runTicks(d, m, 3)
	if got := s.GetOrder(vid).State; got != "CREATED" {
		t.Fatalf("a deferred first RUNNING must leave the order CREATED; got %q", got)
	}
	if len(em.status) != 0 {
		t.Fatalf("nothing may emit on a deferral; emitted %v", em.status)
	}
	if p := d.progress[vid]; p.phase != phaseCreated {
		t.Fatalf("the driver must stay in phaseCreated across a deferral; phase=%v", p.phase)
	}

	// The resolver resolves; the next tick's retry carries it the rest of the
	// way — including to in_transit on Core (the waybill went out with the
	// robot id).
	res.resist = false
	runTicks(d, m, 1)
	if got := s.GetOrder(vid).State; got != "RUNNING" {
		t.Fatalf("the retry must drive RUNNING; got %q", got)
	}
	if len(em.assigned) != 1 || em.assigned[0] != vid+":AMR-01" {
		t.Fatalf("the waybill must carry the robot on the retried RUNNING; got %v", em.assigned)
	}
	if got := s.MapState(s.GetOrder(vid).State); got != "in_transit" {
		t.Fatalf("RUNNING maps to in_transit; got %q", got)
	}
}

// The WAITING arm must not latch p.staged on a deferral (the latch was what
// made the old drop permanent), and the retry must drive WAITING cleanly —
// status reaches "staged" exactly once the report lands.
func TestDriverDeferredWaitingDoesNotLatchStaged(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}
	m := clock.NewManual(driverStart)
	em := &captureEmitter{}
	s := New(WithClock(m))
	res := &flakyResolver{resist: true}
	s.InitTracker(em, res)
	d := NewDriver(s, cfg, m, rand.New(rand.NewSource(11)))

	// A staged-shape order: one pickup block, Complete=false — the driver
	// drains it and dwells at the wait point, where it drives WAITING.
	created, err := s.CreateOrder(fleet.CreateOrderRequest{
		ExternalID: "waiter",
		Blocks:     []fleet.OrderBlock{{BlockID: "b0", Location: "P", BinTask: "JackLoad"}},
		Complete:   false,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	vid := created.VendorOrderID

	// Setup with resist=true: the first RUNNING defers. Once resolved, the
	// block completes (~5 s) — and the dwell deadline is one tick after the
	// block completes, so the WAITING attempt can fire inside a coarse
	// runTicks window. Walk forward one tick at a time and stop the moment
	// the order dwells RUNNING with nothing to drive, BEFORE the WAITING
	// deadline fires.
	res.resist = false
	// The block completes on some tick inside the long window; WAITING could
	// be driven the very next tick (dwell deadline = completion tick + 1 s),
	// so stop the instant the block has completed and leave zero slack for a
	// WAITING attempt — which would have landed (resist was off) and ruined
	// the scenario. The completion is visible as an EmitBlockCompleted.
	for i := 0; i < 20; i++ {
		runTicks(d, m, 1)
		if len(em.blocks) == 1 {
			break
		}
	}
	if len(em.blocks) != 1 {
		t.Fatalf("setup: the pickup block never completed; blocks=%v", em.blocks)
	}
	if got := s.GetOrder(vid).State; got != "RUNNING" {
		t.Fatalf("setup: order should dwell RUNNING after its released block, got %q", got)
	}

	// The resist window must COVER the dwell deadline: completing a block
	// re-arms the deadline at now+transit (5 s), so the WAITING attempt fires
	// 5 ticks after the completion, not 1. Resist stays on through it. The
	// WAITING attempt defers; p.staged must NOT latch — a latched staged with
	// no WAITING committed is the frozen-in_transit shape this test exists to
	// kill.
	res.resist = true
	runTicks(d, m, 5)
	if got := s.GetOrder(vid).State; got != "RUNNING" {
		t.Fatalf("a deferred WAITING must not advance the state; got %q", got)
	}
	if p := d.progress[vid]; p.staged {
		t.Fatalf("a deferred WAITING must not latch p.staged")
	}
	if contains(em.status, vid+":WAITING") {
		t.Fatalf("no WAITING may emit on the deferral; emitted %v", em.status)
	}

	// Resolve: the retried WAITING lands (the deferral re-armed the deadline
	// at +1 s) and the staged latch lands with it.
	res.resist = false
	runTicks(d, m, 1)
	if got := s.GetOrder(vid).State; got != "WAITING" {
		t.Fatalf("the retried WAITING must land; got %q", got)
	}
	if p := d.progress[vid]; !p.staged {
		t.Fatalf("the landed WAITING must latch p.staged")
	}
}
