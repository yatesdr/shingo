package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

func consumeClaim(swapMode string) *processes.NodeClaim {
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

func consumeFixtures(swapMode string) (*processes.Node, *processes.RuntimeState, *processes.NodeClaim) {
	node := &processes.Node{ID: 2, Name: "CONSUME-NODE"}
	runtime := &processes.RuntimeState{RemainingUOP: 0}
	return node, runtime, consumeClaim(swapMode)
}

func TestBuildConsumePlan_SimpleMode(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("simple")

	plan, err := BuildConsumePlan(node, runtime, claim, 1, true, true)
	if err != nil {
		t.Fatalf("BuildConsumePlan: %v", err)
	}
	if !plan.SimpleMove {
		t.Errorf("SimpleMove = false, want true for simple mode")
	}
	if plan.CycleMode() != "simple" {
		t.Errorf("CycleMode = %q, want simple", plan.CycleMode())
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

	plan, err := BuildConsumePlan(node, runtime, claim, 2, false /*nodeOccupied*/, false)
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

func TestBuildConsumePlan_TwoRobot_Occupied(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("two_robot")

	plan, err := BuildConsumePlan(node, runtime, claim, 1, true, false)
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
	if plan.CycleMode() != "two_robot" {
		t.Errorf("CycleMode = %q, want two_robot", plan.CycleMode())
	}
}

func TestBuildConsumePlan_Sequential(t *testing.T) {
	t.Parallel()
	node, runtime, claim := consumeFixtures("sequential")

	plan, err := BuildConsumePlan(node, runtime, claim, 1, true, false)
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

	plan, err := BuildConsumePlan(node, runtime, claim, 0, true, false)
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
		if _, err := BuildConsumePlan(node, runtime, nil, 1, true, false); err == nil {
			t.Fatalf("expected error for nil claim")
		}
	})

	t.Run("wrong_role", func(t *testing.T) {
		c := *claim
		c.Role = protocol.ClaimRoleProduce
		if _, err := BuildConsumePlan(node, runtime, &c, 1, true, false); err == nil {
			t.Fatalf("expected error for non-consume role")
		}
	})

	t.Run("simple_missing_inbound_source", func(t *testing.T) {
		c := *claim
		c.InboundSource = ""
		if _, err := BuildConsumePlan(node, runtime, &c, 1, true, false); err == nil {
			t.Fatalf("expected error for missing inbound_source on simple mode")
		}
	})

	t.Run("downgrade_missing_inbound_source", func(t *testing.T) {
		c := *claim
		c.SwapMode = "two_robot"
		c.InboundSource = ""
		if _, err := BuildConsumePlan(node, runtime, &c, 1, false /*empty*/, false); err == nil {
			t.Fatalf("expected error for missing inbound_source on node-empty downgrade")
		}
	})
}
