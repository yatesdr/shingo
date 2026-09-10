package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// THE PIN UNDER FACE 1, AND IT GUARDS A NUMBER RATHER THAN A NAME.
//
// Core's evac anti-strand hold (swap_hold.go Face 1, ALN_003 2026-06-03) holds a
// leg that lifts the line's bin until its supply sibling has secured a
// replacement — otherwise the line is cleared with nothing coming and stands
// empty. It decides "does this leg secure its own replacement?" with
// legSecuresOwnReplacement (shingo-core/dispatch/swap_leg_role.go), and that
// function is:
//
//	return pickups > 1
//
// ITS NAME AND ITS TEST ARE NOT THE SAME QUESTION. The doc says the leg "brings
// a replacement INTO the swap itself". The code counts pickups. A two_robot
// supply scores 2 because it re-picks its own staged bin — the right answer for
// the wrong reason, and the two only agree by accident of today's step shapes.
//
// THE DISARM IS ONE HOP AWAY, and the shape to copy is in this very file.
// buildSingleRobotChangeoverSwap's removal leg parks the old bin at
// OutboundStaging and picks it up again later — three pickups. Give the
// two_robot evac the same courtesy and its pickup count hits 2,
// legSecuresOwnReplacement starts returning true, and Face 1 silently stops
// firing for the mode it was written for. No test fails. No comment changes. The
// line strands on the next dry supermarket.
//
// So this pins the number. If you are here because this test went red, you did
// not break a test — you disarmed ALN_003's guard. Either keep the evac to one
// pickup, or change legSecuresOwnReplacement to ask its real question (does this
// leg pick up from somewhere that is NOT the line?) in the same commit.
func TestTwoRobotEvacHasExactlyOnePickup_FaceOnePin(t *testing.T) {
	t.Parallel()

	claim := &processes.NodeClaim{
		CoreNodeName:        "PIN-LINE",
		Role:                protocol.ClaimRoleConsume,
		SwapMode:            protocol.SwapModeTwoRobot,
		PayloadCode:         "PIN-PART",
		InboundSource:       "PIN-SUPER",
		InboundStaging:      "PIN-IN-STAGE",
		OutboundStaging:     "PIN-OUT-STAGE",
		OutboundDestination: "PIN-OUT",
	}
	toClaim := *claim
	toClaim.PayloadCode = "PIN-PART-B"

	_, steadyEvac := BuildTwoRobotSwapSteps(claim)
	changeover := buildTwoRobotChangeoverSwap(claim, &toClaim)
	if changeover.Roles == nil {
		t.Fatal("buildTwoRobotChangeoverSwap produced no roles — the shape this pins is gone; " +
			"re-derive Face 1's population before deleting this test")
	}

	for _, tc := range []struct {
		what  string
		steps []protocol.ComplexOrderStep
	}{
		{"steady state (BuildTwoRobotSwapSteps orderB)", steadyEvac},
		{"changeover (buildTwoRobotChangeoverSwap evac)", changeover.Roles.evac.steps},
	} {
		if len(tc.steps) == 0 {
			t.Fatalf("%s: built no steps at all", tc.what)
		}
		pickups := 0
		for _, s := range tc.steps {
			if s.Action == string(protocol.ActionPickup) {
				pickups++
			}
		}
		if pickups != 1 {
			t.Errorf("the two_robot evac %s now has %d pickups, want exactly 1.\n"+
				"THIS DISARMS FACE 1. Core's evac anti-strand hold asks "+
				"legSecuresOwnReplacement (shingo-core/dispatch/swap_leg_role.go), which is "+
				"`pickups > 1` — so a second pickup makes this leg read as self-sufficient and "+
				"the hold stops firing for two_robot entirely. The evac then lifts the line's "+
				"bin with nothing guaranteed to be coming, which is ALN_003 (2026-06-03).\n"+
				"If the extra pickup is deliberate, change legSecuresOwnReplacement to ask its "+
				"real question in the same commit — not this assertion.\nsteps: %+v",
				tc.what, pickups, tc.steps)
		}
	}

	// The counter-example, asserted rather than described: the single-robot
	// changeover removal really does carry the staging round-trip, so the shape
	// above is a choice this pin protects and not a thing that cannot happen.
	single := buildSingleRobotChangeoverSwap(claim, &toClaim, false)
	singlePickups := 0
	for _, s := range single.StepsB {
		if s.Action == string(protocol.ActionPickup) {
			singlePickups++
		}
	}
	if singlePickups < 2 {
		t.Errorf("buildSingleRobotChangeoverSwap's removal leg has %d pickups, want >1 — this test's "+
			"whole premise is that the multi-pickup shape exists next door and is one edit away "+
			"from the two_robot evac. If that stopped being true, re-derive the hazard", singlePickups)
	}
}
