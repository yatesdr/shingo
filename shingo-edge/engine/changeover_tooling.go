package engine

import (
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/release"
	"shingoedge/store/processes"
)

// ── TOOLING IS A DECORATOR, NOT A COMPETITOR ────────────────────────────────
//
// A press whose outgoing claim marks positions is going down for a tool change.
// The rule the floor gave us is one sentence: the marked positions are cleared, the
// new material waits at inbound staging, the operator does the tool change and
// marks it done, and the material moves in.
//
// ── SHINGO DOES NOT TOUCH TOOLING, AND THERE IS NO BAY ──────────────────────
//
// The tool change is human work done at the asset. What shingo owes it is FLOOR
// SPACE and TIMING: get the material off the marked positions quickly so the people
// have room to set up the next run, and hold the incoming material out of the
// cell until they say they are done, rather than delivering it to line nodes a
// human is standing in. Some cells change over without evacuating anything at
// all, because their tool change happens internally.
//
// So CLEARANCE IS NORMAL ROUTING. A marked position's bin goes wherever that cell's
// bins ordinarily go — its unloader, its buffer, its market: the outgoing
// claim's outbound, where Core sends the carrier by what it holds as it does any
// carrier lifted off a line — and this pass leaves that leg alone. There is no
// destination of the tool change's own: one existed (an override, and before it
// a "tooling bay" with one slot, where the second bin of a two-position press
// had nowhere to go and robots dwelt holding bins nothing would take away), and
// it was a second place to answer where a carrier goes.
//
// WHY THIS IS A PASS OVER THE FINISHED PLAN AND NOT A FIFTH FAN-OUT.
//
// It used to be a fan-out — FanOutStagedToolingEvacuation — sitting in the diff
// pipeline beside the bin-type fan-out (now PlanPressPositions), and the two competed. The
// bin-type pass ran first and rewrote the diff's SwapMode to press_position;
// the tooling pass's predicate required two_robot_press_index; so on a press
// that was BOTH marked and changing bin type, tooling silently did nothing.
// Marked positions were never cleared and new material drove into a cell with a
// human in it. No error, no advisory. That is N1, proven on the sim
// 2026-08-24, and the shape it broke is the common one: a press changing
// carrier type is a press changing tooling.
//
// Precedence between passes is the disease. This pass has no precedence to get
// wrong: it runs LAST, it reads the ORIGINAL claims rather than whatever an
// earlier pass rewrote, and it edits legs the pipeline already produced. There
// is no changeover shape it can miss, because it never has to recognise one.
//
// ── A MARKED PRESS NEVER INDEXES ────────────────────────────────────────────
//
// The whole-cell same-bin-type builder (buildPressIndexChangeoverSwap)
// evacuates ONLY the front position and then INDEXES the paired bin forward
// onto it. That index motion is exactly what a tool change forbids. So the diff
// pipeline splits a marked press position by position, as it splits a press
// changing carrier type (shouldFanOutPressIndex), each position's situation
// its own (domain.ChangeoverSituation): this pass then replaces the marked
// positions' legs and leaves every unmarked position its ordinary per-position
// changeover. It creates no node of its own; the marks only choose which
// planned legs are the tool change's.

// toolingPress is one marked press in a changeover.
type toolingPress struct {
	from *processes.NodeClaim // the OUTGOING claim — it owns the position marks
	to   *processes.NodeClaim // the incoming claim on the SAME node, if any
	// positions are the core nodes of every marked position the layout actually has.
	positions []string
	staging   string
	// roundTrip is the subset of positions whose part carries over and whose cell
	// chose outbound_staging, mapped to the spot their bin waits at.
	roundTrip map[string]string
}

// toolingChangeover is the tooling decoration for ONE changeover. The zero
// value is "not a tooling changeover" and decorates nothing.
type toolingChangeover struct {
	presses []toolingPress
	// stageAt maps a press position's core node to the staging node its INCOMING
	// bin waits at. Every position of the cell appears, marked or not — the press
	// is down and a human is inside it, so nothing may drive to lineside.
	stageAt map[string]string
	// roundTrip maps a marked position whose part CARRIES OVER, and whose cell
	// chose outbound_staging, to the staging spot its bin waits at. That bin
	// does not go to the supermarket and no replacement is fetched: the same
	// carrier hops out to clear the floor and returns on the tooling-done
	// release. Empty for every other position.
	roundTrip map[string]string
}

