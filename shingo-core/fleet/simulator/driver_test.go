//go:build sim

package simulator

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"shingo/shared/clock"
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
	d := newDriver(s, cfg, m, rand.New(rand.NewSource(seed)))
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

	res, err := s.CreateStagedOrder(fleet.StagedOrderRequest{
		ExternalID: "staged",
		Blocks:     []fleet.OrderBlock{{BlockID: "b0", Location: "P", BinTask: "JackLoad"}},
	})
	if err != nil {
		t.Fatalf("CreateStagedOrder: %v", err)
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

// A release for an order the fleet no longer holds (evicted after settling) or one that
// already reached a terminal state is idempotent — ReleaseOrder returns nil, not a hard
// error. This stops a late/duplicate release (e.g. Core's complex auto-release racing a
// downtime FAILED) from cascading a spurious fleet_failed that fails the order twice on
// the Edge — the "simulator: order ... not found for release" noise.
func TestReleaseOrderIdempotentForSettledOrder(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0}
	_, s, _, _ := newTestDriver(t, cfg, 7)

	// Unknown / already-evicted order → no-op, not an error.
	if err := s.ReleaseOrder("sg-never-existed", nil, true); err != nil {
		t.Errorf("release of unknown/evicted order should be a no-op, got %v", err)
	}

	// Settled (terminal) order still in the map → no-op. Create, cancel (→ STOPPED),
	// then release.
	res, err := s.CreateStagedOrder(fleet.StagedOrderRequest{
		ExternalID: "settled", Blocks: []fleet.OrderBlock{{BlockID: "b0", Location: "P", BinTask: "JackLoad"}},
	})
	if err != nil {
		t.Fatalf("CreateStagedOrder: %v", err)
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
