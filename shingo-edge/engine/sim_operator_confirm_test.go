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
// exactly the legs that need signing:
//
//   - press-index R1 serves the press by CLEARING it. It leaves no bin there, and
//     its delivery_node names the index node it stages at.
//   - single-robot A swaps the press bin out and a fresh one in, but ENDS at the
//     outbound destination, so its delivery_node names the supermarket.
//
// Neither auto-confirms. Both would hang. "Does this leg touch this node?" is the
// question — weaker than "does it leave a bin here" on purpose, because clearing
// the press is serving it.
func TestSimOperator_LegServesNode_ReadsStepsNotDeliveryNode(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	op := &simOperator{e: eng}

	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	press := node.CoreNodeName

	disp, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build press-index dispatch")

	// R1: not auto-confirmed, and its delivery_node is the INDEX node — the very
	// value the old guard compared against the press and rejected.
	r1 := mkSwapLeg(t, db, nodeID, "sim-r1", disp.StepsA, disp.DeliveryNodeA)
	if disp.AutoConfirmA {
		t.Fatal("press-index R1 is expected to need an operator receipt (AutoConfirmA=false)")
	}
	if r1.DeliveryNode == press {
		t.Fatal("fixture is not exercising the bug: R1's delivery_node must NOT be the press")
	}
	if !op.legServesNode(r1, press) {
		t.Error("press-index R1 must be signed for — it serves the press by clearing it, and nothing else will confirm it")
	}

	// R2 auto-confirms itself on FINISHED, so the sim must not race it. runConfirm
	// rejects it on order.AutoConfirm before legServesNode is reached; legServesNode
	// still recognises it as serving the press, which is what makes that skip a
	// deliberate one rather than an accident of geometry.
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
