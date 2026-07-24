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

// TestPressIndexSteadyState_IndexLegPicksUpEmpty is the hop A3 regression
// (2026-07-23): the steady-state press-index index leg (R2) fetches the on-deck
// carrier as an EMPTY, so a wrong-part stamp on that empty — the half-cutover
// state that hung Hopkinsville — doesn't block the pickup. Every R2 pickup at a
// paired / on-deck node must carry Empty=true (Core then drops the payload
// filter and takes the empty regardless of its stamp).
func TestPressIndexSteadyState_IndexLegPicksUpEmpty(t *testing.T) {
	t.Parallel()

	claim := func(secondPaired string) *processes.NodeClaim {
		return &processes.NodeClaim{
			CoreNodeName:         "PRESS",
			Role:                 protocol.ClaimRoleProduce,
			InboundSource:        "MARKET-EMPTIES",
			OutboundDestination:  "MARKET",
			PairedCoreNode:       "INDEX-B",
			SecondPairedCoreNode: secondPaired,
		}
	}

	// 2-position: R2 = wait(B) → pickup(B, empty) → dropoff(PRESS).
	_, r2 := BuildTwoRobotPressIndexSwapSteps(claim(""))
	assertIndexPickupsEmpty(t, "2-pos", r2, map[string]bool{"INDEX-B": true})

	// 3-position: R2 picks up at B AND C, both empties moving forward.
	_, r3 := BuildTwoRobotPressIndexSwapSteps(claim("INDEX-C"))
	assertIndexPickupsEmpty(t, "3-pos", r3, map[string]bool{"INDEX-B": true, "INDEX-C": true})

	// A CONSUME press-index would index FULL bins, not empties — its index
	// pickups must stay full (produce-scoped flagging).
	consume := &processes.NodeClaim{
		CoreNodeName:        "PRESS",
		Role:                protocol.ClaimRoleConsume,
		InboundSource:       "MARKET-EMPTIES",
		OutboundDestination: "MARKET",
		PairedCoreNode:      "INDEX-B",
	}
	_, cr2 := BuildTwoRobotPressIndexSwapSteps(consume)
	for _, s := range cr2 {
		if s.Action == protocol.ActionPickup && s.Empty {
			t.Errorf("consume press-index index pickup must NOT be Empty (it indexes a full bin): %+v", s)
		}
	}
}

// assertIndexPickupsEmpty checks every pickup at an on-deck node carries Empty.
func assertIndexPickupsEmpty(t *testing.T, label string, steps []protocol.ComplexOrderStep, onDeck map[string]bool) {
	t.Helper()
	seen := map[string]bool{}
	for _, s := range steps {
		if s.Action != protocol.ActionPickup || !onDeck[s.Node] {
			continue
		}
		seen[s.Node] = true
		if !s.Empty {
			t.Errorf("%s: index-leg pickup at on-deck node %q must be Empty (it fetches an empty carrier; a wrong-part stamp must not block it)", label, s.Node)
		}
	}
	for node := range onDeck {
		if !seen[node] {
			t.Errorf("%s: expected an index-leg pickup at on-deck node %q", label, node)
		}
	}
}
