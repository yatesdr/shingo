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
// AND SHINGO NEVER TOUCHES TOOLING. The tool change is human work done at the
// asset — press, weld cell, whatever it is. What shingo owes it is FLOOR SPACE
// and TIMING: get the material off the marked positions quickly so the people have
// room to set up, and hold the incoming material somewhere out of the cell
// until they say they are done, instead of delivering it to line nodes a human
// is standing in. Some cells change over without evacuating at all, because
// their tool change happens internally.
//
// So a marked press ALWAYS gets tooling treatment, and treatment means exactly
// two things: its marked positions are CLEARED, and every bin coming back to the
// press waits at inbound staging until the operator marks the change done.
// Bin type never disqualifies it, and neither does the incoming style running
// on different nodes.
//
// CLEARED MEANS NORMAL ROUTING. A marked position's bin goes wherever that cell's
// bins ordinarily go — its unloader, its buffer, its market — and the leg the
// pipeline already planned is left alone. `changeover_evac_destination` is an
// OPTIONAL OVERRIDE for a cell that wants its clearance somewhere else, like a
// different node group; empty is the default and empty means untouched. There
// is no bay: the one-slot "tooling bay" in the demo fixture was built on the
// wrong premise and produced robots dwelling on an occupied station with bins
// nothing would ever take away.
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

// outboundOf is where this action disposes of the bin it lifts off its own
// position: the first dropoff after the pickup at the position, across both orders.
//
// It exists because "did the decorator steer the outbound leg" is now a
// question about ONE step, and comparing whole shapes cannot answer it — a
// marked and an unmarked position differ on the inbound half by design.
func outboundOf(t *testing.T, a changeover.NodeAction) string {
	t.Helper()
	for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
		if spec == nil || spec.Complex == nil {
			continue
		}
		seen := false
		for _, s := range spec.Complex.Steps {
			if s.Action == protocol.ActionPickup && s.Node == a.CoreNodeName {
				seen = true
				continue
			}
			if seen && s.Action == protocol.ActionDropoff {
				return s.Node
			}
		}
	}
	return "<none>"
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

// pressClaim builds a press-index claim. positions/evacDest belong on the OUTGOING
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

// TestToolingBeatsRelabel is the contract in one sentence: a marked position gets
// tooling treatment even when the changeover also changes bin type.
//
// The bin-type fan-out rewrites the diff's SwapMode to press_position, and the
// staged fan-out's predicate required two_robot_press_index — so the press that
// most needs a tool change (its carriers are physically different) was the one
// shape that silently got none. Marked beats relabel.
func TestToolingBeatsRelabel(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront}

	to := pressClaim("PRESS", "PRESS_B", "PART-B")
	to.InboundStaging = "IN-STAGE"

	binTypes := map[string]string{"PART-A": "BIG", "PART-B": "SMALL"}
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		binTypes, "PRESS", "PRESS_B")

	// The MARKED position: cleared by NORMAL ROUTING, and its replacement holds at
	// staging until the operator marks the change done.
	marked := actionFor(t, plan, "PRESS")
	got := allSteps(marked)
	if !strings.Contains(got, "wait@IN-STAGE") {
		t.Errorf("marked position PRESS does not hold its incoming bin at inbound staging.\n"+
			"steps = %s\n"+
			"Tooling is literally just adding a staging step for inbound: the new bin waits "+
			"out of the cell while a human is inside it.", got)
	}
	if !strings.Contains(got, "dropoff@MARKET") {
		t.Errorf("marked position PRESS lost its ordinary clearance destination.\n"+
			"steps = %s\n"+
			"With no changeover_evac_destination set, a marked position is cleared by NORMAL "+
			"ROUTING — wherever this cell's bins ordinarily go. The decorator does not own "+
			"the outbound side unless an override says so.", got)
	}

	// THE POINT OF THE SHAPE: bin type does not disqualify the position from the
	// HOLD. That is what "marked beats relabel" means now that clearance is
	// normal routing — the bin-type fan-out relabels the diff, and the hold
	// survives it.
	unmarked := actionFor(t, plan, "PRESS_B")
	ugot := allSteps(unmarked)
	if !strings.Contains(ugot, "wait@IN-STAGE") {
		t.Errorf("unmarked position PRESS_B delivers into a press mid tool-change.\n"+
			"steps = %s\n"+
			"EVERY inbound leg to the press holds — the press is down, marked position or not.", ugot)
	}

	// And with no override, the OUTBOUND half of a marked position is
	// indistinguishable from an unmarked one. That is by design: clearing a
	// position is not a different journey, it is the same journey done now.
	if outboundOf(t, marked) != outboundOf(t, unmarked) {
		t.Errorf("marked position PRESS clears to %q but unmarked PRESS_B to %q.\n"+
			"With no override both are ordinary evacuations to the same place; a difference "+
			"here means the decorator is still steering the outbound leg.",
			outboundOf(t, marked), outboundOf(t, unmarked))
	}
}

