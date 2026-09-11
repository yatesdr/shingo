package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestSingleRobotSwap_ProduceLineRebindIsProcessed is census 26: the EDGE half
// of the single_robot line rebind, on a PRODUCE cell.
//
// TestSingleRobotSwap_LineRebindIsProcessed pins it for a consume node. A produce
// single_robot swap has the identical step list — the step-7 placement of the
// fresh carrier is an intermediate dropoff, so its bind rides Core's
// UOPAdjustment{Bound} broadcast — but the carrier it places is an EMPTY one the
// press is about to fill, so it arrives with nothing remaining. If the Edge's
// Bound arm treated "nothing remaining" as nothing to bind, a produce press would
// be left unbound with an empty carrier standing on it, and SimMachineReady stops
// a produce node on active_bin_id NULL exactly as it stops a consume one.
//
// COVERAGE PIN. Expected to pass at bcbde0d2. MUTATION: in HandleUOPAdjustment's
// Bound arm, refuse a bind when NewRemaining <= 0 — this fails; the consume twin
// does not.
func TestSingleRobotSwap_ProduceLineRebindIsProcessed(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, protocol.SwapModeSingleRobot)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	const filledBin, emptyBin = int64(8801), int64(8802)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &[]int64{filledBin}[0]), "bind the filled carrier")
	eng := testEngine(t, db)

	// Step 4: the swap lifts the filled carrier off the press.
	testutil.MustNoErr(t, db.ClearProcessNodeActiveBinAndCount(nodeID), "unbind on the press pickup")

	// Step 7: the swap sets the fresh EMPTY carrier on the press.
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        emptyBin,
		CoreNodeName: node.CoreNodeName,
		NewRemaining: 0,
		Epoch:        3,
		Bound:        true,
	})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime after the placement")
	if rt.ActiveBinID == nil {
		t.Fatal("the produce press is UNBOUND after its line placement was announced — an empty carrier " +
			"is standing on it and the press has nothing to count into")
	}
	if *rt.ActiveBinID != emptyBin {
		t.Fatalf("press bound to bin %d, want the empty carrier %d", *rt.ActiveBinID, emptyBin)
	}
}
