// operator_release_hop_guard_test.go — regression tests for the Hopkinsville
// press-index index-leg hang (2026-07-23). The consolidated two-robot RELEASE
// must gate each leg on Core's own release precondition (hop A4-i) and re-fire
// a deferred leg when it later reaches staged, its sibling having already gone
// (hop A4-ii).
package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestReleaseStagedOrders_HeldSupplyLegNotReleasedNorDesynced is the hop A4-i
// regression: the operator's consolidated two-robot RELEASE gates each leg on
// orders.ReleasableAtCore. The staged evac releases; a supply leg still at
// sourcing is SKIPPED, not force-flipped to in_transit. Before the fix the
// fan-out used releaseUnlessTerminal (IsTerminal only), which moved the Edge
// row to in_transit and then rolled it back on Core's invalid_state — the
// persistent divergence that hid the RELEASE button on the press-index hang.
func TestReleaseStagedOrders_HeldSupplyLegNotReleasedNorDesynced(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "HOP-A4I", PayloadCode: "PART-A4I", UOPCapacity: 1200, InitialUOP: 800,
	})
	// seedTwoRobotPair: orderA -> ActiveOrderID (supply), orderB -> StagedOrderID
	// (evac). ResolveSwapPair maps those slots to supply/evac respectively.
	supplyID, evacID := seedTwoRobotPair(t, db, nodeID, "uuid-a4i", "two_robot")

	// Supply leg is NOT yet releasable — held at sourcing, exactly the state the
	// pre-hop fan-out optimistically force-flipped to in_transit.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusSourcing)), "hold supply at sourcing")

	// Drain seed/setup envelopes so the release-envelope count is exact.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}), "ReleaseStagedOrders with a held supply leg")

	// Exactly one release envelope: the staged evac. The held supply is skipped,
	// so Core is never handed a release it would refuse.
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes = %d, want 1 (evac only; a held supply must not queue one)", len(releases))
	}

	// The desync assertion: the supply leg must stay exactly where it was.
	supply, err := db.GetOrder(supplyID)
	if err != nil {
		t.Fatalf("re-read supply: %v", err)
	}
	if supply.Status != protocol.StatusSourcing {
		t.Errorf("supply status = %q, want %q — a skipped release must not move the Edge row",
			supply.Status, protocol.StatusSourcing)
	}

	// The evac actually released (advanced past staged).
	evac, err := db.GetOrder(evacID)
	if err != nil {
		t.Fatalf("re-read evac: %v", err)
	}
	if evac.Status == protocol.StatusStaged {
		t.Errorf("evac status = %q, want past staged — the staged evac must still release", evac.Status)
	}
}

// TestReleaseStagedOrders_RefiresDeferredSiblingOnStaged is the hop A4-ii
// regression: a leg deferred by the consolidated RELEASE (Core would have
// refused it) fires the release the operator already intended the moment it
// later reaches staged — its sibling having already gone.
func TestReleaseStagedOrders_RefiresDeferredSiblingOnStaged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "HOP-A4II", PayloadCode: "PART-A4II", UOPCapacity: 1200, InitialUOP: 800,
	})
	supplyID, _ := seedTwoRobotPair(t, db, nodeID, "uuid-a4ii", "two_robot")

	// Supply held pre-staged; evac at staged. The consolidated release fans out:
	// the evac releases, the supply is deferred (Core would refuse it now).
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusSourcing)), "hold supply at sourcing")

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}), "ReleaseStagedOrders")

	// Precondition: the supply really was deferred, not released.
	supply, _ := db.GetOrder(supplyID)
	if supply.Status != protocol.StatusSourcing {
		t.Fatalf("precondition: supply status = %q, want sourcing (must have been deferred)", supply.Status)
	}

	// Drain the evac's release envelope so the re-fire's is the only one we count.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	// Supply now reaches staged. In production Core's order.staged push drives
	// this transition and the engine bridges it onto the event bus; here we set
	// the row and fire the same EventOrderStatusChanged the lifecycle emits.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusStaged)), "supply reaches staged")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: supplyID, NewStatus: string(protocol.StatusStaged),
	}})

	// The deferred release fired: supply advanced past staged, one OrderRelease
	// envelope queued.
	supply, _ = db.GetOrder(supplyID)
	if supply.Status == protocol.StatusStaged {
		t.Errorf("supply status = %q, want past staged — the deferred release must re-fire when the leg reaches staged", supply.Status)
	}
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 1 {
		t.Errorf("OrderRelease envelopes after re-fire = %d, want 1 (the deferred supply)", len(releases))
	}
}

// TestSiblingReleaseRefire_NoRefireWithoutPriorRelease is the safety property:
// a leg reaching staged must NOT auto-release unless its sibling already
// released on an operator click. Without a recorded deferral there is no
// operator intent, so the handler must do nothing — never auto-release, never
// cancel, never re-plan.
func TestSiblingReleaseRefire_NoRefireWithoutPriorRelease(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "HOP-A4II-NEG", PayloadCode: "PART-A4IIN", UOPCapacity: 1200, InitialUOP: 800,
	})
	supplyID, _ := seedTwoRobotPair(t, db, nodeID, "uuid-a4ii-neg", "two_robot")

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// No consolidated RELEASE was ever clicked, so nothing is recorded as a
	// deferred sibling. Drain setup envelopes.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	// The supply leg reaches staged on its own.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusStaged)), "supply reaches staged")
	eng.Events.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: supplyID, NewStatus: string(protocol.StatusStaged),
	}})

	// Nothing must have been released — the operator never clicked.
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Errorf("OrderRelease envelopes = %d, want 0 — a leg with no recorded operator release must not auto-fire", len(releases))
	}
	supply, _ := db.GetOrder(supplyID)
	if supply.Status != protocol.StatusStaged {
		t.Errorf("supply status = %q, want staged unchanged (no auto-release)", supply.Status)
	}
}
