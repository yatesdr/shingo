package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

func consumeClaim(swapMode protocol.SwapMode) *processes.NodeClaim {
	return &processes.NodeClaim{
		Role:                protocol.ClaimRoleConsume,
		SwapMode:            swapMode,
		PayloadCode:         "WIDGET-A",
		CoreNodeName:        "CONSUME-NODE",
		InboundSource:       "FILLED-STORAGE",
		InboundStaging:      "CONSUME-IN-STAGING",
		OutboundStaging:     "CONSUME-OUT-STAGING",
		OutboundDestination: "EMPTY-STORAGE",
		PairedCoreNode:      "CONSUME-NODE-BACK",
		AutoConfirm:         false,
	}
}

func consumeFixtures(swapMode protocol.SwapMode) (*processes.Node, *processes.RuntimeState, *processes.NodeClaim) {
	node := &processes.Node{ID: 2, Name: "CONSUME-NODE"}
	runtime := &processes.RuntimeState{RemainingUOPCached: 0}
	return node, runtime, consumeClaim(swapMode)
}

// occMap builds a CoreNodeName→occupied map for tests. Pass occupied=true
// for the head-only "node has a bin" case, false for the empty-trigger
// case. Paired-node entries default to occupied=true (the
// missing-means-occupied semantics in isOccupied), so a test that only
// passes the head value gets the partial-empty matrix.
func occMap(head string, occupied bool) map[string]bool {
	return map[string]bool{head: occupied}
}

func TestBuildConsumePlan_SimpleMode(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("simple")

	plan, err := BuildConsumePlan(node, runtime, claim, 1, occMap(claim.CoreNodeName, true), true)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if !plan.SimpleMove {
		t.Errorf("SimpleMove = false, want true for simple mode")
	}
	if plan.Dispatch != nil {
		t.Errorf("simple mode should have no Dispatch; got %+v", plan.Dispatch)
	}
	if plan.SimpleSource != "FILLED-STORAGE" || plan.SimpleDest != "CONSUME-NODE" {
		t.Errorf("simple move endpoints = %q→%q, want FILLED-STORAGE→CONSUME-NODE", plan.SimpleSource, plan.SimpleDest)
	}
	if plan.DowngradedFromSwapMode != "" {
		t.Errorf("DowngradedFromSwapMode should be empty for native simple mode; got %q", plan.DowngradedFromSwapMode)
	}
	if !plan.AutoConfirm {
		t.Errorf("AutoConfirm = false, want true (passed in)")
	}
}

func TestBuildConsumePlan_NodeEmptyDowngrade(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("two_robot")

	plan, err := BuildConsumePlan(node, runtime, claim, 2, occMap(claim.CoreNodeName, false), false)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if !plan.SimpleMove {
		t.Errorf("empty-node should downgrade to SimpleMove")
	}
	if plan.DowngradedFromSwapMode != "two_robot" {
		t.Errorf("DowngradedFromSwapMode = %q, want two_robot", plan.DowngradedFromSwapMode)
	}
	if plan.Dispatch != nil {
		t.Errorf("downgrade should have no Dispatch; got %+v", plan.Dispatch)
	}
	if plan.Quantity != 2 {
		t.Errorf("Quantity = %d, want 2 (passed through)", plan.Quantity)
	}
}

// TestBuildConsumePlan_PressIndexAllEmptyPrimes pins the empty-station
// prime-fill: a 3-position press_index claim with A, B, C all empty
// downgrades to 3 simple deliveries (head + 2 primes), not 1. Regression
// guard for the cold-start case where the operator's single click used
// to leave B and C empty and the next swap stalled on R2's pickup(B).
func TestBuildConsumePlan_PressIndexAllEmptyPrimes(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures(protocol.SwapModeTwoRobotPressIndex)
	claim.SecondPairedCoreNode = "CONSUME-NODE-BACK-2"
	claim.OutboundDestination = "EMPTY-STORAGE"

	occ := map[string]bool{
		claim.CoreNodeName:         false,
		claim.PairedCoreNode:       false,
		claim.SecondPairedCoreNode: false,
	}
	plan, err := BuildConsumePlan(node, runtime, claim, 1, occ, false)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if !plan.SimpleMove || plan.SimpleDest != claim.CoreNodeName {
		t.Errorf("head simple-move missing: SimpleMove=%v dest=%q", plan.SimpleMove, plan.SimpleDest)
	}
	if plan.DowngradedFromSwapMode != protocol.SwapModeTwoRobotPressIndex {
		t.Errorf("DowngradedFromSwapMode = %q, want two_robot_press_index", plan.DowngradedFromSwapMode)
	}
	if len(plan.PrimePairedPositions) != 2 {
		t.Fatalf("PrimePairedPositions = %d entries, want 2 (B and C)", len(plan.PrimePairedPositions))
	}
	wantDests := map[string]bool{claim.PairedCoreNode: true, claim.SecondPairedCoreNode: true}
	for _, p := range plan.PrimePairedPositions {
		if p.Source != claim.InboundSource {
			t.Errorf("prime to %s: source = %q, want %q", p.Dest, p.Source, claim.InboundSource)
		}
		if !wantDests[p.Dest] {
			t.Errorf("unexpected prime dest %q", p.Dest)
		}
		delete(wantDests, p.Dest)
	}
	if len(wantDests) != 0 {
		t.Errorf("missing primes for %v", wantDests)
	}
}

