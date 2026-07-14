// operator_release_sibling_test.go — regression tests for the durable
// sibling-pointer pair-tracking that replaced the volatile runtime
// two-slot model on 2026-05-04.
//
// Background incident (ALN_002 plant-a 2026-05-04):
//
//	A two-robot swap entered a stuck state where runtime.ActiveOrderID
//	had been nulled by handler_bin_picked_up (supply bin physically left
//	the supermarket) while runtime.StagedOrderID still pointed at the
//	pending evac order. Two downstream gates depended on both runtime
//	slots being non-nil:
//
//	  1. ReleaseStagedOrders refused the operator's release click
//	     ("expected two tracked orders for two-robot release"). The
//	     operator had to release manually via edge.
//	  2. isSupplyOrderInActiveTwoRobotSwap returned false for the supply
//	     order, so manifest sync was NOT suppressed when Order A was
//	     eventually released — Core wiped the supply bin's manifest.
//	     The bin physically arrived at the slot empty; PLC consume
//	     ticks dragged it to uop_remaining=-20 on Core.
//
// The fix introduces orders.sibling_order_id — a durable pointer set
// at order-creation time that survives bin-pickup, status transitions,
// and process restarts. Both gates now consult sibling + delivery_node
// + claim.SwapMode instead of volatile runtime slots. This file pins
// each invariant with a focused test; if a future refactor reintroduces
// the volatile-slot dependency, one of these fails before it ships.
package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedTwoRobotPair stages a realistic two-robot swap pair on a consume
// node: Order A (supply, delivery_node=slot), Order B (evac,
// delivery_node!=slot), runtime tracking populated, sibling pointer
// linked. Returns (orderA, orderB).
func seedTwoRobotPair(t *testing.T, db *store.DB, nodeID int64, prefix string, swapMode protocol.SwapMode) (int64, int64) {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get node %d: %v", nodeID, err)
	}
	process, err := db.GetProcess(node.ProcessID)
	if err != nil {
		t.Fatalf("get process %d: %v", node.ProcessID, err)
	}
	if process.ActiveStyleID == nil {
		t.Fatalf("process has no active style")
	}
	claim, err := db.GetStyleNodeClaimByNode(*process.ActiveStyleID, node.CoreNodeName)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	in := processes.NodeClaimInput{
		StyleID:        claim.StyleID,
		CoreNodeName:   claim.CoreNodeName,
		Role:           claim.Role,
		SwapMode:       swapMode,
		PayloadCode:    claim.PayloadCode,
		UOPCapacity:    claim.UOPCapacity,
		InboundSource:  "TR-SOURCE",
		InboundStaging: "TR-STAGING",
	}
	if swapMode == "two_robot_press_index" {
		in.PairedCoreNode = node.CoreNodeName + "-PAIR"
		in.OutboundDestination = "TR-OUTBOUND"
	}
	if _, err := db.UpsertStyleNodeClaim(in); err != nil {
		t.Fatalf("promote claim to %s: %v", swapMode, err)
	}

	// A DELIVERS to the node (supply); B carries the spent bin away (evac).
	// isSupplyOrderInTwoRobotSwap reads the STEPS to tell them apart — it used to
	// read delivery_node, which is wrong in both directions for press-index (the evac
	// leg stored the process node, and the real supply leg is auto-confirmed so its
	// delivery_node is blanked).
	orderA := stageOrderForConsumeNode(t, db, nodeID, prefix+"-A")
	orderB := stageOrderForConsumeNode(t, db, nodeID, prefix+"-B")
	redirectLegAway(t, db, orderB, node.CoreNodeName, "TR-EVAC-DEST")

	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB), "track A+B on runtime")
	testutil.MustNoErr(t, db.LinkOrderSiblings(orderA, orderB), "link siblings")
	return orderA, orderB
}

// TestRegression_SiblingLinkBidirectional pins LinkOrderSiblings's
// contract: writing the link sets both halves' SiblingOrderID, so a
// sibling lookup from either order finds the other.
func TestRegression_SiblingLinkBidirectional(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SIB-LINK", PayloadCode: "PART-LINK", UOPCapacity: 1200, InitialUOP: 800,
	})
	orderA, orderB := seedTwoRobotPair(t, db, nodeID, "uuid-link", "two_robot")

	a, _ := db.GetOrder(orderA)
	b, _ := db.GetOrder(orderB)
	if a.SiblingOrderID == nil || *a.SiblingOrderID != orderB {
		t.Errorf("A.SiblingOrderID = %v, want &%d", a.SiblingOrderID, orderB)
	}
	if b.SiblingOrderID == nil || *b.SiblingOrderID != orderA {
		t.Errorf("B.SiblingOrderID = %v, want &%d", b.SiblingOrderID, orderA)
	}
}

// TestRegression_SupplyGuardFiresWhenActiveOrderCleared is the core
// regression for the ALN_002 manifest-wipe bug. Simulates the bin-pickup
// handler nulling runtime.ActiveOrderID before release fires, then
// releases the supply order. With the volatile-slot guard, this would
// have failed → manifest sync sent → Core wipes supply bin. With the
// durable sibling-pointer guard, manifest sync stays suppressed.
func TestRegression_SupplyGuardFiresWhenActiveOrderCleared(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SIB-CLEARED", PayloadCode: "PART-CLR", UOPCapacity: 1200, InitialUOP: 800,
	})
	orderA, orderB := seedTwoRobotPair(t, db, nodeID, "uuid-cleared", "two_robot")

	// Simulate handler_bin_picked_up: supply bin left the supermarket,
	// ActiveOrderID nulled. StagedOrderID stays.
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderB), "simulate bin-pickup null")

	// Drain outbox so findOutboxByType is exact.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	eng := testEngine(t, db)
	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderA, disp), "release supply order A")

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes: got %d, want 1", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])
	if rel.RemainingUOP != nil {
		t.Errorf("supply-order wire RemainingUOP = %d, want nil — supply guard must fire even when runtime.ActiveOrderID has been cleared (sibling pointer is the durable signal)",
			*rel.RemainingUOP)
	}
}

