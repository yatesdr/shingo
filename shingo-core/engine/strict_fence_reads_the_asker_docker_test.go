//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store/nodes"
)

// TestStrictGroup_ComplexLegToThePairedNodeIsNotFencedForItsSupportedPress: a
// strict group that supports a press lets that press's complex leg take an
// empty from it, when the leg sets the empty down on the press's paired node.
//
// The strict fence asks whether the ASKER may source from the group
// (nodes.GroupFencesAsker, keyed on the need's ProcessNode). A complex leg's
// empty pickup used to build its need with ProcessNode = the leg's next
// dropoff (plan.Planner.StepNode, and the allocator's re-anchor), which for an
// index refill is the paired node, not the press. The paired node is not in
// the group's supports, so the press's own refill was fenced out of the group
// that exists to feed it. The next dropoff now rides as need.SetDownNode.
func TestStrictGroup_ComplexLegToThePairedNodeIsNotFencedForItsSupportedPress(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, btID := fenceGroup(t, db, "SFA-GRP", "SFA-TOTE-1", 0, 2, true, "SFA-PRESS")
	landAt(t, db, btID, firstChild(t, db, grpID).ID, "SFA-EMPTY")
	paired := &nodes.Node{Name: "SFA-INDEX-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(paired), "the paired node")

	// The refill half of an unflipped index leg: an empty from the group, set
	// down on the paired node, for the press the group supports.
	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "sfa-refill", Quantity: 1, ProcessNode: "SFA-PRESS",
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: "SFA-GRP", Empty: true},
			{Action: protocol.ActionDropoff, Node: paired.Name},
		},
	})
	for range 3 {
		eng.RunFulfillmentScan()
	}

	o, err := db.GetOrderByUUID("sfa-refill")
	testutil.MustNoErr(t, err, "the refill leg")
	var steps []protocol.ComplexOrderStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "steps")
	if o.Status != protocol.StatusDispatched || len(steps) == 0 || steps[0].Node != firstChild(t, db, grpID).Name {
		t.Errorf("the refill is %s (code %q, cause %q, reason %q), its pickup at %v; want dispatched "+
			"with the empty from %s. The group supports SFA-PRESS, the leg's asker; %s is only "+
			"where the empty is set down",
			o.Status, o.QueueCode, o.QueueCause, o.QueueReason, steps, firstChild(t, db, grpID).Name, paired.Name)
	}
}
