package www

import (
	"testing"

	"shingo/protocol"
)

// handlers_test_orders_staging_test.go — the Core-side mirror of the Edge's
// TestEveryStagingDropoffIsDeclared (shingo-edge/engine/material_orders_invariant_test.go),
// for the other two authors of complex steps in this tree.
//
// ── WHO AUTHORS STAGING DROPOFFS ──────────────────────────────────────────
//
// Three parties build complex-order plans: the Edge's step builders, the
// operator's staged form (apiManualOrderSubmit — fixed at handlers_orders.go,
// pinned by TestStagedOrder_DeclaresTheStagingDropoffExclusive), and THESE
// two /test-orders handlers — the Kafka route and the direct route, which
// build byte-identical step lists and together carried SIX undeclared
// staging dropoffs (two_robot resupply + single_robot × both handlers).
//
// The fix's first pass missed the operator door entirely because the
// investigation had been reading the Edge builders; this fence exists so
// the next author is found by enumeration rather than by an incident.
//
// ── WHY DECLARING IT MATTERS (the same sentence as the Edge's) ────────────
//
// Core gates its destination checks on node ROLE, and a staging node fails
// that test — it is seeded as a station with no parent, so Core reads "not
// a storage slot" and both the capacity check and the slot reservation
// stand down. Nothing reserves the node and nothing asks whether it is
// free, so a second order takes it while the first robot is on its way.
//
// AND THE LINE HALF IS ASSERTED TOO, in both directions: a dropoff at the
// CELL (location) must not be declared — gating a line/cell dropoff
// re-creates the deadlock Core's 2b05dce fixed — and a staging dropoff
// must be.
//
// MUTATION (verified): make stagingDropoffStep return the plain
// dropoffStep literal. All six staging dropoffs report undeclared.
func TestEveryWwwStagingDropoffIsDeclared(t *testing.T) {
	t.Parallel()

	req := complexSwapRequest{
		CycleMode:           protocol.SwapModeSingleRobot, // widest: names every field
		Location:            "CELL-LINE",
		InboundStaging:      "IN-STAGING",
		OutboundStaging:     "OUT-STAGING",
		InboundSource:       "MARKET-EMPTIES",
		OutboundDestination: "MARKET",
		PayloadCode:         "PART-A",
	}

	staging := map[string]bool{"IN-STAGING": true, "OUT-STAGING": true}
	cell := map[string]bool{"CELL-LINE": true, "MARKET": true, "MARKET-EMPTIES": true}

	legs := map[string][]protocol.ComplexOrderStep{
		"sequential":         buildSwapSequentialSteps(req),
		"two_robot resupply": buildSwapResupplySteps(req),
		"two_robot removal":  buildSwapRemovalSteps(req),
		"single_robot":       buildSwapSingleRobotSteps(req),
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
						"Core cannot tell a staging node from a line node — one STATION node type, and the "+
						"staging designation lives in the Edge's cell config. Undeclared, both of Core's "+
						"destination gates stand down and a second order is free to take the node, so "+
						"the first robot arrives to find it full. Build it with stagingDropoffStep().",
						name, i, s.Node)
				}
			}
			if cell[s.Node] && s.ExclusiveSlot {
				t.Errorf("%s step %d: dropoff at cell/line/destination node %q is declared exclusive.\n"+
					"Gating a cell dropoff re-creates the deadlock Core's 2b05dce fixed — the location "+
					"field is the cell the swap happens at, and its sibling evac is on the way to clear it.",
					name, i, s.Node)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no staging dropoffs found in any builder — the walk is empty, so this test is " +
			"vacuous and the fence guards nothing. Either the builders moved or the fixture " +
			"stopped naming the staging nodes.")
	}
}
