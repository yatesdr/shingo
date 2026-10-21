package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// secondReleaseWorld is one mode's steady consume swap at the front node with
// its bin's count set.
func secondReleaseWorld(t *testing.T, mode protocol.SwapMode, count int) *relHarness {
	t.Helper()
	h := newRelHarness(t)
	if mode == protocol.SwapModeSingleRobot {
		h.singleRobot(protocol.ClaimRoleConsume)
		statuses(h, "swap", protocol.StatusStaged)
	} else {
		h.steadyPair(pairSpec{mode: mode, role: protocol.ClaimRoleConsume})
		statuses(h, "evac", protocol.StatusStaged, "supply", protocol.StatusStaged)
	}
	rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, h.db.SetProcessNodeRuntime(h.nodeID, rt.ActiveClaimID, count), "the bin's count")
	h.binAtPress(count)
	return h
}

// A release for a leg already past its station wait declares nothing (order 337
// in the run at 1f623a65): the per-order door refuses it, and no envelope goes.
// The leg's own release is out; a second press, here the sim operator's other
// scheduler 160 ms later with the count it read before the pull, would carry a
// partial for a bin the first press emptied.
func TestReleasePin_ASecondReleasePastTheWaitDeclaresNothing(t *testing.T) {
	t.Parallel()
	modes := []protocol.SwapMode{
		protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex,
	}
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			h := secondReleaseWorld(t, mode, 20)
			lifter, err := "evac", error(nil)
			if mode == protocol.SwapModeSingleRobot {
				lifter = "swap"
				err = h.eng.ReleaseOrderWithLineside(h.leg("swap"), dispPull(20))
			} else {
				err = h.eng.ReleaseStagedOrders(h.nodeID, dispPull(20))
			}
			if err != nil {
				t.Fatalf("the press that pulls the 20 lineside: %v", err)
			}
			if st := h.order(lifter).Status; st != protocol.StatusInTransit {
				t.Fatalf("%s after its release: %s, want in_transit", lifter, st)
			}
			h.mark()
			if err := h.eng.ReleaseOrderWithLineside(h.leg(lifter), dispPartial(20)); err == nil {
				t.Errorf("a second release of the %s past its station wait was accepted; want it refused", lifter)
			}
			if rel := h.releasedSinceMark(); len(rel) != 0 {
				t.Errorf("released %v again with partial 20; a leg past its station wait declares nothing", rel)
			}
		})
	}
}