func (t toolingChangeover) active() bool { return len(t.presses) > 0 }

// planToolingChangeover reads the ORIGINAL claim lists — not the diffs — and
// answers two questions: which positions have their clearance redirected by an
// override, and which positions hold their incoming material at staging.
//
// Reading the originals is the whole point. The diffs are what the earlier
// passes rewrote, and the field the old predicate keyed on (SwapMode) is one of
// the fields they rewrite.
//
// SCOPE IS THE CHANGEOVER, which is the cell. A changeover belongs to one
// process; if that process's outgoing style marks positions, the cell is down. The
// staging set is therefore drawn from every INCOMING press-index claim, which
// is what makes the disjoint-node shape work at all: when style 1 runs on
// PLN_001-002 and style 2 on PLN_005-006, nothing pairs the two sides, and the
// only honest reading of "the press" is the cell they both belong to.
func planToolingChangeover(fromClaims, toClaims []processes.NodeClaim) toolingChangeover {
	toByNode := make(map[string]*processes.NodeClaim, len(toClaims))
	for i := range toClaims {
		toByNode[toClaims[i].CoreNodeName] = &toClaims[i]
	}

	var t toolingChangeover
	for i := range fromClaims {
		fc := &fromClaims[i]
		marked := domain.MarkedEvacNodes(fc)
		if len(marked) == 0 {
			continue
		}
		// CARRY-OVER SPLITS THE MARKED POSITIONS THREE WAYS. A position whose part the
		// cell keeps may not need clearing at all (keep_lineside — it never
		// appears below, so nothing in this pass touches its leg or builds one)
		// or may only need to be out of the way (outbound_staging — the same
		// bin walks out and comes back). Everything else is cleared, which
		// includes every position whose part is changing: the disposition is about
		// a bin the cell keeps and there is no such bin there.
		var positions []string
		for _, position := range marked {
			switch domain.ClearanceTreatmentAtNode(fc, toByNode[fc.CoreNodeName], position) {
			case domain.ClearanceKeep:
				continue
			case domain.ClearanceRoundTrip:
				if t.roundTrip == nil {
					t.roundTrip = make(map[string]string)
				}
				t.roundTrip[position] = fc.OutboundStaging
			}
			positions = append(positions, position)
		}
		if len(positions) == 0 {
			continue // every marked position keeps its part; nothing to decorate
		}
		t.presses = append(t.presses, toolingPress{
			from:      fc,
			to:        toByNode[fc.CoreNodeName],
			positions: positions,
			roundTrip: pressRoundTrips(t.roundTrip, positions),
		})
	}
	if !t.active() {
		return toolingChangeover{}
	}

	t.stageAt = toolChangeHolds(t.presses, fromClaims, toClaims)
	return t
}

// toolChangeStaging is where a marked cell's incoming carrier waits for
// Tooling done: the incoming claim's inbound staging on the cell's node, else
// the marked claim's own, which the save requires of a claim with marks.
func toolChangeStaging(p toolingPress) string {
	if p.to != nil && p.to.InboundStaging != "" {
		return p.to.InboundStaging
	}
	return p.from.InboundStaging
}

// toolChangeHolds is the tool change's holds: the positions whose incoming
// carrier waits at staging until the operator's Tooling done, mapped to that
// staging. presses get their staging set (toolChangeStaging).
//
//   - Every marked position: it is cleared at START and filled after Tooling
//     done.
//   - Every position the incoming style ADDS to the process, a node no
//     outgoing claim stands on (the disjoint shape: a press that moved). It is
//     filled during the tool change with no Ready press to gate it, so it
//     waits at its incoming claim's staging when that claim names one.
//
// An unmarked position an outgoing claim stands on is NOT held: it gets its
// ordinary changeover and its ordinary Ready press.
func toolChangeHolds(presses []toolingPress, fromClaims, toClaims []processes.NodeClaim) map[string]string {
	holds := map[string]string{}
	for i := range presses {
		presses[i].staging = toolChangeStaging(presses[i])
		if presses[i].staging == "" {
			continue
		}
		for _, position := range presses[i].positions {
			holds[position] = presses[i].staging
		}
	}
	occupied := outgoingPositions(fromClaims)
	for i := range toClaims {
		tc := &toClaims[i]
		if tc.InboundStaging == "" {
			continue
		}
		for _, position := range tc.ToolChangePositions() {
			if !occupied[position] {
				holds[position] = tc.InboundStaging
			}
		}
	}
	return holds
}

