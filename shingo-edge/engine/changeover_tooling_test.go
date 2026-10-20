package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
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
	from.ChangeoverEvacNodes = []string{"PRESS"}

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
	from.ChangeoverEvacNodes = []string{"PRESS"}
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
	from.ChangeoverEvacNodes = []string{"PRESS", "PRESS_B"}
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
	from.ChangeoverEvacNodes = []string{"PLN_001", "PLN_002"}

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
	from.ChangeoverEvacNodes = []string{"PRESS", "PRESS_B"}

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
	from.ChangeoverEvacNodes = []string{"PLN_002"}

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
	if !strings.Contains(err.Error(), "inbound staging") {
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

// ── Press-index: the press seat pulls for the pair's life ─────────────────
//
// active_pull is written in two places only: writePullSide (from commitFlip,
// which loadFlip applies to sequential pairs alone) and
// clearActivePullForEvacuate. The column defaults to 1 (store/schema:
// active_pull INTEGER NOT NULL DEFAULT 1). A press-index pair has no parked
// side: the index swaps carriers under the one press position, so its front is
// where the line pulls for the pair's whole life, and nothing has to write the
// bit there. The tick path skips a dark seat on a sequential pair only
// (isInactivePairedNode), and a tooling evacuate darkens a sequential pair only
// (the release guards that read the bit, loadFlip and linePullsFrom, read it on
// sequential alone).

func charSPActivePull(h *relHarness) bool {
	h.t.Helper()
	return runtimeOf(h.t, h.eng, h.nodeID).ActivePull
}

// seatSum is a seat's count plus its hold: where a tick lands, bound or held.
func seatSum(t *testing.T, h *relHarness, nodeID int64) int64 {
	t.Helper()
	rt := runtimeOf(t, h.eng, nodeID)
	return int64(rt.RemainingUOPCached) + rt.PendingUOPDelta
}

// TestPressIndex_ThePressSeatPullsForThePairsLife — the requirement strict pack
// rests on at a press-index press: the press seat of a two_robot_press_index
// pair has active_pull set at steady state, nothing on the pair writes it, a
// pair click (R1 out, R2 indexes) leaves it set, and produce ticks land before
// and after the release.
func TestPressIndex_ThePressSeatPullsForThePairsLife(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobotPressIndex}, "evac", protocol.StatusStaged, "supply", protocol.StatusStaged)(h)
	style := activeStyleOf(t, h.eng, h.processID)
	if !charSPActivePull(h) {
		t.Fatal("press seat active_pull = false at steady state, want the column default (true)")
	}
	charSPTick(h.eng, h.processID, style, 1)
	if got := remainingUOP(t, h.db, h.nodeID); got != fxCount+1 {
		t.Errorf("count after a tick = %d, want %d: the press seat takes the tick", got, fxCount+1)
	}

	testutil.MustNoErr(t, h.eng.ReleaseStagedOrders(h.nodeID, dispNone), "pair click")
	if !charSPActivePull(h) {
		t.Error("press seat active_pull = false after the pair click, want true: nothing writes it on a press-index pair")
	}
	charSPTick(h.eng, h.processID, style, 2)
	if rt := runtimeOf(t, h.eng, h.nodeID); rt.PendingUOPDelta != 2 {
		t.Errorf("hold after the release = %d, want 2: the finalize cleared the seat and the next ticks are held for the next bin", rt.PendingUOPDelta)
	}
}

