package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// release_guard_test.go — sim gate V6 ("release-while-held: released: 0,
// pending: 1, no desync") at the unit level.
//
// The defect: Manager.ReleaseOrderWithDisposition guards only terminal +
// pending/submitted, then force-transitions the Edge row to in_transit. Core
// refuses a release for anything that is not staged or in_transit
// ("invalid_state", shingo-core/dispatch/complex_release.go). So releasing a
// leg that is still queued/sourcing/dispatched/acknowledged queued an envelope
// Core would refuse AND moved the Edge row anyway — a persistent Edge/Core
// status divergence plus a "released" count that never happened.
//
// Reachable today via press-index self-sufficient shapes; it becomes the
// NORMAL path once pool-sourced supply legs can sit in a wait state, which is
// why the guard ships ahead of that work.

// TestReleaseChangeoverWait_HeldLegIsNotReleasedAndCountsPending is the V6
// core: a leg that Core would refuse must not be released, must be counted
// Pending (so the operator knows to come back), and must leave BOTH sides'
// status untouched.
func TestReleaseChangeoverWait_HeldLegIsNotReleasedAndCountsPending(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "release guard")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected an evac order on the task")
	}
	evacID := *task.OldMaterialReleaseOrderID

	// Hold the evac at sourcing — Core has it but it has not reached a wait.
	// This is the exact state the old code released into.
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusSourcing)), "hold evac at sourcing")

	// Drain the changeover-start envelopes so the assertion below is exact.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	result, err := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "test-operator"})
	if err != nil {
		t.Fatalf("ReleaseChangeoverWait: %v", err)
	}

	if result.Released != 0 {
		t.Errorf("Released = %d, want 0 — a leg Core would refuse must not count as released", result.Released)
	}
	if result.Pending == 0 {
		t.Error("Pending = 0, want >=1 — the held leg must be surfaced so the operator clicks again")
	}

	// No envelope may be queued: Core would answer invalid_state.
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Errorf("OrderRelease envelopes queued: got %d, want 0 (Core would refuse every one)", len(releases))
	}

	// THE DESYNC ASSERTION. The old path transitioned the Edge row to
	// in_transit while Core still held it at sourcing, and no later
	// re-release could heal it.
	got, err := db.GetOrder(evacID)
	if err != nil {
		t.Fatalf("re-read evac: %v", err)
	}
	if got.Status != orders.StatusSourcing {
		t.Errorf("evac status = %q, want %q — the Edge row must not move when the release was never sent",
			got.Status, orders.StatusSourcing)
	}
}

// TestReleaseChangeoverWait_StagedLegStillReleases is the over-blocking guard:
// the fix must not make the normal release a no-op. Companion to the test
// above — same scenario, staged instead of sourcing.
func TestReleaseChangeoverWait_StagedLegStillReleases(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "release guard happy path")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected an evac order on the task")
	}
	evacID := *task.OldMaterialReleaseOrderID
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusStaged)), "force evac staged")

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	result, err := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "test-operator"})
	if err != nil {
		t.Fatalf("ReleaseChangeoverWait: %v", err)
	}
	if result.Released != 1 {
		t.Errorf("Released = %d, want 1 — a staged leg must still release", result.Released)
	}
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes queued: got %d, want 1", len(releases))
	}
}

// TestReleaseIfReleasable_SkipsHeldOrder covers the DEFERRED path directly —
// the one HandleBinPickedUp drives on evac-pickup confirm, where nothing
// upstream guarantees the supply leg has staged.
func TestReleaseIfReleasable_SkipsHeldOrder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "deferred release guard")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected an evac order on the task")
	}
	orderID := *task.OldMaterialReleaseOrderID

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	for _, held := range []protocol.Status{
		orders.StatusQueued, orders.StatusSourcing,
		orders.StatusDispatched, orders.StatusAcknowledged,
	} {
		testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(held)), "set held status")

		released, err := eng.releaseIfReleasable(orderID, "test-deferred-supply", ReleaseDisposition{CalledBy: "test"})
		if err != nil {
			t.Fatalf("releaseIfReleasable(%s): unexpected error: %v", held, err)
		}
		if released {
			t.Errorf("releaseIfReleasable(%s) = true, want false — Core would refuse this status", held)
		}
		if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
			t.Errorf("status %s: OrderRelease envelopes queued: got %d, want 0", held, len(releases))
		}
		got, _ := db.GetOrder(orderID)
		if got.Status != held {
			t.Errorf("status %s: order moved to %q — a skipped release must not transition the row", held, got.Status)
		}
	}

	// And the positive control: staged releases and reports true.
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusStaged)), "stage it")
	released, err := eng.releaseIfReleasable(orderID, "test-deferred-supply", ReleaseDisposition{CalledBy: "test"})
	if err != nil {
		t.Fatalf("releaseIfReleasable(staged): %v", err)
	}
	if !released {
		t.Error("releaseIfReleasable(staged) = false, want true")
	}
}