// TestToolingClearanceOverrideRedirectsOnlyMarkedPositions is the OTHER half of the
// same rule, and it is the half a fixture must keep exercising: when a cell
// names a clearance destination, its MARKED positions go there and nothing else
// does.
//
// The override exists for a cell whose ordinary outbound is the wrong place to
// put bins during a setup — "somewhere new, like a different node group". It is
// not a bay, it is not special, and whatever it names gets ordinary capacity
// behaviour, which is why the fixture points it at a group.
func TestToolingClearanceOverrideRedirectsOnlyMarkedPositions(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront}
	from.ChangeoverEvacDestination = "CLEARANCE-GROUP"

	to := pressClaim("PRESS", "PRESS_B", "PART-B")
	to.InboundStaging = "IN-STAGE"

	binTypes := map[string]string{"PART-A": "BIG", "PART-B": "SMALL"}
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		binTypes, "PRESS", "PRESS_B")

	marked := actionFor(t, plan, "PRESS")
	if got := allSteps(marked); !strings.Contains(got, "dropoff@CLEARANCE-GROUP") {
		t.Errorf("marked position PRESS ignored changeover_evac_destination.\nsteps = %s\n"+
			"An override that does not redirect is an override that ships unexercised.", got)
	}
	unmarked := actionFor(t, plan, "PRESS_B")
	if got := allSteps(unmarked); strings.Contains(got, "CLEARANCE-GROUP") {
		t.Errorf("unmarked position PRESS_B was redirected to the clearance destination.\n"+
			"steps = %s\nOnly MARKED positions are cleared; an unmarked position keeps whatever the "+
			"normal machinery gives it.", got)
	}
}

// TestToolingWithoutOverridePlansNoOutboundEdit pins the contract at the seam
// rather than through a shape: with no override there is nothing for the
// decorator to do on the outbound side, so it must not have an edit queued at
// all. A pass that "redirects" every marked position to the destination the leg
// already had is one config change away from redirecting it somewhere else.
func TestToolingWithoutOverridePlansNoOutboundEdit(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront, domain.EvacPositionPaired}
	to := pressClaim("PRESS", "PRESS_B", "PART-A")
	to.InboundStaging = "IN-STAGE"

	tc := planToolingChangeover([]processes.NodeClaim{from}, []processes.NodeClaim{to})
	if !tc.active() {
		t.Fatal("a marked press is a tooling changeover whether or not it overrides clearance")
	}
	if len(tc.evacDest) != 0 {
		t.Errorf("planToolingChangeover queued outbound redirects %v with no override set.\n"+
			"Empty changeover_evac_destination means the outbound leg is UNTOUCHED — not "+
			"redirected to the value it already had.", tc.evacDest)
	}
	if len(tc.stageAt) == 0 {
		t.Error("no staging hold planned — the hold is the half that is never optional")
	}
}

// ---------------------------------------------------------------------------
// SHAPE 2 — marked + DISJOINT nodes. The incoming style runs elsewhere.
// ---------------------------------------------------------------------------

