package engine

import (
	"fmt"
	"sort"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// ── TOOLING IS A DECORATOR, NOT A COMPETITOR ────────────────────────────────
//
// A press whose outgoing claim marks seats is going down for a tool change.
// The rule the floor gave us is one sentence: the marked seats are cleared, the
// new material waits at inbound staging, the operator does the tool change and
// marks it done, and the material moves in.
//
// ── SHINGO DOES NOT TOUCH TOOLING, AND THERE IS NO BAY ──────────────────────
//
// The tool change is human work done at the asset. What shingo owes it is FLOOR
// SPACE and TIMING: get the material off the marked seats quickly so the people
// have room to set up the next run, and hold the incoming material out of the
// cell until they say they are done, rather than delivering it to line nodes a
// human is standing in. Some cells change over without evacuating anything at
// all, because their tool change happens internally.
//
// So CLEARANCE IS NORMAL ROUTING. A marked seat's bin goes wherever that cell's
// bins ordinarily go — its unloader, its buffer, its market — and this pass
// leaves that leg alone. ChangeoverEvacDestination is an OPTIONAL OVERRIDE for
// a cell that wants clearance somewhere else, like a different node group;
// empty is the default and empty means untouched. Whatever it names is an
// ordinary destination with ordinary capacity behaviour. An earlier round read
// this field as a dedicated "tooling bay" and the demo fixture grew a one-slot
// station to match: the second bin of a two-seat press had nowhere to go, and
// robots dwelt on it holding bins that nothing would ever take away.
//
// WHY THIS IS A PASS OVER THE FINISHED PLAN AND NOT A FIFTH FAN-OUT.
//
// It used to be a fan-out — FanOutStagedToolingEvacuation — sitting in the diff
// pipeline beside FanOutPressIndexDifferentBinType, and the two competed. The
// bin-type pass ran first and rewrote the diff's SwapMode to press_position;
// the tooling pass's predicate required two_robot_press_index; so on a press
// that was BOTH marked and changing bin type, tooling silently did nothing.
// Marked seats were never cleared and new material drove into a cell with a
// human in it. No error, no advisory. That is N1, proven on the sim
// 2026-08-24, and the shape it broke is the common one: a press changing
// carrier type is a press changing tooling.
//
// Precedence between passes is the disease. This pass has no precedence to get
// wrong: it runs LAST, it reads the ORIGINAL claims rather than whatever an
// earlier pass rewrote, and it edits legs the pipeline already produced. There
// is no changeover shape it can miss, because it never has to recognise one.
//
// ── THE ONE THING IT DOES CREATE, AND WHY ───────────────────────────────────
//
// For a marked press whose seats did NOT already get one action each, this pass
// expands the press into one action per marked seat (expandMarkedPress). That
// is not a second mechanism sneaking back in — it is the same rule, and it has
// to happen HERE because only this pass knows the press is marked:
//
//	buildPressIndexChangeoverSwap — the whole-cell same-bin-type builder —
//	evacuates ONLY the front position and then INDEXES the paired bin forward
//	onto it. That index motion is exactly what a tool change forbids: the
//	paired bin never leaves the press. Decorating its legs cannot fix that,
//	because the leg that would have to change is a supply leg doing a
//	different job.
//
// So a marked press must reach per-seat granularity however it got here. When
// the bin-type fan-out already split it (different bin type) or the nodes are
// disjoint (Drops and Adds are per-node by construction), the seats already
// have their own actions and this step does nothing.

// toolingPress is one marked press in a changeover.
type toolingPress struct {
	from *processes.NodeClaim // the OUTGOING claim — it owns the seat marks
	to   *processes.NodeClaim // the incoming claim on the SAME node, if any
	// seats are the core nodes of every marked seat the layout actually has.
	seats    []string
	evacDest string
	staging  string
}

// toolingChangeover is the tooling decoration for ONE changeover. The zero
// value is "not a tooling changeover" and decorates nothing.
type toolingChangeover struct {
	presses []toolingPress
	// evacDest maps a MARKED seat's core node to the OVERRIDE destination its
	// bin is redirected to. Only marked seats of a cell that set the override
	// appear at all: with no override there is nothing to redirect, and an
	// unmarked seat keeps whatever the normal machinery gave it either way.
	evacDest map[string]string
	// stageAt maps a press seat's core node to the staging node its INCOMING
	// bin waits at. Every seat of the cell appears, marked or not — the press
	// is down and a human is inside it, so nothing may drive to lineside.
	stageAt map[string]string
}

func (t toolingChangeover) active() bool { return len(t.presses) > 0 }

// pressIndexSeats is every core node a press-index claim occupies, front to
// back. Empty for a claim that is not press-index.
func pressIndexSeats(c *processes.NodeClaim) []string {
	if c == nil || c.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return nil
	}
	var out []string
	for _, n := range []string{c.CoreNodeName, c.PairedCoreNode, c.SecondPairedCoreNode} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// planToolingChangeover reads the ORIGINAL claim lists — not the diffs — and
// answers two questions: which seats have their clearance redirected by an
// override, and which seats hold their incoming material at staging.
//
// Reading the originals is the whole point. The diffs are what the earlier
// passes rewrote, and the field the old predicate keyed on (SwapMode) is one of
// the fields they rewrite.
//
// SCOPE IS THE CHANGEOVER, which is the cell. A changeover belongs to one
// process; if that process's outgoing style marks seats, the cell is down. The
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
		seats := domain.MarkedEvacSeatNodes(fc)
		if len(seats) == 0 {
			continue
		}
		// THE OVERRIDE IS THE ONLY REASON TO TOUCH AN OUTBOUND LEG, and it is
		// empty by default. Clearing a seat is not a different journey from
		// evacuating it — it is the same journey, made to happen now — so with
		// no override the leg the pipeline planned is exactly right and this
		// pass leaves it alone. Mapping every marked seat to the destination it
		// already had would read as harmless and be one config change away from
		// steering legs nobody asked it to steer.
		if override := fc.ChangeoverEvacDestination; override != "" {
			if t.evacDest == nil {
				t.evacDest = make(map[string]string)
			}
			for _, seat := range seats {
				t.evacDest[seat] = override
			}
		}
		t.presses = append(t.presses, toolingPress{
			from:  fc,
			to:    toByNode[fc.CoreNodeName],
			seats: seats,
			// The EXPANSION builds a leg from nothing, so it has to name a
			// destination: the override when set, this cell's ordinary outbound
			// otherwise. That is EvacDestinationFor's whole job, and it is the
			// one caller that still wants the fallback.
			evacDest: domain.EvacDestinationFor(fc),
		})
	}
	if !t.active() {
		return toolingChangeover{}
	}

	// The staging set: every seat of every incoming press-index claim that
	// names a staging node. The arm gate has already refused a marked press
	// whose incoming claims name none, so an empty set here means the incoming
	// style has no press at all — a cell being emptied, with nothing to hold.
	for i := range toClaims {
		tc := &toClaims[i]
		if tc.InboundStaging == "" {
			continue
		}
		for _, seat := range pressIndexSeats(tc) {
			if t.stageAt == nil {
				t.stageAt = make(map[string]string)
			}
			t.stageAt[seat] = tc.InboundStaging
		}
	}
	// Each marked press remembers a staging node for the seats it expands. Its
	// own node first; otherwise any the changeover declared, taken in sorted
	// order so a multi-press cell plans the same way twice.
	fallback := ""
	if len(t.stageAt) > 0 {
		keys := make([]string, 0, len(t.stageAt))
		for k := range t.stageAt {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fallback = t.stageAt[keys[0]]
	}
	for i := range t.presses {
		if s := t.stageAt[t.presses[i].from.CoreNodeName]; s != "" {
			t.presses[i].staging = s
			continue
		}
		t.presses[i].staging = fallback
	}
	return t
}

