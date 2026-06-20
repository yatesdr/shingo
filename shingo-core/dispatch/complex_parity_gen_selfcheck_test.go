package dispatch

// Self-check for the parity-case generator. Runs without Docker: it asserts the
// generated population resembles the production-grounded weighting (so the fuzz
// harness exercises realistic traffic) and that every case is structurally
// valid (every pickup node appears in the step sequence, the process node is a
// pickup node, multi-pickup shapes really carry two pickups).

import (
	"testing"

	"shingo/protocol"
)

func TestParityGenerator_DistributionAndValidity(t *testing.T) {
	t.Parallel()
	const n = 6000
	cases := generateCases(1, n)
	if len(cases) != n {
		t.Fatalf("generateCases returned %d, want %d", len(cases), n)
	}

	var (
		pk1, pk2      int
		emptyLegs     int
		sameNode      int
		compoundChild int // cases with ParentOrderID != nil (junction suppressed)
		withRejects   int // cases where at least one node leads with a read-reject
		withEmptyAll  int // cases where at least one node has zero candidates
	)

	for _, c := range cases {
		// Structural validity: collect pickup nodes from steps and from the
		// pickups list; they must reference the same node set.
		stepPickupNodes := map[string]int{}
		for _, s := range c.steps {
			if s.Action == protocol.ActionPickup {
				stepPickupNodes[s.Node]++
			}
		}
		if len(stepPickupNodes) == 0 {
			t.Fatalf("%s: no pickup steps", c.name)
		}
		// Every declared pickup node must appear as a pickup step.
		totalPickupSteps := 0
		for _, p := range c.pickups {
			if stepPickupNodes[p.node] == 0 {
				t.Fatalf("%s: pickup node %q not present as a pickup step", c.name, p.node)
			}
			// Count how many pickup steps this materialization feeds.
			totalPickupSteps += stepPickupNodes[p.node]
		}
		// Process node must be one of the pickup nodes.
		if stepPickupNodes[c.processNode] == 0 {
			t.Fatalf("%s: process node %q is not a pickup node", c.name, c.processNode)
		}

		if totalPickupSteps == 1 {
			pk1++
		} else {
			pk2++
		}

		// Same-node double-pick: one materialized node feeding >=2 pickup steps.
		for _, p := range c.pickups {
			if stepPickupNodes[p.node] >= 2 {
				sameNode++
				break
			}
		}

		if c.compoundChild {
			compoundChild++
			if totalPickupSteps < 2 {
				t.Fatalf("%s: compound-child set on a single-pickup shape", c.name)
			}
		}

		for _, p := range c.pickups {
			if p.empty {
				emptyLegs++
				break
			}
		}
		for _, p := range c.pickups {
			if len(p.bins) == 0 {
				withEmptyAll++
				break
			}
			if len(p.bins) > 0 && !p.bins[0].claimable(p.empty) {
				withRejects++
				break
			}
		}
	}

	pk1Pct := pct(pk1, n)
	pk2Pct := pct(pk2, n)
	t.Logf("pk1=%d (%.1f%%) pk2=%d (%.1f%%) emptyLegs=%d sameNode=%d compoundChild=%d leadRejects=%d emptyNodes=%d",
		pk1, pk1Pct, pk2, pk2Pct, emptyLegs, sameNode, compoundChild, withRejects, withEmptyAll)

	// pk1/pk2 should track the ~52/48 production split within a tolerance band.
	if pk1Pct < 44 || pk1Pct > 60 {
		t.Errorf("pk1 share %.1f%% outside expected 44-60%% band", pk1Pct)
	}
	if pk2Pct < 40 || pk2Pct > 56 {
		t.Errorf("pk2 share %.1f%% outside expected 40-56%% band", pk2Pct)
	}
	// The deliberately-kept edge and contention shapes must actually appear.
	if emptyLegs == 0 {
		t.Error("no empty-leg cases generated")
	}
	if sameNode == 0 {
		t.Error("no same-node double-pick cases generated")
	}
	if compoundChild == 0 {
		t.Error("no compound-child cases generated")
	}
	if withRejects == 0 {
		t.Error("no walk-past-reject cases generated")
	}
	if withEmptyAll == 0 {
		t.Error("no empty-node cases generated")
	}
}

func pct(x, total int) float64 { return 100 * float64(x) / float64(total) }
