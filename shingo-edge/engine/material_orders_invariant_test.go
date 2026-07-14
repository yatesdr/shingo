package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestSwapBuilders_EveryLegEndsOnADropoff pins an invariant Core depends on
// without stating it, and which nothing was checking.
//
// Core derives order.DeliveryNode from extractEndpoints (complex_steps.go), which
// takes the last PICKUP-OR-DROPOFF of a leg. That value is not merely displayed:
// patchRedirectSegments (complex_release.go) rewrites the final segment's last
// DROPOFF to it, so a redirect issued while the order was staged reaches the
// robot. The two agree only because every leg we build happens to end on a
// dropoff — so on the happy path the patch rewrites a dropoff to itself.
//
// Build a leg that ends on a pickup and that stops being true: Core's
// delivery_node would name a pickup node, and the patch would re-aim the robot's
// final drop at it. Nothing would fail loudly; a robot would just take a bin to
// the wrong place.
//
// So: every leg ends on a dropoff. If a new mode needs a leg that does not, this
// test is where you find out that it is not a local decision.
func TestSwapBuilders_EveryLegEndsOnADropoff(t *testing.T) {
	t.Parallel()

	claim := func(secondPaired string) *processes.NodeClaim {
		return &processes.NodeClaim{
			CoreNodeName:         "PRESS",
			Role:                 protocol.ClaimRoleConsume,
			InboundSource:        "MARKET-EMPTIES",
			InboundStaging:       "IN-STAGING",
			OutboundStaging:      "OUT-STAGING",
			OutboundDestination:  "MARKET",
			PairedCoreNode:       "INDEX-B",
			SecondPairedCoreNode: secondPaired,
		}
	}

	twoRobotA, twoRobotB := BuildTwoRobotSwapSteps(claim(""))
	pi2R1, pi2R2 := BuildTwoRobotPressIndexSwapSteps(claim(""))
	pi3R1, pi3R2 := BuildTwoRobotPressIndexSwapSteps(claim("INDEX-C"))

	legs := []struct {
		name  string
		steps []protocol.ComplexOrderStep
	}{
		{"single_robot", BuildSingleSwapSteps(claim(""))},
		{"sequential removal", BuildSequentialRemovalSteps(claim(""))},
		{"sequential backfill", BuildSequentialBackfillSteps(claim(""))},
		{"two_robot A", twoRobotA},
		{"two_robot B", twoRobotB},
		{"press_index 2-pos R1", pi2R1},
		{"press_index 2-pos R2", pi2R2},
		{"press_index 3-pos R1", pi3R1},
		{"press_index 3-pos R2", pi3R2},
	}

	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			if len(leg.steps) == 0 {
				t.Fatal("builder produced no steps — the claim fixture is missing a required field")
			}
			last := leg.steps[len(leg.steps)-1]
			if last.Action != protocol.ActionDropoff {
				t.Errorf("leg ends on %q at %q, want a dropoff.\n"+
					"Core's extractEndpoints takes the last pickup-OR-dropoff as the order's delivery_node, and "+
					"patchRedirectSegments rewrites the leg's final dropoff to that value — so a leg ending on a pickup "+
					"makes Core re-aim the robot's last drop at the node it picked up from. This is not a local change.",
					last.Action, last.Node)
			}
		})
	}
}