// outgoingPositions is every position an outgoing claim stands on.
func outgoingPositions(fromClaims []processes.NodeClaim) map[string]bool {
	out := map[string]bool{}
	for i := range fromClaims {
		for _, position := range fromClaims[i].ToolChangePositions() {
			out[position] = true
		}
	}
	return out
}

// resolveToolingNodes makes sure every marked position is a node this plan can
// name, and it is the fix for N1-a.
//
// A marked position owns no claim row, so nothing had ever created its
// process_nodes row except ChangeoverService.Create — which runs after the plan
// is built. The first changeover of a press therefore planned around a position
// that did not exist yet, and the second one worked because the first had left
// the row behind.
//
// materialize splits the two callers. Start writes the rows here, BEFORE
// planning, and re-reads so the ids are real. Preview must not write, so it
// carries the same positions as unsaved nodes and shows the operator the work the
// changeover will actually do.
//
// The refusal is LOUD, and deliberately so: a node this cannot resolve is a node
// whose work silently does not happen, and the silent version of that sentence
// is the whole reason N1 existed.
func (e *Engine) resolveToolingNodes(processID int64, t toolingChangeover,
	participants []domain.ParticipantInput, nodes []processes.Node,
	materialize bool) ([]processes.Node, error) {

	positions := changeoverTouchedNodes(t, participants)
	if len(positions) == 0 {
		return nodes, nil
	}
	have := make(map[string]bool, len(nodes))
	for i := range nodes {
		have[nodes[i].CoreNodeName] = true
	}
	created := false
	for _, position := range positions {
		if have[position] {
			continue
		}
		have[position] = true
		if !materialize {
			nodes = append(nodes, processes.Node{
				ProcessID:    processID,
				CoreNodeName: position,
				Code:         position,
				Name:         position,
				Enabled:      true,
			})
			continue
		}
		if _, err := e.db.CreateProcessNode(processes.NodeInput{
			ProcessID:    processID,
			CoreNodeName: position,
			Code:         position,
			Name:         position,
			Enabled:      true,
		}); err != nil {
			return nil, fmt.Errorf("cannot start changeover: this changeover acts on node %s, "+
				"which this process does not have and which could not be created: %w", position, err)
		}
		created = true
	}
	if created {
		// Re-read rather than patch the slice: the ids have to be the real ones
		// or the node tasks below point at rows that do not exist.
		return e.db.ListProcessNodesByProcess(processID)
	}
	return nodes, nil
}

// expandsPositions reports whether this press's marked positions are refilled:
// an incoming claim stands on its own node. A press with none is a teardown:
// there is nothing to refill with, its positions are drops as the pipeline
// planned them, and nothing waits at staging for them.
func (p toolingPress) expandsPositions() bool { return p.to != nil }

// changeoverTouchedNodes is every node this changeover acts on, deduped: the
// PARTICIPANTS the diffs produced, plus the marked nodes.
//
// BOTH HALVES, AND THE SECOND ONE WAS FOUND THE HARD WAY. Scoping this to the
// marked nodes fixed the paired node and left the Adds exactly where they were:
// an Add exists because PlanPressPositions synthesized it, so no mark names it,
// and on a disjoint changeover its node has no row either. It got a task, no
// order, and blocked the cutover — the same defect as N1-a, one branch over,
// caught by the spot-check that was meant to confirm the fix.
//
// The marks are still added explicitly rather than trusted to appear among the
// participants: a marked node is a node this pass may expand an action for
// whatever the diffs did with it.
func changeoverTouchedNodes(t toolingChangeover, participants []domain.ParticipantInput) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, p := range participants {
		add(p.CoreNodeName)
	}
	for _, press := range t.presses {
		for _, position := range press.positions {
			add(position)
		}
	}
	return out
}

