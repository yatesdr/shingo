package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// fixedNow is the wall clock every produce/consume planner test uses so
// timestamp comparisons are deterministic.
var fixedNow = time.Date(2026, 4, 29, 14, 0, 0, 0, time.UTC)

func produceClaim(swapMode string) *processes.NodeClaim {
	return &processes.NodeClaim{
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            swapMode,
		PayloadCode:         "WIDGET-A",
		CoreNodeName:        "PRODUCE-NODE",
		InboundSource:       "EMPTY-STORAGE",
		InboundStaging:      "PRODUCE-IN-STAGING",
		OutboundStaging:     "PRODUCE-OUT-STAGING",
		OutboundDestination: "FILLED-STORAGE",
		PairedCoreNode:      "PRODUCE-NODE-BACK",
	}
}

func produceFixtures(swapMode string) (*processes.Node, *processes.RuntimeState, *processes.NodeClaim) {
	node := &processes.Node{ID: 1, Name: "PRODUCE-NODE"}
	runtime := &processes.RuntimeState{RemainingUOP: 50}
	return node, runtime, produceClaim(swapMode)
}

func TestBuildProducePlan_SimpleMode(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("")

	plan, err := BuildProducePlan(node, runtime, claim, true, fixedNow)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if !plan.SimpleOnly {
		t.Errorf("SimpleOnly = false, want true for simple mode")
	}
	if plan.CycleMode() != "simple" {
		t.Errorf("CycleMode = %q, want simple", plan.CycleMode())
	}
	if plan.Dispatch != nil {
		t.Errorf("simple mode should have no Dispatch; got %+v", plan.Dispatch)
	}
	if len(plan.Manifest) != 1 || plan.Manifest[0].Quantity != 50 || plan.Manifest[0].PartNumber != "WIDGET-A" {
		t.Errorf("manifest mismatch: %+v", plan.Manifest)
	}
	if plan.ProducedAtRFC3339 != "2026-04-29T14:00:00Z" {
		t.Errorf("ProducedAt = %q, want fixed timestamp", plan.ProducedAtRFC3339)
	}
}

func TestBuildProducePlan_Sequential(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("sequential")

	plan, err := BuildProducePlan(node, runtime, claim, false, fixedNow)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.CycleMode() != "sequential" {
		t.Errorf("CycleMode = %q, want sequential", plan.CycleMode())
	}
	if plan.SimpleOnly {
		t.Errorf("sequential should not be SimpleOnly")
	}
	if plan.Dispatch == nil {
		t.Fatalf("sequential must have a Dispatch")
	}
	if !plan.Dispatch.AutoConfirmA {
		t.Errorf("sequential's removal order is dispatched via the auto-confirm path; AutoConfirmA = false, want true")
	}
	if plan.Dispatch.StepsB != nil {
		t.Errorf("sequential is single-order; StepsB should be nil")
	}
	if plan.Dispatch.RequiresActiveSwapGuard {
		t.Errorf("sequential should not require swap guard (backfill is auto-created on transit)")
	}
}

func TestBuildProducePlan_TwoRobotPressIndex_OK(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("two_robot_press_index")

	plan, err := BuildProducePlan(node, runtime, claim, true, fixedNow)
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.CycleMode() != "two_robot_press_index" {
		t.Errorf("CycleMode = %q, want two_robot_press_index", plan.CycleMode())
	}
	if plan.Dispatch == nil || plan.Dispatch.StepsA == nil || plan.Dispatch.StepsB == nil {
		t.Errorf("two_robot_press_index must produce both R1 and R2 steps via Dispatch")
	}
	if plan.Dispatch != nil && !plan.Dispatch.RequiresActiveSwapGuard {
		t.Errorf("two_robot_press_index must require swap guard")
	}
}

func TestBuildProducePlan_PreconditionErrors(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("simple")

	t.Run("nil_claim", func(t *testing.T) {
		if _, err := BuildProducePlan(node, runtime, nil, true, fixedNow); err == nil {
			t.Fatalf("expected error for nil claim")
		}
	})

	t.Run("wrong_role", func(t *testing.T) {
		c := *claim
		c.Role = protocol.ClaimRoleConsume
		if _, err := BuildProducePlan(node, runtime, &c, true, fixedNow); err == nil {
			t.Fatalf("expected error for non-produce role")
		}
	})

	t.Run("zero_uop", func(t *testing.T) {
		r := *runtime
		r.RemainingUOP = 0
		if _, err := BuildProducePlan(node, &r, claim, true, fixedNow); err == nil {
			t.Fatalf("expected error for zero RemainingUOP")
		}
	})
}
