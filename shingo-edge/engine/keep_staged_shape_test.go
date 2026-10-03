package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// THE KEEP-STAGED SWAP, AS IT GOES OUT ON THE WIRE.
//
// A keep-staged claim keeps a spare on its inbound staging node and its swap
// starts from it. Four things hold for every leg it sends, steady state and
// changeover, both roles, both modes that offer it:
//
//   - there is no fetch: nothing picks up at the inbound source;
//   - the spare is lifted once, by a pickup that names the claim's part and is
//     never Empty (spotPickup; Core's TestSpotPickup_HowTheStepFlagJudgesTheBin
//     says why);
//   - a station wait comes before that lift, and on the two-robot supply it
//     stamps the spot itself, so a cancel while the robot waits leaves the
//     spare where it stands;
//   - no complex leg drops on the spot. Every arrival at a keep-staged spot is
//     a plain order, because Core's dropoff gate counts orders DELIVERING to a
//     node and cannot see a complex leg staging there on its way to the line.

func keepStagedClaim(role protocol.ClaimRole, mode protocol.SwapMode, payload string) *processes.NodeClaim {
	c := goldenClaim(role, mode, payload, "", false)
	c.PairedCoreNode = "" // single_robot forbids pairing; two_robot does not read it
	c.KeepStaged = true
	return c
}

func assertKeepStagedLeg(t *testing.T, what string, steps []protocol.ComplexOrderStep, claim *processes.NodeClaim, wantLift bool) {
	t.Helper()
	lifts, waitedBefore := 0, false
	sawWait := false
	for _, s := range steps {
		switch {
		case s.Action == protocol.ActionWait:
			sawWait = true
		case s.Action == protocol.ActionPickup && s.Node == claim.InboundSource:
			t.Errorf("%s fetches from the inbound source %q: a keep-staged swap has no fetch", what, s.Node)
		case s.Action == protocol.ActionDropoff && s.Node == claim.InboundStaging:
			t.Errorf("%s drops on the keep-staged spot %q: every arrival there must be a plain order", what, s.Node)
		case s.Action == protocol.ActionPickup && s.Node == claim.InboundStaging:
			lifts++
			waitedBefore = sawWait
			if s.Empty || s.PayloadCode != claim.PayloadCode {
				t.Errorf("%s lifts the spare with %+v, want a pickup naming %q and never Empty", what, s, claim.PayloadCode)
			}
		}
	}
	if !wantLift {
		if lifts != 0 {
			t.Errorf("%s lifts the spare %d times, want none", what, lifts)
		}
		return
	}
	if lifts != 1 {
		t.Fatalf("%s lifts the spare %d times, want exactly once: %+v", what, lifts, steps)
	}
	if !waitedBefore {
		t.Errorf("%s lifts the spare before any station wait: %+v", what, steps)
	}
}

func TestKeepStagedSwap_LiftsTheSpareOnceAfterTheWait(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeSingleRobot} {
			claim := keepStagedClaim(role, mode, "P-RUN")
			disp, err := BuildSwapDispatch(node, claim)
			if err != nil {
				t.Fatalf("%s/%s: %v", role, mode, err)
			}
			what := string(role) + "/" + string(mode)
			assertKeepStagedLeg(t, what+" steady A", disp.StepsA, claim, true)
			if mode == protocol.SwapModeTwoRobot {
				assertKeepStagedLeg(t, what+" steady B", disp.StepsB, claim, false)
				assertOneStationWait(t, disp.StepsA, claim.InboundStaging, "swap", what+" steady supply")
			}

			for _, situation := range []ChangeoverSituation{SituationSwap, SituationEvacuate} {
				from := keepStagedClaim(role, mode, "P-FROM")
				from.KeepStaged = false
				to := keepStagedClaim(role, mode, "P-TO")
				action := planNodeAction(ChangeoverNodeDiff{CoreNodeName: "LINE", Situation: situation,
					FromClaim: from, ToClaim: to}, node, false, nil)
				if action.Err != nil {
					t.Fatalf("%s %s: %v", what, situation, action.Err)
				}
				cw := what + " changeover " + string(situation)
				switch mode {
				case protocol.SwapModeTwoRobot:
					supply := specSteps(action.SupplyOrder)
					assertKeepStagedLeg(t, cw+" supply", supply, to, true)
					assertOneStationWait(t, supply, to.InboundStaging, "ready", cw+" supply")
					assertKeepStagedLeg(t, cw+" evac", specSteps(action.EvacOrder), to, false)
				case protocol.SwapModeSingleRobot:
					// The spare needs no stage order: order B evacuates the line and
					// collects it.
					if action.SupplyOrder != nil {
						t.Errorf("%s planned a stage order %+v; the spare is already staged", cw, action.SupplyOrder)
					}
					assertKeepStagedLeg(t, cw+" order B", specSteps(action.EvacOrder), to, true)
				}
				if action.EvacOrder == nil || action.EvacOrder.Complex.PayloadCode != from.PayloadCode {
					t.Errorf("%s: the evac-bearing order must carry the outgoing payload (ALN_001): %+v", cw, action.EvacOrder)
				}
			}
		}
	}
}

// A keep-staged FROM claim changing over to a claim that keeps no spare is
// planned like any other changeover: the full shape for the to-claim. The spare
// left on the spot is the changeover-start reconcile's to return.
func TestPlanNodeAction_KeepStagedFromClaimIsPlanned(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeSingleRobot} {
		from := keepStagedClaim(protocol.ClaimRoleConsume, mode, "P-FROM")
		to := keepStagedClaim(protocol.ClaimRoleConsume, mode, "P-TO")
		to.KeepStaged = false
		action := planNodeAction(ChangeoverNodeDiff{CoreNodeName: "LINE", Situation: SituationSwap,
			FromClaim: from, ToClaim: to}, &processes.Node{ID: 1, Name: "LINE"}, false, nil)
		if action.Err != nil {
			t.Fatalf("%s: a keep-staged from-claim was refused: %v", mode, action.Err)
		}
		if refills := refillPickupsIn(action, to.InboundSource); len(refills) == 0 {
			t.Errorf("%s: the to-claim keeps no spare, so its supply fetches from %q; it did not", mode, to.InboundSource)
		}
	}
}
