//go:build sim

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	edgeorders "shingoedge/orders"
)

// TestSimOperator_LegServesNode_ReadsStepsNotDeliveryNode pins the confirm scope.
//
// The sim is the operator: a leg that is not auto-confirmed sits `delivered`
// until something signs for it, and while it does, CanAcceptOrders reports
// "active/staged order in progress" and blocks the next relief until the cell
// overfills (PLN_003).
//
// The old guard asked `order.DeliveryNode == node.CoreNodeName`, which skipped
// exactly the legs whose work at a node its delivery_node does not name:
//
//   - press-index R1 serves the press by CLEARING it. It leaves no bin there, and
//     its delivery_node names the index node it stages at.
//   - single-robot A swaps the press bin out and a fresh one in, but ENDS at the
//     outbound destination, so its delivery_node names the supermarket.
//
// "Does this leg touch this node?" is the question — weaker than "does it leave a
// bin here" on purpose, because clearing the press is serving it.
//
// WHICH OF THEM ACTUALLY NEEDS A SIGNATURE is a separate question, and this test
// used to conflate the two: it asserted AutoConfirmA == false for press-index R1
// on the reasoning that "neither auto-confirms, both would hang". Since the
// derived confirm policy (2026-09-03) a leg auto-confirms iff it leaves no bin on
// claim.CoreNodeName — so unflipped R1, which is the pure evac, auto-confirms and
// closes itself. Scope and receipt are decided by different predicates
// (legTouchesNode here, legPlacesBinAt there) and the fixture guard below now
// says so instead of pinning the old coincidence.
func TestSimOperator_LegServesNode_ReadsStepsNotDeliveryNode(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	op := &simOperator{e: eng}

	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	press := node.CoreNodeName

	disp, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build press-index dispatch")

	// R1: its delivery_node is the INDEX node — the very value the old guard
	// compared against the press and rejected.
	r1 := mkSwapLeg(t, db, nodeID, "sim-r1", disp.StepsA, disp.DeliveryNodeA)
	if r1.DeliveryNode == press {
		t.Fatal("fixture is not exercising the bug: R1's delivery_node must NOT be the press")
	}
	if legPlacesBinAt(disp.StepsA, press) || !disp.AutoConfirmA {
		t.Fatal("fixture drift: unflipped press-index R1 is the pure evac — it leaves no bin on the " +
			"press, so the derived confirm policy auto-confirms it. If that changed, the receipt " +
			"question moved and this case's subject (SCOPE, not receipt) needs re-stating")
	}
	if !op.legServesNode(r1, press) {
		t.Error("press-index R1 is not in the press operator's scope — it works the press by clearing it, " +
			"and a scope read off delivery_node would hand it to the index node instead")
	}

	// R2 is the leg that PLACES the fresh carrier on the press, so since the
	// derived confirm policy (2026-09-03) it is the leg the operator signs for
	// — it no longer auto-confirms. legServesNode must recognise it, and for
	// the plainest possible reason: the bin it left is sitting on the press.
	r2 := mkSwapLeg(t, db, nodeID, "sim-r2", disp.StepsB, "")
	if !op.legServesNode(r2, press) {
		t.Error("press-index R2 places the bin ON the press — it plainly serves it")
	}

	// A leg for a different node is not this operator's to sign.
	other := mkSwapLeg(t, db, nodeID, "sim-other", []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "SOMEWHERE"},
		{Action: protocol.ActionDropoff, Node: "ELSEWHERE"},
	}, press) // delivery_node LIES and names the press; the steps never go near it
	if op.legServesNode(other, press) {
		t.Error("a leg that never touches the press was claimed by the press's operator — on the strength of delivery_node alone")
	}
}

// A simple (non-complex) order carries no steps; its delivery_node is exactly
// where its one bin goes, and that stays the test for it — the same split the
// delivered gate makes in wiring_delivered.go.
func TestSimOperator_LegServesNode_SimpleOrderUsesDeliveryNode(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	op := &simOperator{e: eng}

	nodeID, node, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobot, "")
	press := node.CoreNodeName

	id, err := db.CreateOrder("sim-simple", edgeorders.TypeRetrieve, &nodeID, false, 1, press, "", "", "", false, "WIDGET-A")
	testutil.MustNoErr(t, err, "create retrieve order")
	simple, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "get retrieve order")

	if !op.legServesNode(simple, press) {
		t.Error("a simple retrieve delivering to this node must still be signed for")
	}
	if op.legServesNode(simple, "OTHER-NODE") {
		t.Error("a simple retrieve delivering elsewhere is not this node's to sign")
	}
}