// refuseToolingChangeoverWithoutStaging is START's backstop for the save's rule
// that a claim with marks names inbound staging: a tool change holds the
// incoming carriers there (toolChangeHolds), so every hold has to say where.
// Scoped by the marks, never by the mode: a changeover with no marks is none of
// its business, and in one with marks it names the marked cells whose refill
// has no staging (the incoming claim on the node names none, and nor does the
// marked claim, a row saved before the rule) and every incoming claim that adds
// a node to the process with none.
//
// Named fields, named cells. "changeover requires inbound staging" sends an
// engineer to the wrong page on a line with six presses.
func refuseToolingChangeoverWithoutStaging(fromClaims, toClaims []processes.NodeClaim) error {
	t := planToolingChangeover(fromClaims, toClaims)
	if !t.active() {
		return nil
	}
	var marked, missing []string
	for _, p := range t.presses {
		marked = append(marked, p.from.CoreNodeName)
		if p.expandsPositions() && p.staging == "" {
			missing = append(missing, p.from.CoreNodeName)
		}
	}
	occupied := outgoingPositions(fromClaims)
	for i := range toClaims {
		tc := &toClaims[i]
		if tc.InboundStaging != "" {
			continue
		}
		for _, position := range tc.ToolChangePositions() {
			if !occupied[position] {
				missing = append(missing, tc.CoreNodeName)
				break
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("cannot start changeover: %s marks positions for a tool change, which holds the incoming "+
		"carriers at inbound staging until Tooling done — set inbound staging on the claim for %s",
		strings.Join(marked, ", "), strings.Join(missing, ", "))
}

// applyToolingChangeover is the decorator. It runs LAST, over the finished
// plan, in which the diff pipeline has already planned a marked press position
// by position (shouldFanOutPressIndex) and every other marked cell node by
// node. It makes two edits:
//
//   - every MARKED position whose action lifts the old carrier and brings the
//     new one is given the tool change's leg instead (toolingPositionAction):
//     lift at START, take it out to the outgoing claim's outbound, fetch the
//     replacement, hold it at staging until Tooling done, set it down. A
//     carried-over part whose cell chose outbound_staging walks out and back
//     instead (setCarryoverRoundTrip);
//   - every leg that delivers to a held position (toolChangeHolds) gains a wait
//     at its staging, so the carrier stays out of the cell until the operator
//     releases it.
//
// A marked position the incoming style does not use is a drop, and it is
// cleared at START like the clearing leg (clearDropAtStart), whether the press
// is refilled elsewhere or torn down.
//
// An unmarked position keeps the action the pipeline planned: its ordinary
// changeover, released by its own Ready press. An unmarked drop still waits
// for its Ready.
func applyToolingChangeover(p changeover.Plan, nodes []processes.Node, t toolingChangeover, fallbackAutoConfirm bool) changeover.Plan {
	if !t.active() {
		return p
	}
	cleared := map[string]toolingPress{}
	marked := map[string]bool{}
	for _, press := range t.presses {
		for _, position := range press.positions {
			marked[position] = true
		}
		if !press.expandsPositions() {
			continue // a teardown: nothing comes back, its drops stand as planned
		}
		for _, position := range press.positions {
			cleared[position] = press
		}
	}
	actions := append([]changeover.NodeAction(nil), p.Actions...)
	for i := range actions {
		a := &actions[i]
		if press, ok := cleared[a.CoreNodeName]; ok && liftsAndRefills(a) {
			if node := findNodeByCoreName(nodes, a.CoreNodeName); node != nil {
				*a = toolingPositionAction(press, a.CoreNodeName, node)
				if staging := press.roundTrip[a.CoreNodeName]; staging != "" {
					setCarryoverRoundTrip(a, staging)
				}
			}
			continue
		}
		if marked[a.CoreNodeName] && a.Situation == string(SituationDrop) {
			clearDropAtStart(a)
		}
		if stage := t.stageAt[a.CoreNodeName]; stage != "" {
			holdInbound(a, stage, fallbackAutoConfirm)
		}
	}
	return changeover.Plan{Actions: actions}
}

// liftsAndRefills reports whether the pipeline planned this node as a carrier
// out and a carrier in: a swap or an evacuation. An Add only brings one (it is
// held, never cleared), a Drop only takes one away, and an errored action is
// left to report its error.
func liftsAndRefills(a *changeover.NodeAction) bool {
	if a.Err != nil {
		return false
	}
	return a.Situation == string(SituationSwap) || a.Situation == string(SituationEvacuate)
}

// toolingPositionAction is ONE marked position's clearance: one robot lifts the bin off
// the position, takes it wherever this cell's bins go (or to the override, if the
// cell named one), fetches the replacement, holds it at staging until the
// operator marks the change done, and sets it down on the position.
//
// This is the shape the sim proved on the floor (2026-08-24), and it is now
// produced HERE rather than by a builder a separate predicate selected.
func toolingPositionAction(press toolingPress, position string, node *processes.Node) changeover.NodeAction {
	// A press position has no claim of its own: it is synthesized from the
	// press's, as the per-position fan-out does. Any other mode marks its own
	// node, whose claims are the leg's as they stand (a keep-staged incoming
	// claim keeps the spot its refill lifts from).
	fromPosition, toPosition := press.from, press.to
	if press.from.IsPressIndex() {
		fromPosition = domain.SynthesizePositionClaim(press.from, position)
		toPosition = domain.SynthesizePositionClaim(press.to, position)
	}

	// The replacement leg is a fresh EMPTY carrier for a produce position, and
	// names the incoming style when the carrier type changes — refillPickup owns
	// both, inside buildToolingEvacSteps.
	steps := buildToolingEvacSteps(position, press.from.OutboundDestination, fromPosition, toPosition, press.staging)
	// The order OPENS by lifting the old bin off the position, so it carries the
	// from-style payload; without it the pickup filters for the new payload and
	// finds no bin (the ALN_001 shape).
	supply := complexSpecWithPayload(position, position, steps, true, fromPosition.PayloadCode)
	// THE LIFT GOES AT START, with no Ready press in front of it, so START
	// does what that press does elsewhere: a position behind a live light
	// curtain gets its curtain wait before the lift (CurtainWaits, S7), and the
	// departing produce count is locked when the leg is created
	// (ClearsAtStart; the applier's lockClearingCount).
	supply.Complex.CurtainWaits = true
	return changeover.NodeAction{
		NodeID:        node.ID,
		NodeName:      node.Name,
		CoreNodeName:  position,
		Situation:     string(SituationEvacuate),
		SupplyOrder:   supply,
		NextState:     domain.NodeTaskStagingRequested,
		LogTag:        "evacuate_staged_position",
		ClearsAtStart: true,
	}
}

// clearDropAtStart is a marked drop's clearance: the position leaves the
// style, so nothing comes back, and its lift goes at START as the clearing
// leg's does. The Ready wait in front of the lift goes; START does what that
// press does elsewhere, as for the clearing leg: a live light curtain holds
// the lift (CurtainWaits) and the departing produce count is locked when the
// leg is created (ClearsAtStart, on the drop's evac leg).
func clearDropAtStart(a *changeover.NodeAction) {
	if a.Err != nil || a.EvacOrder == nil || a.EvacOrder.Complex == nil {
		return
	}
	c := a.EvacOrder.Complex
	c.Steps = slices.DeleteFunc(slices.Clone(c.Steps), func(s protocol.ComplexOrderStep) bool {
		return s.Action == protocol.ActionWait && s.Node == a.CoreNodeName && s.Purpose == string(release.PurposeReady)
	})
	c.CurtainWaits = true
	a.ClearsAtStart = true
}

// pressRoundTrips narrows the changeover-wide round-trip map to one press's
// positions, so the expansion does not have to reach back out to the changeover.
func pressRoundTrips(all map[string]string, positions []string) map[string]string {
	if len(all) == 0 {
		return nil
	}
	var out map[string]string
	for _, position := range positions {
		if staging, ok := all[position]; ok {
			if out == nil {
				out = make(map[string]string, len(positions))
			}
			out[position] = staging
		}
	}
	return out
}

// setCarryoverRoundTrip rewrites one position's action into the carry-over shape: the
// SAME bin out to staging, held there through the setup, and back on the
// tooling-done release.
//
//	pickup(position) -> dropoff(staging) -> wait(staging) -> pickup(staging) -> dropoff(position)
//
// It REPLACES the action's orders rather than editing them, because there is
// nothing here to edit: no outbound destination (the bin is not leaving the
// cell), no refill leg (the bin that comes back is the bin that left), and no
// inbound hold to add (the wait is already the same gate). An evac order, if
// the pipeline planned one, is dropped for the same reason — one robot makes
// this whole trip.
//
// The wait is a station wait at the staging spot, which is what puts this leg
// on the same release as every inbound leg of the press: one tooling-done, and
// the kept bin walks back in beside the new material.
func setCarryoverRoundTrip(a *changeover.NodeAction, staging string) {
	if a == nil || staging == "" {
		return
	}
	position := a.CoreNodeName
	steps := []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: position},
		{Action: protocol.ActionDropoff, Node: staging},
		stationWait(staging, release.PurposeToolingDone),
		{Action: protocol.ActionPickup, Node: staging},
		{Action: protocol.ActionDropoff, Node: position},
	}
	payload := ""
	if a.SupplyOrder != nil && a.SupplyOrder.Complex != nil {
		payload = a.SupplyOrder.Complex.PayloadCode
	}
	a.SupplyOrder = complexSpecWithPayload(position, position, steps, true, payload)
	// It lifts at START too, so a live light curtain holds it as it holds the
	// clearing leg. Its count is not locked there: the bin that leaves is the
	// bin that comes back, its count carrying on.
	a.SupplyOrder.Complex.CurtainWaits = true
	a.ClearsAtStart = false
	a.EvacOrder = nil
	a.Situation = string(SituationEvacuate)
	a.NextState = domain.NodeTaskStagingRequested
	a.LogTag = "carryover_outbound_staging"
}

// holdInbound makes the leg that delivers to this position wait at `staging` first.
//
// The bin is HELD, not parked: the robot keeps it on the deck at the staging
// node and moves in on release, so the release is a short move rather than a
// full fetch. That is why this is a wait step and not a dropoff/pickup pair.
//
// An Add's supply order is a plain retrieve, which has no steps to insert into
// and no way to express a hold — a retrieve's StagingNode is a record, not a
// gate. It becomes the equivalent complex order so the changeover's release can
// reach it.
func holdInbound(a *changeover.NodeAction, staging string, fallbackAutoConfirm bool) {
	if a.SupplyOrder != nil && a.SupplyOrder.Retrieve != nil {
		r := a.SupplyOrder.Retrieve
		steps := []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: r.SourceNode},
			stationWait(staging, release.PurposeToolingDone),
			{Action: protocol.ActionDropoff, Node: r.DeliveryNode},
		}
		if r.RetrieveEmpty && r.SourceNode != "" {
			markInboundEmpty(steps, r.SourceNode, "")
		}
		a.SupplyOrder = complexSpecWithPayload(
			r.DeliveryNode, a.CoreNodeName, steps, r.AutoConfirm, r.PayloadCode)
		_ = fallbackAutoConfirm
		return
	}
	holdComplexInbound(a.SupplyOrder, a.CoreNodeName, staging)
	holdComplexInbound(a.EvacOrder, a.CoreNodeName, staging)
}

