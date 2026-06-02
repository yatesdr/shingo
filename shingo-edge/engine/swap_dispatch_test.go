package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

func dispatchClaim(swapMode protocol.SwapMode) *processes.NodeClaim {
	return &processes.NodeClaim{
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            swapMode,
		PayloadCode:         "WIDGET-A",
		CoreNodeName:        "CORE-NODE",
		InboundSource:       "INBOUND-SRC",
		InboundStaging:      "IN-STAGING",
		OutboundStaging:     "OUT-STAGING",
		OutboundDestination: "OUT-DEST",
		PairedCoreNode:      "CORE-NODE-BACK",
	}
}

func dispatchNode() *processes.Node {
	return &processes.Node{ID: 1, Name: "CORE-NODE"}
}

func TestBuildSwapDispatch_Simple(t *testing.T) {
	t.Parallel()
	// Empty and unrecognized swap modes both pass through to the
	// caller-handled non-complex branch (BuildSwapDispatch returns
	// nil + nil error). The legacy "simple" enum value was removed;
	// the literal "simple" still exercises the same code path that
	// any unknown mode would.
	for _, mode := range []protocol.SwapMode{"", "simple", "unknown_mode"} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			d, err := BuildSwapDispatch(dispatchNode(), dispatchClaim(mode))
			if err != nil {
				t.Fatalf("BuildSwapDispatch(%q): %v", mode, err)
			}
			if d != nil {
				t.Errorf("BuildSwapDispatch(%q) = %+v, want nil (caller handles non-complex modes)", mode, d)
			}
		})
	}
}

// TestBuildSwapDispatch_ProduceMarksInboundEmpty pins the produce-node fix:
// the inbound-source pickup (fetch a fresh carrier from the supermarket) must
// be flagged Empty so Core sources an empty to fill — and ONLY that leg. The
// CoreNode pickup that removes the produced full must stay full. Covers the
// multi-step modes that emit complex orders (press-index here).
func TestBuildSwapDispatch_ProduceMarksInboundEmpty(t *testing.T) {
	t.Parallel()
	d, err := BuildSwapDispatch(dispatchNode(), dispatchClaim("two_robot_press_index"))
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	inboundEmpty := 0
	for _, steps := range [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB} {
		for _, s := range steps {
			if !s.Empty {
				continue
			}
			if s.Action == "pickup" && s.Node == "INBOUND-SRC" {
				inboundEmpty++
			} else {
				t.Errorf("non-inbound step flagged empty: %+v (only the InboundSource pickup should be empty)", s)
			}
		}
	}
	if inboundEmpty != 1 {
		t.Errorf("InboundSource pickup empty-flag count = %d, want exactly 1", inboundEmpty)
	}
}

// TestBuildSwapDispatch_ConsumeLeavesFullRetrieve is the dual: a consume node's
// inbound pickup fetches a FULL (to consume), so no leg may be flagged empty.
func TestBuildSwapDispatch_ConsumeLeavesFullRetrieve(t *testing.T) {
	t.Parallel()
	claim := dispatchClaim("two_robot_press_index")
	claim.Role = protocol.ClaimRoleConsume
	d, err := BuildSwapDispatch(dispatchNode(), claim)
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	for _, steps := range [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB} {
		for _, s := range steps {
			if s.Empty {
				t.Errorf("consume claim must not flag any empty leg; got %+v", s)
			}
		}
	}
}

func TestBuildSwapDispatch_Sequential(t *testing.T) {
	t.Parallel()
	d, err := BuildSwapDispatch(dispatchNode(), dispatchClaim("sequential"))
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	if d.CycleMode != "sequential" {
		t.Errorf("CycleMode = %q, want sequential", d.CycleMode)
	}
	if !d.AutoConfirmA {
		t.Errorf("sequential's removal order is auto-confirmed; AutoConfirmA = false, want true")
	}
	if d.StepsB != nil {
		t.Errorf("sequential is single-order; StepsB should be nil")
	}
	if d.RequiresActiveSwapGuard {
		t.Errorf("sequential should not require swap guard")
	}
	if d.DeliveryNodeA != "" {
		t.Errorf("sequential's DeliveryNodeA should be empty (Core resolves from steps); got %q", d.DeliveryNodeA)
	}
}

func TestBuildSwapDispatch_SingleRobot_OK(t *testing.T) {
	t.Parallel()
	d, err := BuildSwapDispatch(dispatchNode(), dispatchClaim("single_robot"))
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	if d.CycleMode != "single_robot" {
		t.Errorf("CycleMode = %q, want single_robot", d.CycleMode)
	}
	if d.DeliveryNodeA != "CORE-NODE" {
		t.Errorf("DeliveryNodeA = %q, want CORE-NODE", d.DeliveryNodeA)
	}
	if d.StepsB != nil {
		t.Errorf("single_robot is single-order; StepsB should be nil")
	}
	if d.RequiresActiveSwapGuard {
		t.Errorf("single_robot should not require swap guard")
	}
}

func TestBuildSwapDispatch_SingleRobot_MissingStaging(t *testing.T) {
	t.Parallel()
	c := dispatchClaim("single_robot")
	c.InboundStaging = ""
	if _, err := BuildSwapDispatch(dispatchNode(), c); err == nil {
		t.Fatalf("expected error for missing inbound staging")
	}
	c = dispatchClaim("single_robot")
	c.OutboundStaging = ""
	if _, err := BuildSwapDispatch(dispatchNode(), c); err == nil {
		t.Fatalf("expected error for missing outbound staging")
	}
}

func TestBuildSwapDispatch_TwoRobot_OK(t *testing.T) {
	t.Parallel()
	d, err := BuildSwapDispatch(dispatchNode(), dispatchClaim("two_robot"))
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	if d.CycleMode != "two_robot" {
		t.Errorf("CycleMode = %q, want two_robot", d.CycleMode)
	}
	if d.DeliveryNodeA != "CORE-NODE" {
		t.Errorf("DeliveryNodeA = %q, want CORE-NODE", d.DeliveryNodeA)
	}
	if d.StepsA == nil || d.StepsB == nil {
		t.Errorf("two_robot must produce both step lists")
	}
	if !d.AutoConfirmB {
		t.Errorf("two_robot's removal (B) order is auto-confirmed; AutoConfirmB = false, want true")
	}
	if !d.RequiresActiveSwapGuard {
		t.Errorf("two_robot must require swap guard")
	}
}

func TestBuildSwapDispatch_TwoRobot_MissingStaging(t *testing.T) {
	t.Parallel()
	c := dispatchClaim("two_robot")
	c.InboundStaging = ""
	if _, err := BuildSwapDispatch(dispatchNode(), c); err == nil {
		t.Fatalf("expected error for missing inbound staging")
	}
}

func TestBuildSwapDispatch_TwoRobotPressIndex_MissingFields(t *testing.T) {
	t.Parallel()
	c := dispatchClaim("two_robot_press_index")
	c.PairedCoreNode = ""
	if _, err := BuildSwapDispatch(dispatchNode(), c); err == nil {
		t.Fatalf("expected error for missing paired_core_node")
	}
	c = dispatchClaim("two_robot_press_index")
	c.OutboundDestination = ""
	if _, err := BuildSwapDispatch(dispatchNode(), c); err == nil {
		t.Fatalf("expected error for missing outbound_destination")
	}
}
