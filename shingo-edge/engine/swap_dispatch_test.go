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
	// Sequential's A-leg is the REMOVAL: it ends at the outbound destination, not
	// at the process node. DeliveryNodeA is derived from the steps, so it names
	// where the leg actually ends. (The order row still stores "" — AutoConfirmA
	// blanks it in dispatchComplexLeg — but the dispatch must not claim the leg
	// delivers to the press.)
	if d.DeliveryNodeA != "OUT-DEST" {
		t.Errorf("sequential's A-leg is a removal ending at the outbound destination; DeliveryNodeA = %q, want OUT-DEST", d.DeliveryNodeA)
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
	// The single-robot A-leg drops a fresh bin at the press MID-sequence but ENDS
	// at the outbound destination (it carries the spent bin out last). The old
	// assertion of CORE-NODE encoded the bug: it asserted the leg delivers to the
	// press, and wiring_delivered's gate believed it.
	if d.DeliveryNodeA != "OUT-DEST" {
		t.Errorf("single_robot's A-leg ends at the outbound destination; DeliveryNodeA = %q, want OUT-DEST", d.DeliveryNodeA)
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

// TestBuildSwapDispatch_TwoRobotPressIndex_OK is the happy path this mode never
// had — the gap that let the HK 2026-07-14 misbind ship. The R1 (A) leg clears
// the press and then stages a fresh carrier at the PAIRED INDEX node, so it ends
// at CORE-NODE-BACK. DeliveryNodeA must say so: naming the press instead is what
// made wiring_delivered bind an empty tote to the press and drove its tile to 0.
func TestBuildSwapDispatch_TwoRobotPressIndex_OK(t *testing.T) {
	t.Parallel()
	d, err := BuildSwapDispatch(dispatchNode(), dispatchClaim("two_robot_press_index"))
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	if d.CycleMode != "two_robot_press_index" {
		t.Errorf("CycleMode = %q, want two_robot_press_index", d.CycleMode)
	}
	if d.DeliveryNodeA != "CORE-NODE-BACK" {
		t.Errorf("R1 stages the fresh carrier at the paired index node; DeliveryNodeA = %q, want CORE-NODE-BACK (naming the press is the misbind bug)", d.DeliveryNodeA)
	}
	if d.DeliveryNodeA == "CORE-NODE" {
		t.Errorf("DeliveryNodeA must never be the process node for press-index — that is the HK misbind")
	}
	// R2 is the leg that actually puts a bin on the press.
	if got := finalDropoff(d.StepsB); got != "CORE-NODE" {
		t.Errorf("R2 final dropoff = %q, want CORE-NODE (R2 indexes the staged bin onto the press)", got)
	}
	if !d.AutoConfirmB {
		t.Errorf("press-index R2 is auto-confirmed; AutoConfirmB = false, want true")
	}
	if !d.RequiresActiveSwapGuard {
		t.Errorf("press-index must require swap guard")
	}
}

// Three-position index: R1's fresh carrier feeds the SECOND paired node.
func TestBuildSwapDispatch_TwoRobotPressIndex_ThreePosition(t *testing.T) {
	t.Parallel()
	c := dispatchClaim("two_robot_press_index")
	c.SecondPairedCoreNode = "CORE-NODE-C"
	d, err := BuildSwapDispatch(dispatchNode(), c)
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	if d.DeliveryNodeA != "CORE-NODE-C" {
		t.Errorf("3-position R1 ends at the second paired node; DeliveryNodeA = %q, want CORE-NODE-C", d.DeliveryNodeA)
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