// TestRegression_ReleaseStagedOrdersWorksAfterActiveOrderCleared pins
// the gate fix: if runtime.ActiveOrderID has been nulled, the gate
// follows the sibling pointer on Order B to find Order A and proceeds.
// Pre-fix this returned "expected two tracked orders" and refused.
func TestRegression_ReleaseStagedOrdersWorksAfterActiveOrderCleared(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SIB-GATE", PayloadCode: "PART-GATE", UOPCapacity: 1200, InitialUOP: 800,
	})
	orderA, orderB := seedTwoRobotPair(t, db, nodeID, "uuid-gate", "two_robot")

	// Same simulated bin-pickup null.
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderB), "simulate bin-pickup null")

	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}), "ReleaseStagedOrders after ActiveOrderID cleared")

	// Both orders should have advanced past staged.
	a, _ := db.GetOrder(orderA)
	b, _ := db.GetOrder(orderB)
	if a.Status == protocol.StatusStaged {
		t.Errorf("Order A still at %q, want past staged (sibling-walk should have released A)", a.Status)
	}
	if b.Status == protocol.StatusStaged {
		t.Errorf("Order B still at %q, want past staged", b.Status)
	}
}

// TestRegression_ReleaseStagedOrdersWorksAfterStagedOrderCleared is the
// mirror case: StagedOrderID nulled but ActiveOrderID intact. The gate
// follows Order A's sibling pointer to find Order B.
func TestRegression_ReleaseStagedOrdersWorksAfterStagedOrderCleared(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SIB-GATE-EVAC", PayloadCode: "PART-GE", UOPCapacity: 1200, InitialUOP: 800,
	})
	orderA, orderB := seedTwoRobotPair(t, db, nodeID, "uuid-gate-evac", "two_robot")

	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, nil), "null staged_order_id")

	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-op"}), "ReleaseStagedOrders after StagedOrderID cleared")

	a, _ := db.GetOrder(orderA)
	b, _ := db.GetOrder(orderB)
	if a.Status == protocol.StatusStaged {
		t.Errorf("Order A still at %q, want past staged", a.Status)
	}
	if b.Status == protocol.StatusStaged {
		t.Errorf("Order B still at %q, want past staged (sibling-walk should have released B)", b.Status)
	}
}

// TestRegression_SupplyGuardSkipsForOrderWithoutSibling pins the
// negative case: an order with no SiblingOrderID is never identified
// as a supply leg, regardless of swap_mode or delivery_node. Defends
// against a future refactor that accidentally treats unlinked orders
// as part of a swap pair.
func TestRegression_SupplyGuardSkipsForOrderWithoutSibling(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SIB-NOLINK", PayloadCode: "PART-NL", UOPCapacity: 1200, InitialUOP: 800,
	})
	// Promote claim to two_robot to exercise the swap-mode predicate.
	in := processes.NodeClaimInput{
		StyleID:        claimToStyleID(t, db, claimID),
		CoreNodeName:   "SIB-NOLINK-NODE",
		Role:           "consume",
		SwapMode:       "two_robot",
		PayloadCode:    "PART-NL",
		UOPCapacity:    1200,
		InboundSource:  "TR-SOURCE",
		InboundStaging: "TR-STAGING",
	}
	if _, err := db.UpsertStyleNodeClaim(in); err != nil {
		t.Fatalf("promote claim: %v", err)
	}

	// Single staged order, no sibling link.
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-nolink")
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: DispositionCaptureLineside}), "release")

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes: got %d, want 1", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])
	// No sibling → not a supply leg → manifest sync NOT suppressed →
	// RemainingUOP=&0 (capture_lineside default).
	if rel.RemainingUOP == nil || *rel.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %v, want &0 (no-sibling order is not a supply leg, capture_lineside ships &0)",
			rel.RemainingUOP)
	}
}

// TestRegression_SupplyGuardFiresForPressIndexMode pins coverage of the
// two_robot_press_index swap mode — also a paired-leg choreography that
// needs the same supply-bin manifest protection. Pre-fix the guard only
// matched literal "two_robot", leaving press_index supply bins exposed
// to the manifest-wipe class of bug.
func TestRegression_SupplyGuardFiresForPressIndexMode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SIB-PRESS", PayloadCode: "PART-PI", UOPCapacity: 1200, InitialUOP: 800,
	})
	orderA, _ := seedTwoRobotPair(t, db, nodeID, "uuid-press", "two_robot_press_index")

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	eng := testEngine(t, db)
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderA, ReleaseDisposition{Mode: DispositionCaptureLineside}), "release press_index supply")

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	rel := decodeOrderRelease(t, releases[0])
	if rel.RemainingUOP != nil {
		t.Errorf("press_index supply wire RemainingUOP = %d, want nil — supply guard must cover press_index, not just two_robot",
			*rel.RemainingUOP)
	}
}

// claimToStyleID looks up the style id for a claim. Internal scaffolding
// for the no-sibling test which doesn't go through seedTwoRobotPair.
func claimToStyleID(t *testing.T, db *store.DB, claimID int64) int64 {
	t.Helper()
	c, err := db.GetStyleNodeClaim(claimID)
	if err != nil {
		t.Fatalf("get claim %d: %v", claimID, err)
	}
	return c.StyleID
}