// TestToolingAcrossDisjointNodes pins the owner's second shape: style 1 on
// PLN_001/PLN_002, style 2 on PLN_005/PLN_006. No node is in both styles, so
// the marked positions are Drops and the new positions are Adds, and the old staged
// fan-out — which required FromClaim AND ToClaim on one diff — contributed
// nothing at all: the tooling bins went to the ordinary evac destination and
// the new material was delivered straight to lineside with no hold.
//
// THIS IS ALSO THE CROSS-MODE PIN. DiffStyleClaims produces only PLN_001(drop)
// and PLN_005(add) — the two claim ROWS. The extension positions PLN_002(drop) and
// PLN_006(add) exist solely because FanOutPressIndexCrossMode synthesizes them,
// verified by tracing the passes. So the assertions below on PLN_002 and
// PLN_006 are assertions that the decorator correctly edits legs planned from
// cross-mode's synthesized diffs — the one ordering interaction the two passes
// have, and it is safe by construction: cross-mode makes diffs, the decorator
// edits the plan those diffs produced, so neither can preempt the other.
func TestToolingAcrossDisjointNodes(t *testing.T) {
	t.Parallel()
	from := pressClaim("PLN_001", "PLN_002", "PART-A")
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront, domain.EvacPositionPaired}

	to := pressClaim("PLN_005", "PLN_006", "PART-A")
	to.InboundStaging = "IN-STAGE"

	binTypes := map[string]string{"PART-A": "BIG"}
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		binTypes, "PLN_001", "PLN_002", "PLN_005", "PLN_006")

	// Both marked positions are CLEARED, by normal routing. The point of the shape
	// is that they are cleared AT ALL: the old fan-out required FromClaim AND
	// ToClaim on one diff, so on disjoint nodes it contributed nothing — no
	// clearance guarantee on this side and no hold on the other.
	for _, position := range []string{"PLN_001", "PLN_002"} {
		a := actionFor(t, plan, position)
		got := allSteps(a)
		if !strings.Contains(got, "pickup@"+position) {
			t.Errorf("marked position %s is never cleared.\nsteps = %s\n"+
				"The rule holds across every changeover shape — the incoming style running on "+
				"different nodes does not make these bins any less in the way of the setup.",
				position, got)
		}
		if dest := outboundOf(t, a); dest != "MARKET" {
			t.Errorf("marked position %s clears to %q, not to its ordinary destination.\nsteps = %s\n"+
				"With no override, clearance is normal routing.", position, dest, got)
		}
	}

	// Every new position stages and holds behind tooling-done.
	for _, position := range []string{"PLN_005", "PLN_006"} {
		a := actionFor(t, plan, position)
		got := allSteps(a)
		if !strings.Contains(got, "wait@IN-STAGE") {
			t.Errorf("new position %s takes delivery with no tooling hold.\nsteps = %s\n"+
				"A human is inside the cell. Material must wait at inbound staging and move in "+
				"on the operator's release, not drive straight to lineside.", position, got)
		}
	}
}

// ---------------------------------------------------------------------------
// SHAPE 3 — REGRESSION PIN. Marked + same node + SAME bin type.
// ---------------------------------------------------------------------------

// TestToolingSameBinTypeKeepsTheProvenShape is the pin on the one shape that
// already worked. The sim proved it end to end on 2026-08-24:
//
//	pickup@POSITION -> dropoff@EVAC-DEST -> pickup@SRC -> wait@STAGE -> dropoff@POSITION
//
// The decorator refactor must not change it. This is also the case that proves
// per-position granularity is not optional: the whole-cell press-index builder
// evacuates only the FRONT position and INDEXES the paired bin forward onto it,
// which is exactly the motion a tool change forbids. So a marked press has to
// reach one action per marked position however it got here.
func TestToolingSameBinTypeKeepsTheProvenShape(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront, domain.EvacPositionPaired}

	to := pressClaim("PRESS", "PRESS_B", "PART-A")
	to.InboundStaging = "IN-STAGE"

	binTypes := map[string]string{"PART-A": "BIG"}
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		binTypes, "PRESS", "PRESS_B")

	want := map[string]string{
		"PRESS":   "pickup@PRESS,dropoff@MARKET,pickup@EMPTIES(empty),wait@IN-STAGE,dropoff@PRESS",
		"PRESS_B": "pickup@PRESS_B,dropoff@MARKET,pickup@EMPTIES(empty),wait@IN-STAGE,dropoff@PRESS_B",
	}
	for position, wantSteps := range want {
		a := actionFor(t, plan, position)
		if a.Err != nil {
			t.Fatalf("%s: planning failed: %v", position, a.Err)
		}
		if got := stepString(a.SupplyOrder); got != wantSteps {
			t.Errorf("%s supply steps =\n  %s\nwant\n  %s\n"+
				"This is the shape the sim proved on the floor; the refactor must reproduce it "+
				"exactly, and it must come out of the decorator rather than a second mechanism.",
				position, got, wantSteps)
		}
		if a.EvacOrder != nil {
			t.Errorf("%s: a staged position evacuation is ONE robot's order; got a second: %s",
				position, stepString(a.EvacOrder))
		}
	}
	// Exactly two actions — one per marked position, and no leftover whole-cell
	// action for the press itself.
	if n := len(plan.Actions); n != 2 {
		var have []string
		for _, a := range plan.Actions {
			have = append(have, a.CoreNodeName+"("+a.LogTag+")")
		}
		t.Errorf("plan has %d actions (%s), want exactly 2 — one per marked position.\n"+
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
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront}

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

// A changeover with no marked position is not a tooling changeover and is none of
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
