package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestSingleRobotSwap_LineRebindIsProcessed is the EDGE half of the brief's
// named question: Core emits a Bound=true UOPAdjustment for a single_robot
// consume swap's line-dropoff (proved on the Core side by
// TestSingleRobotSwap_LineDropoffAnnouncesTheRebind) — does Edge process it?
//
// The sequence is the one that froze WELD-2, in the smallest form that has it:
// the line is bound to the spent carrier, the swap's step-4 pickup lifts it and
// Edge unbinds by bin identity, and the step-7 placement must rebind. If the
// rebind does not land, every other arm of HandleUOPAdjustment refuses an empty
// slot — the lifecycle-actor arm, the held-ticks arm, the bound-elsewhere arm —
// each for its own good reason, and the node stays unbound with no releaser and
// nothing to re-ask. SimMachineReady then stops the cell on active_bin_id NULL
// with a full carrier standing on it, which is the observed symptom exactly.
func TestSingleRobotSwap_LineRebindIsProcessed(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "SR-REBIND", PayloadCode: "PART-SR", UOPCapacity: 100, InitialUOP: 40,
	})
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	const spentBin, freshBin = int64(7701), int64(7702)

	// Bound to the spent carrier, mid-life.
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &[]int64{spentBin}[0]), "bind spent carrier")
	testutil.MustNoErr(t, db.UpdateProcessNodeUOP(nodeID, 12), "seed remaining")

	eng := testEngine(t, db)

	// ── STEP 4: the swap lifts the spent carrier off the line ──────────────
	// Edge clears active_bin_id by BIN IDENTITY. Driven through the store call
	// the handler uses, so the test does not depend on order plumbing that is
	// not the subject here.
	testutil.MustNoErr(t, db.ClearProcessNodeActiveBinAndCount(nodeID), "step 4: unbind on line pickup")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime after the lift")
	if rt.ActiveBinID != nil {
		t.Fatalf("precondition: node still bound to %v after its carrier was lifted", rt.ActiveBinID)
	}

	// ── STEP 7: the swap places the fresh carrier ──────────────────────────
	// Exactly the announcement Core puts on the wire from
	// handleStoreBlockCompleted: Bound, the fresh bin, the line node, the bin's
	// remaining and epoch. No Actor — the broadcast carries none.
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        freshBin,
		CoreNodeName: node.CoreNodeName,
		NewRemaining: 100,
		Epoch:        7,
		Bound:        true,
	})

	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime after the placement")
	if rt.ActiveBinID == nil {
		t.Fatal("THE NODE IS STILL UNBOUND after its line placement was announced.\n" +
			"This is WELD-2: a full carrier standing on the node, active_bin_id NULL, " +
			"SimMachineReady stopping the cell, and no releaser — nothing re-asks a bind.")
	}
	if *rt.ActiveBinID != freshBin {
		t.Fatalf("node bound to bin %d, want the fresh carrier %d", *rt.ActiveBinID, freshBin)
	}
	if rt.RemainingUOPCached <= 0 {
		t.Errorf("remaining_uop_cached = %d, want > 0 — a consume node with nothing left to "+
			"consume is the SECOND reason SimMachineReady stops a cell, so a rebind that "+
			"restores the pointer and not the count trades one stall for another",
			rt.RemainingUOPCached)
	}
}