// TestBuildConsumePlan_PressIndexHeadEmptyPairedFull verifies the
// partial-empty branch: head empty but paired positions are occupied →
// existing 1-move downgrade, no primes. Guards against accidentally
// priming nodes that already hold bins.
func TestBuildConsumePlan_PressIndexHeadEmptyPairedFull(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures(protocol.SwapModeTwoRobotPressIndex)
	claim.SecondPairedCoreNode = "CONSUME-NODE-BACK-2"
	claim.OutboundDestination = "EMPTY-STORAGE"

	occ := map[string]bool{
		claim.CoreNodeName:         false,
		claim.PairedCoreNode:       true,
		claim.SecondPairedCoreNode: true,
	}
	plan, err := BuildConsumePlan(node, runtime, claim, 1, occ, false)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if !plan.SimpleMove {
		t.Fatalf("expected SimpleMove downgrade")
	}
	if len(plan.PrimePairedPositions) != 0 {
		t.Errorf("expected no primes when paired positions are occupied; got %+v", plan.PrimePairedPositions)
	}
}

func TestBuildConsumePlan_TwoRobot_Occupied(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("two_robot")

	plan, err := BuildConsumePlan(node, runtime, claim, 1, occMap(claim.CoreNodeName, true), false)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if plan.SimpleMove {
		t.Errorf("occupied two_robot should not be SimpleMove")
	}
	if plan.Dispatch == nil {
		t.Fatalf("two_robot must have a Dispatch")
	}
	if !plan.Dispatch.RequiresActiveSwapGuard {
		t.Errorf("two_robot must require swap guard")
	}
}

func TestBuildConsumePlan_Sequential(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("sequential")

	plan, err := BuildConsumePlan(node, runtime, claim, 1, occMap(claim.CoreNodeName, true), false)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if plan.Dispatch == nil || plan.Dispatch.CycleMode != "sequential" {
		t.Errorf("expected sequential dispatch; got %+v", plan.Dispatch)
	}
}

func TestBuildConsumePlan_QuantityFloor(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("simple")

	plan, err := BuildConsumePlan(node, runtime, claim, 0, occMap(claim.CoreNodeName, true), false)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if plan.Quantity != 1 {
		t.Errorf("Quantity = %d, want 1 (floor of qty<1 is 1)", plan.Quantity)
	}
}

func TestBuildConsumePlan_PreconditionErrors(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("simple")

	t.Run("nil_claim", func(t *testing.T) {
		if _, err := BuildConsumePlan(node, runtime, nil, 1, occMap(claim.CoreNodeName, true), false); err == nil {
			t.Fatalf("expected error for nil claim")
		}
	})

	t.Run("wrong_role", func(t *testing.T) {
		c := *claim
		c.Role = protocol.ClaimRoleProduce
		if _, err := BuildConsumePlan(node, runtime, &c, 1, occMap(claim.CoreNodeName, true), false); err == nil {
			t.Fatalf("expected error for non-consume role")
		}
	})

	t.Run("simple_missing_inbound_source", func(t *testing.T) {
		c := *claim
		c.InboundSource = ""
		if _, err := BuildConsumePlan(node, runtime, &c, 1, occMap(claim.CoreNodeName, true), false); err == nil {
			t.Fatalf("expected error for missing inbound_source on simple mode")
		}
	})

	t.Run("downgrade_missing_inbound_source", func(t *testing.T) {
		c := *claim
		c.SwapMode = "two_robot"
		c.InboundSource = ""
		if _, err := BuildConsumePlan(node, runtime, &c, 1, occMap(claim.CoreNodeName, false), false); err == nil {
			t.Fatalf("expected error for missing inbound_source on node-empty downgrade")
		}
	})
}