// TestPressIndex_ToolingEvacuateLeavesThePressPulling — a tooling evacuate on a
// two_robot_press_index pair leaves the press seat pulling: the outgoing
// style's ticks land on the bound bin while the evacuate is staged, and after
// the evac's release (door 5) and the cutover the incoming style's ticks land
// at the press (held for the carrier R2 is placing there).
//
// At the base the evacuate cleared the bit on the press seat and nothing on a
// press-index pair ever set it again, so every produce tick at the press was
// skipped as inactive_produce: the parts made after a tool change were counted
// nowhere. Step 1 pinned that as the base; this is its flip.
func TestPressIndex_ToolingEvacuateLeavesThePressPulling(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, tooling: true}, "evac", protocol.StatusStaged, "supply", protocol.StatusStaged)(h)
	co, err := h.db.GetActiveProcessChangeover(h.processID)
	testutil.MustNoErr(t, err, "read changeover")
	task, err := h.db.GetChangeoverNodeTaskByNode(co.ID, h.nodeID)
	testutil.MustNoErr(t, err, "read node task")
	if task.Situation != string(SituationEvacuate) {
		t.Fatalf("fixture: situation = %q, want evacuate", task.Situation)
	}
	fromStyle := activeStyleOf(t, h.eng, h.processID)

	if !charSPActivePull(h) {
		t.Fatal("press seat active_pull = false after the tooling evacuate, want true: a press-index pair has no parked side")
	}
	charSPTick(h.eng, h.processID, fromStyle, 2)
	if rt := runtimeOf(t, h.eng, h.nodeID); rt.RemainingUOPCached != fxCount+2 || rt.PendingUOPDelta != 0 {
		t.Errorf("after a tick at the evacuating press: count %d hold %d, want %d and 0: the bound bin takes it",
			rt.RemainingUOPCached, rt.PendingUOPDelta, fxCount+2)
	}

	testutil.MustNoErr(t, sweepClick(dispNone)(h), "release the evacuate's legs")
	toStyle := co.ToStyleID
	testutil.MustNoErr(t, h.eng.SetProcessActiveStyle(h.processID, &toStyle), "cut over to the incoming style")
	if !charSPActivePull(h) {
		t.Fatal("press seat active_pull = false after the release and the cutover, want true")
	}
	before := seatSum(t, h, h.nodeID)
	charSPTick(h.eng, h.processID, toStyle, 5)
	if after := seatSum(t, h, h.nodeID); after != before+5 {
		t.Errorf("incoming style's tick at the press: count+hold %d → %d, want +5 (landed, not skipped)", before, after)
	}
}

// TestPressIndex_ADarkPressSeatStillCounts — a press-index seat whose bit a
// tooling evacuate cleared before this build (a state a plant's database can
// hold at deploy) counts its ticks: the tick path reads the bit on a
// sequential pair only.
func TestPressIndex_ADarkPressSeatStillCounts(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobotPressIndex}, "evac", protocol.StatusStaged, "supply", protocol.StatusStaged)(h)
	testutil.MustNoErr(t, h.db.SetActivePull(h.nodeID, false), "a past evacuate's dark bit")
	charSPTick(h.eng, h.processID, activeStyleOf(t, h.eng, h.processID), 3)
	if got := remainingUOP(t, h.db, h.nodeID); got != fxCount+3 {
		t.Errorf("count after a tick on a dark press-index seat = %d, want %d", got, fxCount+3)
	}
}

