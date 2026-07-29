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

// TestBuildSwapDispatch_ProduceMarksInboundEmpty pins the produce-node empty
// flagging on the press-index dispatch. TWO kinds of pickup are empty: the
// InboundSource pickup (fetch a fresh carrier from the supermarket) and the
// index leg's on-deck pickups (hop A3: fetch the on-deck empty regardless of any
// stamped part). The CoreNode pickup that removes the produced FULL off the
// press must stay full — that is the invariant this test has always protected.
func TestBuildSwapDispatch_ProduceMarksInboundEmpty(t *testing.T) {
	t.Parallel()
	d, err := BuildSwapDispatch(dispatchNode(), dispatchClaim("two_robot_press_index"))
	if err != nil {
		t.Fatalf("BuildSwapDispatch: %v", err)
	}
	emptyNodes := map[string]bool{}
	for _, steps := range [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB} {
		for _, s := range steps {
			if s.Action != "pickup" {
				continue
			}
			if s.Node == "CORE-NODE" && s.Empty {
				t.Errorf("the CoreNode removal pickup must stay full (it lifts the produced full off the press): %+v", s)
			}
			if s.Empty {
				emptyNodes[s.Node] = true
			}
		}
	}
	if !emptyNodes["INBOUND-SRC"] {
		t.Error("InboundSource pickup must be flagged Empty (fetch a fresh carrier to fill)")
	}
	if !emptyNodes["CORE-NODE-BACK"] {
		t.Error("index-leg on-deck pickup (CORE-NODE-BACK) must be flagged Empty (hop A3 — fetch the on-deck empty regardless of stamp)")
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
	// R2 is still the leg that puts a bin on the press — but in 3-position it
	// carries on afterwards to re-index C into B, so it ENDS at the index node.
	// Its final dropoff therefore says "index", while the bin it supplied is
	// sitting on the press. That gap is exactly why the supply/evac classifier
	// asks legPlacesBinAt and not "where did the leg end".
	if got := finalDropoff(d.StepsB); got != "CORE-NODE-BACK" {
		t.Errorf("3-position R2 ends at the paired index node; finalDropoff(StepsB) = %q, want CORE-NODE-BACK", got)
	}
	if !legPlacesBinAt(d.StepsB, "CORE-NODE") {
		t.Errorf("3-position R2 must still PLACE a bin on the press — it drops one there mid-sequence and never picks it back up")
	}
	if legPlacesBinAt(d.StepsA, "CORE-NODE") {
		t.Errorf("3-position R1 lifts the spent bin OFF the press and never replaces it — it must not read as the supply leg")
	}
	if !d.AutoConfirmB {
		t.Errorf("press-index R2 is auto-confirmed; AutoConfirmB = false, want true")
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

// TestBuildSwapDispatch_TwoLegModes_RejectBlankOutboundDestination pins the
// plan-time half of the pairing invariant, over EVERY two-leg mode rather than
// at one point, because the two modes disagreed about this and nothing noticed.
//
// The rule: a mode that emits two legs must validate every field BOTH legs need
// before it returns a dispatch at all. The apply step creates leg A, ships it to
// Core, and only then builds leg B; there is no rollback and none is available,
// because leg A is already committed at Core behind a durable outbox. So a field
// only leg B reads has to be checked HERE — the last place where refusing costs
// nothing — or its absence surfaces as a leg A that Core has accepted and can
// never pair.
//
// OutboundDestination is exactly such a field. Both two-leg builders route their
// evac leg to it via buildStep, which emits a dropoff with NO NODE when it is
// blank (material_orders.go). Core cannot reject that: a node-less dropoff is
// the legitimate deferred-destination form reserved for dedicated-loader
// placement (complex_steps.go, resolveComplexSteps). So a blank here is
// indistinguishable at Core from a deliberate deferral, and the check cannot be
// made downstream.
//
// SPRINGFIELD, 2026-06-23 17:34:38. Edge order 2207 (the supply leg) reached
// Core as order 2217 and lives there today with sibling_order_uuid=”. Its evac
// sibling, Edge order 2208, was built with steps
// [wait ALN_001, pickup ALN_001, dropoff <no node>] — a blank
// OutboundDestination on the ALN_001 claim — and has no Core row at all. Edge's
// sibling_order_id says the pair is intact; Core says the supply is a single
// leg. Nothing reconciles the two. Post-cutover that is 2 of 3,162 Core orders,
// and those 2 are the whole population of Core-side orphans: every other
// unpaired complex order in the dump is single-leg by design.
//
// Scoped to the two-leg modes deliberately. sequential and single_robot route to
// OutboundDestination through the same buildStep and will emit the same
// node-less dropoff, but they emit ONE order, so a blank there strands nothing
// and pairs nothing — a different defect with a different fix, and widening this
// test to cover it would assert a rule this change does not make true.
func TestBuildSwapDispatch_TwoLegModes_RejectBlankOutboundDestination(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{
		protocol.SwapModeTwoRobot,
		protocol.SwapModeTwoRobotPressIndex,
	} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			// A blank OutboundDestination must be refused outright. Returning a
			// dispatch with a usable leg A and a malformed leg B is the failure
			// this exists to prevent: the caller has no way to decline half of it.
			c := dispatchClaim(mode)
			c.OutboundDestination = ""
			d, err := BuildSwapDispatch(dispatchNode(), c)
			if err == nil {
				t.Fatalf("blank outbound_destination accepted; the evac leg would carry a node-less dropoff and orphan leg A at Core")
			}
			if d != nil {
				t.Errorf("BuildSwapDispatch returned a dispatch alongside its error (%+v); a rejected plan must yield nothing to dispatch", d)
			}

			// And the property the error stands in for: whatever a validated
			// dispatch does return, no leg of it drops a bin nowhere. Asserted on
			// the accepted case so the check cannot go vacuous the day the error
			// message or the blank-field spelling changes.
			ok, err := BuildSwapDispatch(dispatchNode(), dispatchClaim(mode))
			if err != nil {
				t.Fatalf("fully-configured claim rejected: %v", err)
			}
			for name, steps := range map[string][]protocol.ComplexOrderStep{
				"StepsA": ok.StepsA, "StepsB": ok.StepsB,
			} {
				for i, s := range steps {
					if s.Action == protocol.ActionDropoff && s.Node == "" {
						t.Errorf("%s[%d] is a dropoff with no node; Core reads that as a deferred dedicated-loader destination, not as a misconfiguration", name, i)
					}
				}
			}
		})
	}
}