// holdComplexInbound inserts the staging wait immediately before the LAST
// dropoff at this position — the step that actually sets the new bin down on it.
//
// A leg whose step before that dropoff is already a Tooling done wait is
// retargeted rather than given a second one: the clearing leg this pass builds
// arrives already held, and a pass that is not idempotent is one nobody can
// re-run. Any other wait there (a Ready, the release of another decision) is
// never overwritten: the hold is inserted after it.
func holdComplexInbound(spec *changeover.OrderSpec, node, staging string) {
	if spec == nil || spec.Complex == nil || staging == "" {
		return
	}
	steps := spec.Complex.Steps
	last := -1
	for i := range steps {
		if steps[i].Action == protocol.ActionDropoff && steps[i].Node == node {
			last = i
		}
	}
	if last < 0 {
		return // this leg does not deliver to the position; nothing inbound to hold
	}
	if last > 0 && steps[last-1].Action == protocol.ActionWait && steps[last-1].Purpose == string(release.PurposeToolingDone) {
		steps[last-1] = stationWait(staging, release.PurposeToolingDone)
		return
	}
	out := make([]protocol.ComplexOrderStep, 0, len(steps)+1)
	out = append(out, steps[:last]...)
	out = append(out, stationWait(staging, release.PurposeToolingDone))
	out = append(out, steps[last:]...)
	spec.Complex.Steps = out
}
