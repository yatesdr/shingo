package engine

import (
	"testing"

	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// The staged-seat branch of planEvacuateAction had no test that reached it.
//
// The merge report claimed the fan-out test pinned this branch's ordering. It
// does not: FanOutStagedToolingEvacuation returns diffs and never calls the
// planner. The report's fallback justification — "planFallbackStagingAction
// would catch it" — is impossible, because
// directTripChangeoverMode(pressPositionSwapMode) is true, so that fallback is
// dead for press positions. The ordering is right and the evidence was wrong.
//
// It also pins one deletable line. FanOutStagedToolingEvacuation ends with
//
//	toSeat.InboundStaging = to.InboundStaging
//
// putting back what SynthesizePressPositionClaim zeroes. Delete it and
// diff.ToClaim.InboundStaging is "", the staged-seat branch below never fires,
// and a staged tooling evacuation silently plans as something else. Nothing
// asserted it.
// ---------------------------------------------------------------------------

// stagedSeatPlan runs the real pipeline — fan-out, then plan — and returns the
// action planned for one marked seat.
func stagedSeatPlan(t *testing.T, seats []string, seat string) changeover.NodeAction {
	t.Helper()
	from, to := stagedFanoutClaims(seats, "PRESS_C")
	parent := ChangeoverNodeDiff{
		CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &from, ToClaim: &to,
	}
	diffs := FanOutStagedToolingEvacuation([]ChangeoverNodeDiff{parent})

	var target *ChangeoverNodeDiff
	for i := range diffs {
		if diffs[i].CoreNodeName == seat {
			target = &diffs[i]
		}
	}
	if target == nil {
		t.Fatalf("the fan-out produced no diff for seat %s — got %s", seat, fanoutNodes(diffs))
	}

	node := &processes.Node{CoreNodeName: target.CoreNodeName, Name: target.CoreNodeName}
	return planEvacuateAction(changeover.NodeAction{}, *target, node, false, nil)
}

// TestPlanEvacuateAction_StagedSeatTakesItsOwnBranch drives the planner for a
// synthesized staged seat and asserts the staged-seat shape fires.
func TestPlanEvacuateAction_StagedSeatTakesItsOwnBranch(t *testing.T) {
	t.Parallel()
	action := stagedSeatPlan(t, []string{domain.EvacSeatFront}, "PRESS")

	if action.Err != nil {
		t.Fatalf("planning a staged seat evacuation failed: %v", action.Err)
	}
	if action.LogTag != "evacuate_staged_seat" {
		t.Fatalf("LogTag = %q, want evacuate_staged_seat.\n"+
			"A staged seat is one synthesized position with its own routing. Falling into any other "+
			"branch means the whole-cell staging logic is deciding for it — and the fallback the "+
			"merge report named cannot even run here, because directTripChangeoverMode is true for "+
			"press positions.", action.LogTag)
	}
	if action.NextState != domain.NodeTaskStagingRequested {
		t.Errorf("NextState = %q, want staging_requested — the incoming bin waits at staging "+
			"until tooling-done", action.NextState)
	}
}

// TestFanOutStagedToolingEvacuation_RestoresInboundStaging is the reachability
// assertion for the one deletable line. It asserts the OBSERVABLE consequence,
// not the assignment: a seat whose staging was not restored plans as something
// other than a staged seat.
func TestFanOutStagedToolingEvacuation_RestoresInboundStaging(t *testing.T) {
	t.Parallel()
	from, to := stagedFanoutClaims([]string{domain.EvacSeatFront}, "PRESS_C")
	parent := ChangeoverNodeDiff{
		CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &from, ToClaim: &to,
	}
	diffs := FanOutStagedToolingEvacuation([]ChangeoverNodeDiff{parent})
	if len(diffs) == 0 {
		t.Fatal("no diffs")
	}
	seat := diffs[0]
	if seat.ToClaim == nil {
		t.Fatal("a seat evacuation needs a to-claim")
	}
	if seat.ToClaim.InboundStaging != to.InboundStaging {
		t.Fatalf("seat to-claim InboundStaging = %q, want %q.\n"+
			"SynthesizePressPositionClaim zeroes it — correct, a single position has no staging "+
			"hop of its own — and a STAGED evacuation is the one shape that needs it back: the "+
			"incoming bin waits there for tooling-done. Without it planEvacuateAction never takes "+
			"the staged-seat branch and the evacuation silently plans as something else.",
			seat.ToClaim.InboundStaging, to.InboundStaging)
	}
}
