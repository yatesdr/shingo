package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// TOOLING IS A DECORATOR, NOT A COMPETITOR.
//
// A marked press ALWAYS gets tooling treatment: its marked seats' bins go to
// the tooling bay, and every bin coming back to the press waits at inbound
// staging until the operator says the tool change is done. Bin type never
// disqualifies it, and neither does the incoming style running on different
// nodes.
//
// The three shapes below are the three ways a changeover can present that
// rule. Before the decorator, the first two produced NO tooling behaviour at
// all — the same-bin-type case was the only one that worked, because it was
// the only one whose diff still carried two_robot_press_index by the time the
// staged fan-out's predicate looked at it.
// ---------------------------------------------------------------------------

// stepString renders one order's steps as "pickup@A,dropoff@B" so a shape
// mismatch reads as a diff rather than as a struct dump.
func stepString(spec *changeover.OrderSpec) string {
	if spec == nil {
		return "<nil>"
	}
	if spec.Retrieve != nil {
		return fmt.Sprintf("retrieve(source=%s,delivery=%s)", spec.Retrieve.SourceNode, spec.Retrieve.DeliveryNode)
	}
	if spec.Complex == nil {
		return "<empty>"
	}
	parts := make([]string, 0, len(spec.Complex.Steps))
	for _, s := range spec.Complex.Steps {
		p := s.Action + "@" + s.Node
		if s.Action == "wait" && s.Node == "" {
			p = "wait@<bare>"
		}
		if s.Empty {
			p += "(empty)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}

// actionFor finds the planned action for a core node.
func actionFor(t *testing.T, p changeover.Plan, coreNode string) changeover.NodeAction {
	t.Helper()
	for _, a := range p.Actions {
		if a.CoreNodeName == coreNode {
			return a
		}
	}
	var have []string
	for _, a := range p.Actions {
		have = append(have, a.CoreNodeName)
	}
	t.Fatalf("no action planned for %s — plan covers %s", coreNode, strings.Join(have, ","))
	return changeover.NodeAction{}
}

// allSteps is every step of both of an action's orders, for the "does this
// shape mention X anywhere" assertions.
func allSteps(a changeover.NodeAction) string {
	return stepString(a.SupplyOrder) + " | " + stepString(a.EvacOrder)
}

// toolingTestPlan runs the changeover pipeline exactly as planChangeover does —
// every diff post-processor, then the plan build — and returns the finished
// plan. This is the seam the decorator has to work at: whatever the pipeline
// produced, tooling edits it.
func toolingTestPlan(t *testing.T, from, to []processes.NodeClaim, binTypes map[string]string, nodeNames ...string) changeover.Plan {
	t.Helper()
	diffs := DiffStyleClaims(from, to)
	diffs = FanOutPressIndexDifferentBinType(diffs, binTypes)
	diffs = FanOutPressIndexCrossMode(diffs, binTypes)

	nodes := make([]processes.Node, 0, len(nodeNames))
	for i, n := range nodeNames {
		nodes = append(nodes, processes.Node{ID: int64(i + 1), CoreNodeName: n, Name: n})
	}
	return BuildChangeoverPlan(diffs, nodes, false, nil, planToolingChangeover(from, to))
}

// pressClaim builds a press-index claim. seats/evacDest belong on the OUTGOING
// claim; inboundStaging on the INCOMING one.
func pressClaim(node, paired, payload string) processes.NodeClaim {
	return processes.NodeClaim{
		CoreNodeName:        node,
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:         payload,
		PairedCoreNode:      paired,
		OutboundDestination: "MARKET",
		InboundSource:       "EMPTIES",
	}
}

// ---------------------------------------------------------------------------
// SHAPE 1 — marked + same node + different bin type. THE N1 CASE.
// ---------------------------------------------------------------------------

// TestToolingBeatsRelabel is the contract in one sentence: a marked seat gets
// tooling treatment even when the changeover also changes bin type.
//
// The bin-type fan-out rewrites the diff's SwapMode to press_position, and the
// staged fan-out's predicate required two_robot_press_index — so the press that
// most needs a tool change (its carriers are physically different) was the one
// shape that silently got none. Marked beats relabel.
func TestToolingBeatsRelabel(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacSeats = []string{domain.EvacSeatFront}
	from.ChangeoverEvacDestination = "TOOLING-BAY"

	to := pressClaim("PRESS", "PRESS_B", "PART-B")
	to.InboundStaging = "IN-STAGE"

	binTypes := map[string]string{"PART-A": "BIG", "PART-B": "SMALL"}
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		binTypes, "PRESS", "PRESS_B")

	// The MARKED seat: its bin goes to the tooling bay, and its replacement
	// holds at staging until tooling-done.
	marked := actionFor(t, plan, "PRESS")
	got := allSteps(marked)
	if !strings.Contains(got, "dropoff@TOOLING-BAY") {
		t.Errorf("marked seat PRESS does not route its outgoing bin to the tooling bay.\n"+
			"steps = %s\n"+
			"A marked seat ALWAYS evacuates to ChangeoverEvacDestination. Bin type does not "+
			"disqualify it — that the carriers differ is a reason the tool change is happening, "+
			"not a reason to skip it.", got)
	}
	if !strings.Contains(got, "wait@IN-STAGE") {
		t.Errorf("marked seat PRESS does not hold its incoming bin at inbound staging.\n"+
			"steps = %s\n"+
			"Tooling is literally just adding a staging step for inbound: the new bin waits "+
			"out of the cell while a human is inside it.", got)
	}

	// The UNMARKED seat on the SAME press still gets its ordinary bin-type
	// swap — its bin is not in the way, so it does not go to the bay.
	unmarked := actionFor(t, plan, "PRESS_B")
	ugot := allSteps(unmarked)
	if strings.Contains(ugot, "TOOLING-BAY") {
		t.Errorf("unmarked seat PRESS_B was routed to the tooling bay.\n"+
			"steps = %s\n"+
			"Only MARKED seats evacuate for tooling; an unmarked seat keeps whatever the "+
			"normal machinery gives it.", ugot)
	}
	// ...but it is still a bin coming back to a press that is down for a tool
	// change, so it waits with the rest.
	if !strings.Contains(ugot, "wait@IN-STAGE") {
		t.Errorf("unmarked seat PRESS_B delivers into a press mid tool-change.\n"+
			"steps = %s\n"+
			"EVERY inbound leg to the press holds — the press is down, marked seat or not.", ugot)
	}
}

// ---------------------------------------------------------------------------
// SHAPE 2 — marked + DISJOINT nodes. The incoming style runs elsewhere.
// ---------------------------------------------------------------------------

// TestToolingAcrossDisjointNodes pins the owner's second shape: style 1 on
// PLN_001/PLN_002, style 2 on PLN_005/PLN_006. No node is in both styles, so
// the marked seats are Drops and the new seats are Adds, and the old staged
// fan-out — which required FromClaim AND ToClaim on one diff — contributed
// nothing at all: the tooling bins went to the ordinary evac destination and
// the new material was delivered straight to lineside with no hold.
//
// THIS IS ALSO THE CROSS-MODE PIN. DiffStyleClaims produces only PLN_001(drop)
// and PLN_005(add) — the two claim ROWS. The extension seats PLN_002(drop) and
// PLN_006(add) exist solely because FanOutPressIndexCrossMode synthesizes them,
// verified by tracing the passes. So the assertions below on PLN_002 and
// PLN_006 are assertions that the decorator correctly edits legs planned from
// cross-mode's synthesized diffs — the one ordering interaction the two passes
// have, and it is safe by construction: cross-mode makes diffs, the decorator
// edits the plan those diffs produced, so neither can preempt the other.
func TestToolingAcrossDisjointNodes(t *testing.T) {
	t.Parallel()
	from := pressClaim("PLN_001", "PLN_002", "PART-A")
	from.ChangeoverEvacSeats = []string{domain.EvacSeatFront, domain.EvacSeatPaired}
	from.ChangeoverEvacDestination = "TOOLING-BAY"

	to := pressClaim("PLN_005", "PLN_006", "PART-A")
	to.InboundStaging = "IN-STAGE"

	binTypes := map[string]string{"PART-A": "BIG"}
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		binTypes, "PLN_001", "PLN_002", "PLN_005", "PLN_006")

	// Both marked seats go to the tooling bay, not to MARKET.
	for _, seat := range []string{"PLN_001", "PLN_002"} {
		a := actionFor(t, plan, seat)
		got := allSteps(a)
		if !strings.Contains(got, "dropoff@TOOLING-BAY") {
			t.Errorf("marked seat %s does not evacuate to the tooling bay.\nsteps = %s\n"+
				"The rule holds across every changeover shape — the incoming style running on "+
				"different nodes does not make these bins any less in the way.", seat, got)
		}
		if strings.Contains(got, "dropoff@MARKET") {
			t.Errorf("marked seat %s still evacuates to the ordinary destination.\nsteps = %s",
				seat, got)
		}
	}

	// Every new seat stages and holds behind tooling-done.
	for _, seat := range []string{"PLN_005", "PLN_006"} {
		a := actionFor(t, plan, seat)
		got := allSteps(a)
		if !strings.Contains(got, "wait@IN-STAGE") {
			t.Errorf("new seat %s takes delivery with no tooling hold.\nsteps = %s\n"+
				"A human is inside the cell. Material must wait at inbound staging and move in "+
				"on the operator's release, not drive straight to lineside.", seat, got)
		}
	}
}

// ---------------------------------------------------------------------------
// SHAPE 3 — REGRESSION PIN. Marked + same node + SAME bin type.
// ---------------------------------------------------------------------------

// TestToolingSameBinTypeKeepsTheProvenShape is the pin on the one shape that
// already worked. The sim proved it end to end on 2026-08-24:
//
//	pickup@SEAT -> dropoff@EVAC-DEST -> pickup@SRC -> wait@STAGE -> dropoff@SEAT
//
// The decorator refactor must not change it. This is also the case that proves
// per-seat granularity is not optional: the whole-cell press-index builder
// evacuates only the FRONT position and INDEXES the paired bin forward onto it,
// which is exactly the motion a tool change forbids. So a marked press has to
// reach one action per marked seat however it got here.
func TestToolingSameBinTypeKeepsTheProvenShape(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacSeats = []string{domain.EvacSeatFront, domain.EvacSeatPaired}
	from.ChangeoverEvacDestination = "TOOLING-BAY"

	to := pressClaim("PRESS", "PRESS_B", "PART-A")
	to.InboundStaging = "IN-STAGE"

	binTypes := map[string]string{"PART-A": "BIG"}
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		binTypes, "PRESS", "PRESS_B")

	want := map[string]string{
		"PRESS":   "pickup@PRESS,dropoff@TOOLING-BAY,pickup@EMPTIES(empty),wait@IN-STAGE,dropoff@PRESS",
		"PRESS_B": "pickup@PRESS_B,dropoff@TOOLING-BAY,pickup@EMPTIES(empty),wait@IN-STAGE,dropoff@PRESS_B",
	}
	for seat, wantSteps := range want {
		a := actionFor(t, plan, seat)
		if a.Err != nil {
			t.Fatalf("%s: planning failed: %v", seat, a.Err)
		}
		if got := stepString(a.SupplyOrder); got != wantSteps {
			t.Errorf("%s supply steps =\n  %s\nwant\n  %s\n"+
				"This is the shape the sim proved on the floor; the refactor must reproduce it "+
				"exactly, and it must come out of the decorator rather than a second mechanism.",
				seat, got, wantSteps)
		}
		if a.EvacOrder != nil {
			t.Errorf("%s: a staged seat evacuation is ONE robot's order; got a second: %s",
				seat, stepString(a.EvacOrder))
		}
	}
	// Exactly two actions — one per marked seat, and no leftover whole-cell
	// action for the press itself.
	if n := len(plan.Actions); n != 2 {
		var have []string
		for _, a := range plan.Actions {
			have = append(have, a.CoreNodeName+"("+a.LogTag+")")
		}
		t.Errorf("plan has %d actions (%s), want exactly 2 — one per marked seat.\n"+
			"A leftover whole-cell action means the press is being evacuated twice, once with "+
			"the index motion a tool change forbids.", n, strings.Join(have, ","))
	}
}