// TestSequential_ADarkPairLightsTheSideTheNextCarrierLandsOn — a tooling
// evacuate on a sequential A/B pair clears the bit on both positions (so the
// release guards let the evacuate's legs strip either one), and a tick on the
// stopped press lands on neither. The evacuate's release flips from no side and
// the cutover writes no bit, so the pair stays dark through both. The first
// carrier that lands on either position lights that position (landCarrier,
// lightDarkSequentialPair); the other stays dark until a release flips the
// line to it, and a carrier landing there meanwhile changes nothing.
func TestSequential_ADarkPairLightsTheSideTheNextCarrierLandsOn(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	coAt(coSpec{mode: protocol.SwapModeSequential, tooling: true})(h)
	co, err := h.db.GetActiveProcessChangeover(h.processID)
	testutil.MustNoErr(t, err, "read changeover")
	task, err := h.db.GetChangeoverNodeTaskByNode(co.ID, h.nodeID)
	testutil.MustNoErr(t, err, "read node task")
	if task.Situation != string(SituationEvacuate) {
		t.Fatalf("fixture: situation = %q, want evacuate", task.Situation)
	}
	if got := h.activePull(); got != "pull=none" {
		t.Fatalf("after the tooling evacuate: %s, want pull=none (both positions dark)", got)
	}
	tickLandsNowhere := func(when string, style int64) {
		t.Helper()
		front, back := seatSum(t, h, h.nodeID), seatSum(t, h, h.partnerID)
		charSPTick(h.eng, h.processID, style, 4)
		if f, b := seatSum(t, h, h.nodeID), seatSum(t, h, h.partnerID); f != front || b != back {
			t.Errorf("%s: a tick on the dark sequential pair moved the seats: front %d→%d back %d→%d, want both unchanged",
				when, front, f, back, b)
		}
	}
	tickLandsNowhere("after the evacuate", activeStyleOf(t, h.eng, h.processID))

	for _, l := range h.legs {
		h.setStatus(l.name, protocol.StatusStaged)
	}
	testutil.MustNoErr(t, sweepClick(dispNone)(h), "release the changeover's legs")
	toStyle := co.ToStyleID
	testutil.MustNoErr(t, h.eng.SetProcessActiveStyle(h.processID, &toStyle), "cut over to the incoming style")
	if got := h.activePull(); got != "pull=none" {
		t.Errorf("after the release and the cutover: %s, want pull=none (no landing yet)", got)
	}
	tickLandsNowhere("after the cutover", toStyle)

	back, err := h.db.GetProcessNode(h.partnerID)
	testutil.MustNoErr(t, err, "read the back position")
	front, err := h.db.GetProcessNode(h.nodeID)
	testutil.MustNoErr(t, err, "read the front position")
	h.eng.HandleBinLanded(protocol.BinLanded{BinID: 9701, CoreNodeName: back.CoreNodeName, Epoch: 1, PlacedSeq: 1 << 40})
	if got := h.activePull(); got != "pull="+back.CoreNodeName {
		t.Fatalf("after a carrier lands on %s: %s, want pull=%s (the side it landed on)", back.CoreNodeName, got, back.CoreNodeName)
	}
	if rt := runtimeOf(t, h.eng, h.nodeID); rt.ActivePull {
		t.Errorf("the front position %s lit too; want it dark until the flip", front.CoreNodeName)
	}
	before := seatSum(t, h, h.partnerID)
	charSPTick(h.eng, h.processID, toStyle, 4)
	if got := seatSum(t, h, h.partnerID); got != before+4 {
		t.Errorf("a tick after the landing: %s's seat %d→%d, want +4 on the lit side", back.CoreNodeName, before, got)
	}
	h.eng.HandleBinLanded(protocol.BinLanded{BinID: 9702, CoreNodeName: front.CoreNodeName, Epoch: 1, PlacedSeq: 1<<40 + 1})
	if got := h.activePull(); got != "pull="+back.CoreNodeName {
		t.Errorf("after a carrier lands on the dark side of a lit pair: %s, want pull=%s unchanged", got, back.CoreNodeName)
	}
	if rt := runtimeOf(t, h.eng, h.nodeID); rt.ActivePull {
		t.Errorf("a landing on %s lit it beside the lit %s", front.CoreNodeName, back.CoreNodeName)
	}
}

// TestSequential_ALandingOnALitPairChangesNothing — a sequential pair pulling
// from its front position: carriers landing on either position leave the bits
// as they are, whichever side they land on.
func TestSequential_ALandingOnALitPairChangesNothing(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	coAt(coSpec{mode: protocol.SwapModeSequential, tooling: true})(h)
	testutil.MustNoErr(t, h.db.SetActivePull(h.nodeID, true), "the line pulls from the front")
	front, err := h.db.GetProcessNode(h.nodeID)
	testutil.MustNoErr(t, err, "read the front position")
	back, err := h.db.GetProcessNode(h.partnerID)
	testutil.MustNoErr(t, err, "read the back position")
	for i, n := range []*processes.Node{back, front} {
		h.eng.HandleBinLanded(protocol.BinLanded{BinID: int64(9711 + i), CoreNodeName: n.CoreNodeName, Epoch: 1, PlacedSeq: 1<<40 + int64(i)})
		if rt := runtimeOf(t, h.eng, n.ID); rt.ActiveBinID == nil || *rt.ActiveBinID != int64(9711+i) {
			t.Fatalf("fixture: the landing on %s bound %v, want bin %d", n.CoreNodeName, rt.ActiveBinID, 9711+i)
		}
		if got := h.activePull(); got != "pull="+front.CoreNodeName {
			t.Errorf("after a carrier lands on %s: %s, want pull=%s unchanged", n.CoreNodeName, got, front.CoreNodeName)
		}
		if rt := runtimeOf(t, h.eng, h.partnerID); rt.ActivePull {
			t.Errorf("after a carrier lands on %s: the back position lit beside the front", n.CoreNodeName)
		}
	}
}

