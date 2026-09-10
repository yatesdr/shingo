//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// THE PIN UNDER FACE 2'S NARROWING, which claimed a deadlock and named nothing.
//
// swap_hold.go's INDEX anti-collision arm is scoped to a SELF-SUFFICIENT evac
// sibling, and the scar says why: "a two_robot supply's sibling is a plain evac
// already held on the supply, and holding both would deadlock." That sentence
// was the only thing standing behind the narrowing — no test, no incident id.
// A scar without a pin is a hypothesis, and this one guards the difference
// between a working two_robot pair and a cell that stops until someone cancels
// a leg.
//
// You cannot pin a deadlock that the guard prevents, so this pins the two halves
// that make it true:
//
//  1. the evac IS held on the supply (Face 1 fires — ALN_003's guard);
//  2. the supply is NOT held (Face 2 steps aside), and the thing that makes it
//     step aside is legSecuresOwnReplacement(evac) == false;
//  3. flip only that input — give the sibling a self-sufficient shape — and
//     Face 2 DOES hold. So the narrowing is the only thing between here and
//     both legs held.
//
// And both-held is terminal, not slow: Face 1 releases the evac when the supply
// secures or commits, which needs the supply to dispatch; Face 2 releases the
// supply when the evac commits, which needs the evac to dispatch. Each release
// condition requires the other leg's dispatch. No event ends that.
func TestSwapFaces_NarrowingIsWhatPreventsTheMutualHold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	superNode := &nodes.Node{Name: "MUTUAL-SUPER", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(superNode), "create super node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// The supply: fetches from the supermarket, sets a bin ON the line.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "mutual-supply", PayloadCode: bp.Code, Quantity: 1, ProcessNode: lineNode.Name,
		SiblingOrderUUID: "mutual-evac",
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: superNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})
	// The line bin the evac would lift — present, so only a hold stops it.
	lineBin := &bins.Bin{BinTypeID: 1, Label: "MUTUAL-LINE-BIN", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "create line bin")
	testutil.MustNoErr(t, db.SetBinManifest(lineBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(lineBin.ID, ""), "confirm manifest")

	// The evac: a PLAIN one. Waits, lifts the line's bin, carries it away. One
	// pickup — it cannot fetch its own replacement.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "mutual-evac", PayloadCode: bp.Code, Quantity: 1, ProcessNode: lineNode.Name,
		SiblingOrderUUID: "mutual-supply",
		Steps: []protocol.ComplexOrderStep{
			{Action: "wait", Node: lineNode.Name},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: superNode.Name},
		},
	})

	supply, err := db.GetOrderByUUID("mutual-supply")
	testutil.MustNoErr(t, err, "get supply")
	evac, err := db.GetOrderByUUID("mutual-evac")
	testutil.MustNoErr(t, err, "get evac")
	supplySteps, ok := decodeSteps(supply.StepsJSON)
	if !ok {
		t.Fatal("supply has no readable steps — intake contract changed")
	}
	evacSteps, ok := decodeSteps(evac.StepsJSON)
	if !ok {
		t.Fatal("evac has no readable steps — intake contract changed")
	}

	// 1. Face 1 fires on the evac. This is ALN_003's guard doing its job.
	if held, _ := d.swapLegHeld(evac, evacSteps); !held {
		t.Fatal("the evac must be held while its supply holds no replacement (Face 1, ALN_003 " +
			"2026-06-03). Without this the rest of the test proves nothing")
	}

	// 2. Face 2 steps aside for the supply. If it did not, both legs of this pair
	//    would be held at once.
	if held, reason := d.swapLegHeld(supply, supplySteps); held {
		t.Errorf("BOTH LEGS OF A two_robot PAIR ARE NOW HELD (%s). Face 2's narrowing has stopped "+
			"working, and this is a mutual hold, not a slow one: Face 1 frees the evac when the "+
			"supply secures or commits — which needs the supply to dispatch — and Face 2 frees the "+
			"supply when the evac commits, which needs the evac to dispatch. Each release condition "+
			"requires the other leg's dispatch, so the cell stops until somebody cancels a leg", reason)
	}

	// 3. The narrowing's input, and the proof it is the load-bearing part: the
	//    evac is NOT self-sufficient, which is exactly why Face 2 let the supply
	//    through above.
	if legSecuresOwnReplacement(evacSteps) {
		t.Error("a plain two_robot evac read as self-sufficient — that is what makes Face 2 hold the " +
			"supply, and it is also what disarms Face 1 (see the pin in shingo-edge/engine, " +
			"TestTwoRobotEvacHasExactlyOnePickup_FaceOnePin)")
	}

	// Flip ONLY that input. A sibling that fetches its own replacement is the
	// press-index R1 shape, and Face 2 is supposed to hold against it.
	selfSufficient := []resolvedStep{
		{Action: protocol.ActionWait, Node: lineNode.Name},
		{Action: protocol.ActionPickup, Node: lineNode.Name},
		{Action: protocol.ActionDropoff, Node: superNode.Name},
		{Action: protocol.ActionPickup, Node: superNode.Name},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	if !legSecuresOwnReplacement(selfSufficient) {
		t.Fatal("the self-sufficient control shape is no longer self-sufficient — this test's third " +
			"leg proves nothing")
	}
	mkSwapLegWithSteps(t, db, "mutual-selfsuf", "mutual-supply2", StatusQueued,
		lineNode.Name, superNode.Name, bp.Code, selfSufficient)
	mkSwapLeg(t, db, "mutual-supply2", "mutual-selfsuf", StatusQueued,
		lineNode.Name, lineNode.Name, bp.Code)
	supply2, err := db.GetOrderByUUID("mutual-supply2")
	testutil.MustNoErr(t, err, "get supply2")
	supply2Steps, ok := decodeSteps(supply2.StepsJSON)
	if !ok {
		t.Fatal("supply2 has no readable steps")
	}
	if held, _ := d.swapLegHeld(supply2, supply2Steps); !held {
		t.Error("Face 2 did NOT hold a filler whose sibling IS a self-sufficient evac. That is the " +
			"case the arm exists for — the sibling clears the line itself, so holding the filler " +
			"sequences the pair instead of deadlocking it. With this arm gone, a filler can drive " +
			"onto a still-occupied line position")
	}
}