// ---------------------------------------------------------------------------
// The arm gate, now changeover-scoped.
// ---------------------------------------------------------------------------

// TestRefuseToolingChangeoverWithoutStaging pins that the arm gate still
// refuses a marked press with nowhere to stage — now for EVERY shape, not only
// the one where the same node appears in both styles.
func TestRefuseToolingChangeoverWithoutStaging(t *testing.T) {
	t.Parallel()
	from := pressClaim("PLN_002", "PLN_003", "PART-A")
	from.ChangeoverEvacSeats = []string{domain.EvacSeatFront}
	from.ChangeoverEvacDestination = "TOOLING-BAY"

	// Disjoint incoming press, and it names no staging node.
	to := pressClaim("PLN_007", "PLN_008", "PART-A")

	err := refuseToolingChangeoverWithoutStaging(
		[]processes.NodeClaim{from}, []processes.NodeClaim{to})
	if err == nil {
		t.Fatal("want a refusal when a marked press has no inbound staging anywhere in the changeover")
	}
	if !strings.Contains(err.Error(), "PLN_002") {
		t.Errorf("refusal must name the cell; got %q", err)
	}
	if !strings.Contains(err.Error(), "Inbound Staging") {
		t.Errorf("refusal must name the missing field; got %q", err)
	}
}

// A changeover with no marked seat is not a tooling changeover and is none of
// this pass's business.
func TestRefuseToolingChangeoverWithoutStaging_IgnoresUnmarkedPresses(t *testing.T) {
	t.Parallel()
	from := pressClaim("PLN_002", "PLN_003", "PART-A")
	to := pressClaim("PLN_002", "PLN_003", "PART-B")
	if err := refuseToolingChangeoverWithoutStaging(
		[]processes.NodeClaim{from}, []processes.NodeClaim{to}); err != nil {
		t.Errorf("an unmarked changeover was refused: %v", err)
	}
}
