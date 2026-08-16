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

// TestEveryEdgeAuthoredWaitIsStamped is W1's drift test on the Edge side: every
// wait this station authors declares that the station owns it.
//
// ── WHY DECLARING IT MATTERS ──────────────────────────────────────────────
//
// The old rule was "no kind means the operator's", and inside Core that was
// enough — the splice stamps the lane waits and everything else is the
// station's by elimination. It fails at the boundary. The Edge holds the plan it
// authored and used to receive no stamp at all, so it could not tell "unmarked
// because I own it" from "unmarked because nobody said", and neither could the
// board it draws. The sim operator guessed with a three-strike retry cap and
// guessed wrong; a human at an HMI has less to go on than that.
//
// So the stamp is now made, not inferred, and this walks every builder to keep
// it that way. A twenty-third wait added as a raw literal fails here rather than
// reaching a plant as an unowned step.
func TestEveryEdgeAuthoredWaitIsStamped(t *testing.T) {
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
	from, to := claim(""), claim("")
	to.CoreNodeName = "PRESS-B"

	twoRobotA, twoRobotB := BuildTwoRobotSwapSteps(claim(""))
	pi2R1, pi2R2 := BuildTwoRobotPressIndexSwapSteps(claim(""))
	pi3R1, pi3R2 := BuildTwoRobotPressIndexSwapSteps(claim("INDEX-C"))
	swapCO := BuildSwapChangeoverSteps(from, to, "PRESS-B", "PRESS")
	evacCO := BuildEvacuateChangeoverSteps(from, to, "PRESS-B", "PRESS")

	legs := map[string][]protocol.ComplexOrderStep{
		"release":               BuildReleaseSteps(claim("")),
		"staged release":        BuildStagedReleaseSteps(claim("")),
		"stage":                 BuildStageSteps(claim("")),
		"staged deliver":        BuildStagedDeliverSteps(claim("")),
		"single_robot":          BuildSingleSwapSteps(claim("")),
		"sequential removal":    BuildSequentialRemovalSteps(claim("")),
		"sequential backfill":   BuildSequentialBackfillSteps(claim("")),
		"two_robot A":           twoRobotA,
		"two_robot B":           twoRobotB,
		"press_index 2-pos R1":  pi2R1,
		"press_index 2-pos R2":  pi2R2,
		"press_index 3-pos R1":  pi3R1,
		"press_index 3-pos R2":  pi3R2,
		"keep-staged evac":      BuildKeepStagedEvacSteps(from),
		"keep-staged deliver":   BuildKeepStagedDeliverSteps(to),
		"keep-staged combined":  BuildKeepStagedCombinedSteps(from, to),
		"changeover swap A":     swapCO.StepsA,
		"changeover swap B":     swapCO.StepsB,
		"changeover evacuate A": evacCO.StepsA,
		"changeover evacuate B": evacCO.StepsB,
	}

	seen := 0
	for name, steps := range legs {
		for i, s := range steps {
			if s.Action != protocol.ActionWait {
				continue
			}
			seen++
			if s.WaitKind != waitKindStation {
				t.Errorf("%s step %d: wait at %q carries wait_kind %q, want %q.\n"+
					"Every wait this file authors is a STATION fact — the line has cleared, the tooling "+
					"is done, the operator is ready — and Core cannot observe any of them. An unstamped "+
					"wait reaches the far side as an unowned step: the board cannot say whether to offer "+
					"Release, which is how three robots sat for a whole soak. Build it with "+
					"stationWait().", name, i, s.Node, s.WaitKind, waitKindStation)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no wait steps found in any builder — the fixture stopped exercising them, so this " +
			"test is now vacuous")
	}
}

// TestEveryStagingDropoffIsDeclared is the mirror of the wait stamp, one field
// over: every dropoff this station authors at a STAGING node declares that the
// node holds one bin and must be reserved.
//
// ── WHY DECLARING IT MATTERS ──────────────────────────────────────────────
//
// Core gates its destination checks on node ROLE, and a staging node fails that
// test — it is seeded as a station with no parent, so Core reads "not a storage
// slot" and both the capacity check and the slot reservation stand down. Nothing
// reserves the node and nothing asks whether it is free.
//
// Core cannot repair that by looking harder: every station carries the one
// STATION node type, the plantspec Kind is advisory and unpersisted, and the
// inbound/outbound staging designation lives in the cell config on THIS side.
// The station is the only party that knows, which makes this the exact shape of
// the wait stamp above — a fact the far side cannot infer, carried on the wire
// rather than guessed at.
//
// Springfield, 2026-08-12: AMR-04 held a bin for 48 minutes at a full SLN_003,
// the fleet reporting the robot RUNNING with no error, until an admin cancelled
// the order two hours in.
//
// LINE DROPOFFS MUST NOT BE DECLARED, and that half is asserted too. Gating a
// supply leg on a line node its sibling evac is on the way to clear re-creates
// the deadlock Core's 2b05dce fixed, so this test fails in both directions: a
// staging dropoff that is not declared, and a line dropoff that is.
//
// MUTATION (verified): return a plain literal from stagingDropoff. Eight legs
// report an undeclared staging dropoff.
func TestEveryStagingDropoffIsDeclared(t *testing.T) {
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
	from, to := claim(""), claim("")
	to.CoreNodeName = "PRESS-B"

	// The staging nodes, by name — the same two every fixture claim carries. A
	// dropoff at one of these is staging; a dropoff anywhere else is not.
	staging := map[string]bool{"IN-STAGING": true, "OUT-STAGING": true}
	// The nodes that must NEVER be declared: a line dropoff gated this way is the
	// 2b05dce deadlock coming back.
	line := map[string]bool{"PRESS": true, "PRESS-B": true, "INDEX-B": true, "INDEX-C": true}

	twoRobotA, twoRobotB := BuildTwoRobotSwapSteps(claim(""))
	pi2R1, pi2R2 := BuildTwoRobotPressIndexSwapSteps(claim(""))
	pi3R1, pi3R2 := BuildTwoRobotPressIndexSwapSteps(claim("INDEX-C"))
	swapCO := BuildSwapChangeoverSteps(from, to, "PRESS-B", "PRESS")
	evacCO := BuildEvacuateChangeoverSteps(from, to, "PRESS-B", "PRESS")

	legs := map[string][]protocol.ComplexOrderStep{
		"release":               BuildReleaseSteps(claim("")),
		"staged release":        BuildStagedReleaseSteps(claim("")),
		"stage":                 BuildStageSteps(claim("")),
		"staged deliver":        BuildStagedDeliverSteps(claim("")),
		"single_robot":          BuildSingleSwapSteps(claim("")),
		"sequential removal":    BuildSequentialRemovalSteps(claim("")),
		"sequential backfill":   BuildSequentialBackfillSteps(claim("")),
		"two_robot A":           twoRobotA,
		"two_robot B":           twoRobotB,
		"press_index 2-pos R1":  pi2R1,
		"press_index 2-pos R2":  pi2R2,
		"press_index 3-pos R1":  pi3R1,
		"press_index 3-pos R2":  pi3R2,
		"keep-staged evac":      BuildKeepStagedEvacSteps(from),
		"keep-staged deliver":   BuildKeepStagedDeliverSteps(to),
		"keep-staged combined":  BuildKeepStagedCombinedSteps(from, to),
		"changeover swap A":     swapCO.StepsA,
		"changeover swap B":     swapCO.StepsB,
		"changeover evacuate A": evacCO.StepsA,
		"changeover evacuate B": evacCO.StepsB,
	}

	seen := 0
	for name, steps := range legs {
		for i, s := range steps {
			if s.Action != protocol.ActionDropoff {
				continue
			}
			if staging[s.Node] {
				seen++
				if !s.ExclusiveSlot {
					t.Errorf("%s step %d: dropoff at staging node %q is not declared exclusive.\n"+
						"Core cannot tell a staging node from a line node — one STATION node type, an "+
						"advisory Kind that is never persisted, and the staging designation living in "+
						"OUR cell config. Undeclared, both of Core's destination gates stand down and "+
						"a second order is free to take the node: SPR AMR-04 held a bin 48 minutes at "+
						"a full SLN_003. Build it with stagingDropoff().", name, i, s.Node)
				}
			}
			if line[s.Node] && s.ExclusiveSlot {
				t.Errorf("%s step %d: dropoff at LINE node %q is declared exclusive.\n"+
					"A supply leg delivers to a line node that a sibling evac is on its way to clear. "+
					"Gating it re-creates the deadlock Core's 2b05dce fixed — which is the whole "+
					"reason Core's role test excludes line nodes rather than checking everything.",
					name, i, s.Node)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no staging dropoffs found in any builder — the fixture stopped exercising them, so " +
			"this test is now vacuous")
	}
}

// TestWaitKindStation_MatchesCore pins the cross-module constant. Edge cannot
// import Core, so the value is duplicated; if Core renames its side, the stamp
// silently stops matching the fence that reads it and every station wait becomes
// unowned. The literal here is the contract, spelled out so a rename has to
// touch both.
func TestWaitKindStation_MatchesCore(t *testing.T) {
	t.Parallel()
	if waitKindStation != "station" {
		t.Errorf("waitKindStation = %q, want %q — this must equal dispatch.WaitKindStation, which is "+
			"what Core's release fence and its population partition read", waitKindStation, "station")
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
