// walk_exact_style_test.go — the counter walk resolves each node's claim under
// the tick's own style: a node that style claims is charged under that claim,
// a node it does not claim is not charged.
package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestWalk_TickOfTheTargetStyleChargesAnIncomingOnlyNode: mid-changeover, a
// tick attributed to the target style charges a position only the target
// claims. Built by hand: nothing stamps a tick with the target style until
// the tick carries the PLC's part.
func TestWalk_TickOfTheTargetStyleChargesAnIncomingOnlyNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	pid, _, _, to, _, _ := seedChangeoverScenario(t, db)
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "TO-ONLY", Code: "TO-ONLY", Name: "TO-ONLY", Enabled: true})
	testutil.MustNoErr(t, err, "node")
	claimID, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: to, CoreNodeName: "TO-ONLY", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-NEW", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "claim")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, binPtr(9301), 40), "a cart")
	testutil.MustNoErr(t, db.SetTargetStyle(pid, &to), "a changeover to the target")

	tickConsume(eng, pid, to, 1)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 39 {
		t.Errorf("incoming-only cart after a target tick = %d, want 39", got)
	}
}

// TestWalk_TickOfTheTargetStyleChargesANodeBothClaim: mid-changeover, a tick
// attributed to the target style charges a node both styles claim under the
// target's claim. The active-first resolution found the active claim and
// dropped the tick as a style mismatch.
func TestWalk_TickOfTheTargetStyleChargesANodeBothClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	pid, nodeID, _, to, _, toClaim := seedChangeoverScenario(t, db)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &toClaim, binPtr(9302), 40), "the incoming cart")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-NEW", true, "delivery"), "its part")
	testutil.MustNoErr(t, db.SetTargetStyle(pid, &to), "a changeover to the target")

	tickConsume(eng, pid, to, 1)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 39 {
		t.Errorf("cart after a target tick = %d, want 39: the tick was dropped", got)
	}
}