// resolveToolingSeatNodes makes sure every marked seat is a node this plan can
// name, and it is the fix for N1-a.
//
// A marked seat owns no claim row, so nothing had ever created its
// process_nodes row except ChangeoverService.Create — which runs after the plan
// is built. The first changeover of a press therefore planned around a seat
// that did not exist yet, and the second one worked because the first had left
// the row behind.
//
// materialize splits the two callers. Start writes the rows here, BEFORE
// planning, and re-reads so the ids are real. Preview must not write, so it
// carries the same seats as unsaved nodes and shows the operator the work the
// changeover will actually do.
//
// The refusal is LOUD, and deliberately so: a marked seat this cannot resolve
// is a seat whose bin stays in the way of people setting up a press, and the
// silent version of that sentence is the whole reason N1 existed.
func (e *Engine) resolveToolingSeatNodes(processID int64, t toolingChangeover,
	nodes []processes.Node, materialize bool) ([]processes.Node, error) {

	seats := toolingSeatNodes(t)
	if len(seats) == 0 {
		return nodes, nil
	}
	have := make(map[string]bool, len(nodes))
	for i := range nodes {
		have[nodes[i].CoreNodeName] = true
	}
	created := false
	for _, seat := range seats {
		if have[seat] {
			continue
		}
		have[seat] = true
		if !materialize {
			nodes = append(nodes, processes.Node{
				ProcessID:    processID,
				CoreNodeName: seat,
				Code:         seat,
				Name:         seat,
				Enabled:      true,
			})
			continue
		}
		if _, err := e.db.CreateProcessNode(processes.NodeInput{
			ProcessID:    processID,
			CoreNodeName: seat,
			Code:         seat,
			Name:         seat,
			Enabled:      true,
		}); err != nil {
			return nil, fmt.Errorf("cannot start changeover: seat %s is marked for changeover "+
				"clearance but has no node on this process, and one could not be created: %w", seat, err)
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

// expandsSeats reports whether this press's marked seats need actions built for
// them — the same question expandMarkedPress answers, asked before the plan
// exists so the seats can be given node rows and node tasks to hang off.
//
// A press with no incoming claim on its own node is a teardown: there is
// nothing to refill with, expandMarkedPress declines to touch it, and inventing
// tasks for seats that will get no orders would block the cutover on work
// nobody planned.
func (p toolingPress) expandsSeats() bool { return p.to != nil }

// toolingSeatNodes is every marked seat of the changeover, deduped, in press
// order. These are the nodes the changeover physically acts on and the ones the
// plan must be able to name.
func toolingSeatNodes(t toolingChangeover) []string {
	var out []string
	seen := make(map[string]bool)
	for _, press := range t.presses {
		for _, seat := range press.seats {
			if seen[seat] {
				continue
			}
			seen[seat] = true
			out = append(out, seat)
		}
	}
	return out
}

// appendToolingSeatTasks gives every marked seat that the diffs did not already
// cover a node task of its own.
//
// WITHOUT THIS THE PLAN IS DISCARDED WHERE IT MATTERS MOST. applyChangeoverPlan
// finds a node task by node id and skips the action when there is none, and the
// task list is built from the DIFFS — which never mention a seat the tooling
// pass expanded. So on a same-bin-type marked press the paired seat's leg was
// planned correctly, logged as "cannot find node task", and dropped.
//
// The seats the diffs DID cover (the bin-type fan-out split them, or they are
// per-node Drops) already have tasks and are left alone, which is the same
// idempotence rule expandMarkedPress follows.
func appendToolingSeatTasks(processID int64, t toolingChangeover, nodeTasks []processes.NodeTaskInput) []processes.NodeTaskInput {
	if !t.active() {
		return nodeTasks
	}
	have := make(map[string]bool, len(nodeTasks))
	for _, nt := range nodeTasks {
		have[nt.CoreNodeName] = true
	}
	for _, press := range t.presses {
		if !press.expandsSeats() {
			continue
		}
		for _, seat := range press.seats {
			if have[seat] {
				continue
			}
			have[seat] = true
			var fromClaimID, toClaimID *int64
			if press.from != nil {
				id := press.from.ID
				fromClaimID = &id
			}
			if press.to != nil {
				id := press.to.ID
				toClaimID = &id
			}
			nodeTasks = append(nodeTasks, processes.NodeTaskInput{
				ProcessID:    processID,
				CoreNodeName: seat,
				FromClaimID:  fromClaimID,
				ToClaimID:    toClaimID,
				Situation:    string(SituationEvacuate),
				State:        "swap_required",
			})
		}
	}
	return nodeTasks
}

// refuseToolingChangeoverWithoutStaging is the arm-time gate: a press marked
// for tooling evacuation stages its incoming bins, so the incoming style has to
// say where.
//
// CHANGEOVER-SCOPED, not diff-scoped. The old gate walked diffs and required
// FromClaim AND ToClaim on the same one, so it could only see a press whose
// node appears in BOTH styles — and said nothing at all about the shape where
// the incoming style runs on different nodes, which is exactly the shape whose
// material would otherwise be delivered into a cell mid tool-change.
//
// Named fields, named cells. "changeover requires inbound staging" sends an
// engineer to the wrong page on a line with six presses.
func refuseToolingChangeoverWithoutStaging(fromClaims, toClaims []processes.NodeClaim) error {
	var marked []string
	for i := range fromClaims {
		if len(domain.MarkedEvacSeatNodes(&fromClaims[i])) > 0 {
			marked = append(marked, fromClaims[i].CoreNodeName)
		}
	}
	if len(marked) == 0 {
		return nil // not a tooling changeover; none of this pass's business
	}

	var incoming, missing []string
	for i := range toClaims {
		tc := &toClaims[i]
		if len(pressIndexSeats(tc)) == 0 {
			continue
		}
		incoming = append(incoming, tc.CoreNodeName)
		if tc.InboundStaging == "" {
			missing = append(missing, tc.CoreNodeName)
		}
	}
	// No incoming press at all: the cell is being emptied and there is nothing
	// to stage. Refusing here would block a legitimate teardown.
	if len(incoming) == 0 {
		return nil
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("cannot start changeover: %s marks press seats for tooling evacuation, "+
		"which stages the incoming bins — set Inbound Staging on the incoming style's claim for %s",
		strings.Join(marked, ", "), strings.Join(missing, ", "))
}

// applyToolingChangeover is the decorator. It runs LAST, over the finished
// plan, and makes two edits:
//
//   - the OUTBOUND leg of a marked seat is redirected, but ONLY when the cell
//     set ChangeoverEvacDestination — otherwise clearance is normal routing and
//     the leg is left exactly as the pipeline planned it;
//   - every INBOUND leg to a press seat gains a wait at inbound staging, so the
//     bin holds out of the cell until the operator releases it.
//
// Plus the per-seat expansion described at the top of this file, for a marked
// press the pipeline planned as one whole cell.
func applyToolingChangeover(p changeover.Plan, nodes []processes.Node, t toolingChangeover, fallbackAutoConfirm bool) changeover.Plan {
	if !t.active() {
		return p
	}
	actions := p.Actions
	for _, press := range t.presses {
		actions = expandMarkedPress(actions, nodes, press)
	}
	for i := range actions {
		a := &actions[i]
		if dest := t.evacDest[a.CoreNodeName]; dest != "" {
			retargetOutbound(a.SupplyOrder, a.CoreNodeName, dest)
			retargetOutbound(a.EvacOrder, a.CoreNodeName, dest)
		}
		if stage := t.stageAt[a.CoreNodeName]; stage != "" {
			holdInbound(a, stage, fallbackAutoConfirm)
		}
	}
	return changeover.Plan{Actions: actions}
}

// expandMarkedPress gives a marked press one action per marked seat when the
// pipeline left it as a single whole-cell action. See the file header for why
// this cannot be decoration.
//
// Idempotent by construction: if every marked seat already has an action — the
// different-bin-type fan-out split it, or the seats are per-node Drops — there
// is nothing missing and the plan is returned untouched.
func expandMarkedPress(actions []changeover.NodeAction, nodes []processes.Node, press toolingPress) []changeover.NodeAction {
	covered := make(map[string]bool, len(actions))
	for _, a := range actions {
		covered[a.CoreNodeName] = true
	}
	missing := false
	for _, seat := range press.seats {
		if !covered[seat] {
			missing = true
			break
		}
	}
	if !missing {
		return actions
	}
	// The incoming claim is what the replacement bin is fetched against. With
	// no incoming claim on this press there is nothing to refill with, so the
	// whole-cell action — a teardown — is left exactly as planned.
	if press.to == nil {
		return actions
	}

	out := make([]changeover.NodeAction, 0, len(actions)+len(press.seats))
	for _, a := range actions {
		// Drop the whole-cell action; its seats are about to replace it.
		if a.CoreNodeName == press.from.CoreNodeName {
			continue
		}
		out = append(out, a)
	}
	for _, seat := range press.seats {
		if covered[seat] && seat != press.from.CoreNodeName {
			continue // already has its own action
		}
		node := findNodeByCoreName(nodes, seat)
		if node == nil {
			continue // the process does not have this seat as a node
		}
		out = append(out, toolingSeatAction(press, seat, node))
	}
	return out
}

// toolingSeatAction is ONE marked seat's clearance: one robot lifts the bin off
// the seat, takes it wherever this cell's bins go (or to the override, if the
// cell named one), fetches the replacement, holds it at staging until the
// operator marks the change done, and sets it down on the seat.
//
// This is the shape the sim proved on the floor (2026-08-24), and it is now
// produced HERE rather than by a builder a separate predicate selected.
func toolingSeatAction(press toolingPress, seat string, node *processes.Node) changeover.NodeAction {
	fromSeat := domain.SynthesizePressPositionClaim(press.from, seat)
	toSeat := domain.SynthesizePressPositionClaim(press.to, seat)

	steps := buildToolingEvacSteps(seat, press.evacDest, toSeat.InboundSource, press.staging)
	// A produce seat's replacement is a fresh EMPTY carrier, not a full
	// payload-matched bin; without this the pickup hunts a full bin in the
	// empty pool and the dispatch fails ("no bin of requested payload").
	if toSeat.Role == protocol.ClaimRoleProduce && toSeat.InboundSource != "" {
		markInboundEmpty(steps, toSeat.InboundSource)
	}
	return changeover.NodeAction{
		NodeID:       node.ID,
		NodeName:     node.Name,
		CoreNodeName: seat,
		Situation:    string(SituationEvacuate),
		// The order OPENS by lifting the old bin off the seat, so it carries the
		// from-style payload; without it the pickup filters for the new payload
		// and finds no bin (the ALN_001 shape).
		SupplyOrder: complexSpecWithPayload(seat, seat, steps, true, fromSeat.PayloadCode),
		NextState:   domain.NodeTaskStagingRequested,
		LogTag:      "evacuate_staged_seat",
	}
}

// retargetOutbound rewrites the dropoff that disposes of the bin lifted off
// `node` so it lands at `dest`.
//
// "The dropoff after the pickup at my own node" is the definition of an
// outbound leg here, and it is deliberately positional rather than a match on
// the old destination: an overriding cell sends its marked seats' bins to the
// override whatever the normal machinery had chosen, including a destination
// this pass has already set (which makes it idempotent).
func retargetOutbound(spec *changeover.OrderSpec, node, dest string) {
	if spec == nil || spec.Complex == nil || dest == "" {
		return
	}
	steps := spec.Complex.Steps
	for i := range steps {
		if steps[i].Action != protocol.ActionPickup || steps[i].Node != node {
			continue
		}
		for j := i + 1; j < len(steps); j++ {
			if steps[j].Action == protocol.ActionDropoff {
				steps[j].Node = dest
				return
			}
		}
		return
	}
}

// holdInbound makes the leg that delivers to this seat wait at `staging` first.
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
			stationWait(staging),
			{Action: protocol.ActionDropoff, Node: r.DeliveryNode},
		}
		if r.RetrieveEmpty && r.SourceNode != "" {
			markInboundEmpty(steps, r.SourceNode)
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
// dropoff at this seat — the step that actually sets the new bin down on it.
//
// A leg that already waits there is retargeted rather than given a second wait:
// the fused seat shape this pass produces itself arrives already correct, and a
// pass that is not idempotent is one nobody can re-run.
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
		return // this leg does not deliver to the seat; nothing inbound to hold
	}
	if last > 0 && steps[last-1].Action == protocol.ActionWait {
		steps[last-1] = stationWait(staging)
		return
	}
	out := make([]protocol.ComplexOrderStep, 0, len(steps)+1)
	out = append(out, steps[:last]...)
	out = append(out, stationWait(staging))
	out = append(out, steps[last:]...)
	spec.Complex.Steps = out
}
