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
