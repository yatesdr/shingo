// walk_exact_style_test.go — the counter walk resolves each node's claim under
// the tick's own style: a node that style claims is charged under that claim,
// a node it does not claim is not charged.
package engine

import (
	"testing"

	"shingo/protocol/testutil"
)

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
