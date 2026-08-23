package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

func stagedFanoutClaims(seats []string, second string) (processes.NodeClaim, processes.NodeClaim) {
	from := processes.NodeClaim{
		CoreNodeName:              "PRESS",
		Role:                      protocol.ClaimRoleProduce,
		SwapMode:                  protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:               "PART-A",
		PairedCoreNode:            "PRESS_B",
		SecondPairedCoreNode:      second,
		OutboundDestination:       "MARKET",
		ChangeoverEvacDestination: "TOOLING-BAY",
		ChangeoverEvacSeats:       seats,
	}
	to := from
	to.PayloadCode = "PART-B"
	to.ChangeoverEvacSeats = nil
	to.ChangeoverEvacDestination = ""
	to.InboundSource = "EMPTIES"
	to.InboundStaging = "IN-STAGE"
	return from, to
}

func fanoutNodes(diffs []ChangeoverNodeDiff) string {
	names := make([]string, 0, len(diffs))
	for _, d := range diffs {
		names = append(names, d.CoreNodeName)
	}
	return strings.Join(names, ",")
}

// A three-seat evacuation is six orders, and a NodeAction carries one supply
// and one evac by design. So it becomes three actions — the same shape the
// different-bin-type case already uses — rather than a widened struct the
// applier, the task states and the order count would all have to learn.
func TestFanOutStagedToolingEvacuation_OneDiffPerMarkedSeat(t *testing.T) {
	t.Parallel()
	from, to := stagedFanoutClaims([]string{domain.EvacSeatFront, domain.EvacSeatSecond}, "PRESS_C")
	parent := ChangeoverNodeDiff{CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &from, ToClaim: &to}

	got := FanOutStagedToolingEvacuation([]ChangeoverNodeDiff{parent})
	if n := fanoutNodes(got); n != "PRESS,PRESS_C" {
		t.Fatalf("diffs = %q, want PRESS,PRESS_C — one per MARKED seat, front to back", n)
	}
	for _, d := range got {
		if d.Situation != SituationEvacuate {
			t.Errorf("%s: situation = %q, want evacuate", d.CoreNodeName, d.Situation)
		}
		if d.FromClaim == nil || d.ToClaim == nil {
			t.Fatalf("%s: a seat evacuation needs both claims", d.CoreNodeName)
		}
		// The synthesizer ZEROES InboundStaging, and a staged evacuation is
		// the one per-position shape that needs it — the incoming bin waits
		// there for tooling-done. This is the assertion that fails if the
		// restore is dropped; the evac destination and inbound source are
		// carried by the synthesizer's struct copy and asserted below only as
		// a guard against it starting to zero them too.
		if d.ToClaim.InboundStaging != "IN-STAGE" {
			t.Errorf("%s: staging node lost in the fan-out; got %q",
				d.CoreNodeName, d.ToClaim.InboundStaging)
		}
		if d.FromClaim.ChangeoverEvacDestination != "TOOLING-BAY" {
			t.Errorf("%s: evac destination lost in the fan-out; got %q",
				d.CoreNodeName, d.FromClaim.ChangeoverEvacDestination)
		}
		if d.ToClaim.InboundSource != "EMPTIES" {
			t.Errorf("%s: inbound source lost in the fan-out; got %q",
				d.CoreNodeName, d.ToClaim.InboundSource)
		}
		// A synthesized single position has no seats of its own.
		if len(d.FromClaim.ChangeoverEvacSeats) != 0 || len(d.ToClaim.ChangeoverEvacSeats) != 0 {
			t.Errorf("%s: the parent's seat set rode into a per-position claim: from=%v to=%v",
				d.CoreNodeName, d.FromClaim.ChangeoverEvacSeats, d.ToClaim.ChangeoverEvacSeats)
		}
	}
}

// UNMARKED SEATS GET NO DIFF AT ALL — the stated default. A seat whose bins do
// not block the tool keeps them and the press indexes around it.
func TestFanOutStagedToolingEvacuation_UnmarkedSeatsStayPut(t *testing.T) {
	t.Parallel()
	from, to := stagedFanoutClaims([]string{domain.EvacSeatPaired}, "PRESS_C")
	parent := ChangeoverNodeDiff{CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &from, ToClaim: &to}

	if n := fanoutNodes(FanOutStagedToolingEvacuation([]ChangeoverNodeDiff{parent})); n != "PRESS_B" {
		t.Errorf("diffs = %q, want only the marked seat PRESS_B", n)
	}
}

// A marked seat the layout does not have is dropped, not emitted at "": a diff
// at the empty node name is a plan to move a bin to nowhere.
func TestFanOutStagedToolingEvacuation_SeatTheLayoutLacksIsDropped(t *testing.T) {
	t.Parallel()
	from, to := stagedFanoutClaims([]string{domain.EvacSeatFront, domain.EvacSeatSecond}, "")
	parent := ChangeoverNodeDiff{CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &from, ToClaim: &to}

	if n := fanoutNodes(FanOutStagedToolingEvacuation([]ChangeoverNodeDiff{parent})); n != "PRESS" {
		t.Errorf("diffs = %q, want only PRESS — the third seat does not exist on this press", n)
	}
}

// Everything that is not a staged tooling evacuation passes through untouched.
// This fan-out runs over every diff in the plan, so a claim it should ignore
// and quietly rewrites is the expensive failure.
func TestFanOutStagedToolingEvacuation_PassesEverythingElseThrough(t *testing.T) {
	t.Parallel()
	markedFrom, to := stagedFanoutClaims([]string{domain.EvacSeatFront}, "PRESS_C")

	noSeats, to2 := stagedFanoutClaims(nil, "PRESS_C")
	otherMode, to3 := stagedFanoutClaims([]string{domain.EvacSeatFront}, "PRESS_C")
	otherMode.SwapMode = protocol.SwapModeTwoRobot

	for _, tc := range []struct {
		name string
		diff ChangeoverNodeDiff
	}{
		{"a swap, not an evacuate", ChangeoverNodeDiff{
			CoreNodeName: "PRESS", Situation: SituationSwap, FromClaim: &markedFrom, ToClaim: &to}},
		{"no seats marked", ChangeoverNodeDiff{
			CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &noSeats, ToClaim: &to2}},
		{"not press-index", ChangeoverNodeDiff{
			CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &otherMode, ToClaim: &to3}},
		{"a drop has no incoming claim", ChangeoverNodeDiff{
			CoreNodeName: "PRESS", Situation: SituationEvacuate, FromClaim: &markedFrom, ToClaim: nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FanOutStagedToolingEvacuation([]ChangeoverNodeDiff{tc.diff})
			if len(got) != 1 || got[0].CoreNodeName != "PRESS" || got[0].Situation != tc.diff.Situation {
				t.Errorf("diff was rewritten; got %+v", got)
			}
		})
	}
}