// TestStrictPack_PressIndexPacksHoldsAndReplaysOntoTheNextBin — strict pack on
// a press-index press through one index: the bound bin is charged to its pack
// and the rest is held; the pair click ships the pack (no declaration: the bin
// is at it) and the seat unbinds keeping the hold; a stroke while R2 is placing
// the next carrier is held with it; the next bin binds at the front and the
// next stroke replays the hold onto it as one produce_tick.
func TestStrictPack_PressIndexPacksHoldsAndReplaysOntoTheNextBin(t *testing.T) {
	t.Parallel()
	const pack = fxCount + 2
	h := newRelHarness(t)
	strictPackAt(pack, true, pairAt(pairSpec{mode: protocol.SwapModeTwoRobotPressIndex}, "evac", protocol.StatusStaged, "supply", protocol.StatusStaged))(h)
	style := activeStyleOf(t, h.eng, h.processID)
	produceTicks := func() []string {
		h.mutator.Flush()
		out := []string{}
		for _, m := range h.sinceMark() {
			if m.MsgType != protocol.SubjectBinUOPDelta {
				continue
			}
			if d := decodeBinUOPDelta(t, m.Payload); d.Reason == protocol.ReasonProduceTick {
				out = append(out, fmt.Sprintf("bin %d %+d", d.BinID, d.Delta))
			}
		}
		return out
	}
	seat := func(want string) {
		t.Helper()
		rt := runtimeOf(t, h.eng, h.nodeID)
		bin := "none"
		if rt.ActiveBinID != nil {
			bin = fmt.Sprint(*rt.ActiveBinID)
		}
		if got := fmt.Sprintf("bin %s count %d hold %d of %q", bin, rt.RemainingUOPCached, rt.PendingUOPDelta, rt.PendingUOPPayload); got != want {
			t.Errorf("seat = %s, want %s", got, want)
		}
	}

	// The crossing stroke: 42 + 5 on a pack of 44.
	h.mark()
	charSPTick(h.eng, h.processID, style, 5)
	seat(fmt.Sprintf("bin %d count %d hold 3 of %q", fxBin, pack, fxPart))
	if got, want := produceTicks(), []string{fmt.Sprintf("bin %d +2", fxBin)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("crossing stroke emitted %v, want %v", got, want)
	}

	// The pair click: R1 takes the full bin, R2 indexes the next carrier in.
	h.mark()
	testutil.MustNoErr(t, h.eng.ReleaseStagedOrders(h.nodeID, dispNone), "pair click")
	if got, want := pStamp(h), []string{fmt.Sprintf("stamp:q=%d", pack)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the release shipped %v, want %v (the pack, no declaration)", got, want)
	}
	seat(fmt.Sprintf("bin none count 0 hold 3 of %q", fxPart))

	// A stroke while R2 is placing the next carrier at the front.
	h.mark()
	charSPTick(h.eng, h.processID, style, 2)
	seat(fmt.Sprintf("bin none count 0 hold 5 of %q", fxPart))
	if got := produceTicks(); len(got) != 0 {
		t.Errorf("a held stroke emitted %v, want nothing", got)
	}

	// R2's carrier binds at the front; the next stroke replays the hold onto it.
	next := fxBin + 50
	testutil.MustNoErr(t, h.db.SetProcessNodeActiveBinID(h.nodeID, &next), "the next bin binds at the front")
	h.mark()
	charSPTick(h.eng, h.processID, style, 1)
	seat(fmt.Sprintf("bin %d count 6 hold 0 of %q", next, ""))
	if got, want := produceTicks(), []string{fmt.Sprintf("bin %d +6", next)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the replay emitted %v, want %v", got, want)
	}
}
