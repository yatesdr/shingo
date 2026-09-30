package engine

// release_matrix_single_test.go — the single_robot steady state.
//
// One robot, one order: it lifts the node's bin and sets the new one in a
// single leg, so it has no pair and no pair door; the operator releases it with
// the per-order click (door 2). Same runner and outcome as the matrix.

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// singleRobot builds a steady-state single_robot node through the REQUEST door
// and names its one order "swap".
func (h *relHarness) singleRobot(role protocol.ClaimRole) {
	h.t.Helper()
	h.seedNode(pairSpec{mode: protocol.SwapModeSingleRobot, role: role})
	var res *NodeOrderResult
	var err error
	if role == protocol.ClaimRoleConsume {
		res, err = h.eng.RequestNodeMaterial(h.nodeID, 1)
	} else {
		res, err = h.eng.RequestProduceSwap(h.nodeID)
	}
	testutil.MustNoErr(h.t, err, "request single_robot swap")
	o := res.Order
	if o == nil {
		o = res.OrderA
	}
	if o == nil {
		h.t.Fatalf("fixture: single_robot request built no order: %+v", res)
	}
	h.addLeg("swap", o.ID)
}

func TestReleaseMatrixSingleRobot(t *testing.T) {
	t.Parallel()
	S := protocol.StatusStaged
	runRelCells(t, []relCell{
		// Produce: the manifest ships at REQUEST for a non-two-robot mode, so
		// the count is already 0 here and the release carries no paperwork.
		{name: "d2/single_robot/swap leg staged",
			want:  "ok | swap=in_transit | rel=swap | ingest=0 capred=0 | uop=0 | env:swap uop=nil kind=-",
			build: func(h *relHarness) { h.singleRobot(protocol.ClaimRoleProduce); statuses(h, "swap", S) },
			act:   orderClick("swap", dispEmpty), probe: probes(pUOP, pEnv)},
		{name: "d2/single_robot/swap leg, curtain live",
			want:  "refuse:curtain | swap=staged | rel=- | ingest=0 capred=0 | uop=0",
			build: withCurtain(curtainLive, func(h *relHarness) { h.singleRobot(protocol.ClaimRoleProduce); statuses(h, "swap", S) }),
			act:   orderClick("swap", dispEmpty), probe: probes(pUOP)},
		{name: "d2/single_robot consume/swap leg, PULL PARTS",
			want: "ok | swap=in_transit | rel=swap | ingest=0 capred=-5 | uop=42 | env:swap uop=nil kind=pull_parts",
			build: func(h *relHarness) {
				h.singleRobot(protocol.ClaimRoleConsume)
				statuses(h, "swap", S)
				h.core.binAt = &NodeBinInfo{NodeName: fxPress, BinID: fxBin, PayloadCode: fxPart, UOPRemaining: fxCount, Occupied: true}
			},
			act: orderClick("swap", dispPull(5)), probe: probes(pUOP, pEnv)},
	})
}
