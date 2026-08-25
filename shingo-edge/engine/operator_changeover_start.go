// operator_changeover_start.go — kick off a changeover.
//
// StartProcessChangeover runs the preflight gate, calls planChangeover
// (see operator_changeover_plan.go), REFUSES if a participating node still
// has an order in flight, creates the changeover row via changeoverService,
// and emits all robot orders with embedded waits.

package engine

import (
	"context"
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// changeoverBlockerFor renders the operator-facing sentence for one blocking
// order. Each status names its own remedy, because "in flight" is the same
// sentence for four different situations and only one of them is "wait, it is
// coming" — an operator told that about a faulted order waits for a robot that
// is not on its way.
func changeoverBlockerFor(nodeName string, orderID int64, status protocol.Status) string {
	switch status {
	case protocol.StatusStaged:
		return fmt.Sprintf("%s: order %d is staged — a robot is holding a bin there. "+
			"Release the wait, then start the changeover", nodeName, orderID)
	case protocol.StatusFaulted:
		return fmt.Sprintf("%s: order %d faulted mid-move and still holds its bin. "+
			"Needs maintenance or a team lead — it is not clearable from this screen",
			nodeName, orderID)
	default:
		// dispatched / in_transit: a carrier really is on its way. This is the
		// only case where "wait and press again" is the whole answer, and it is
		// the case the old blanket wording was written for.
		return fmt.Sprintf("%s: order %d is %s — a carrier is on its way. "+
			"Wait for it to land, then start the changeover", nodeName, orderID, status)
	}
}

// blockNodeSet is every node the changeover physically ACTS ON: the CHANGED diff
// nodes, plus the indexed_over press-index positions.
//
// The positions are the Hopkinsville blind spot — the removed AbortNodeOrders sweep
// missed orders 1249/1251 "only because it walks plan.diffs (the task nodes)"
// while they were delivering to PLN_02 and PLN_05, and the gate that replaced it
// inherited the same walk. A robot is physically traversing those positions, so
// they block.
//
// SituationUnchanged NODES ARE NOT IN THIS SET, and this is a correction. They
// were, on the claim that "a carrier moving toward an unchanged node of this
// process is still a carrier the changeover will collide with." That is not
// true: at an unchanged node the changeover does nothing — no evacuation, no
// supply leg, no claim change — so a carrier landing there collides with
// nothing. The claim went unchallenged while the gate read runtime pointers,
// which almost never held those orders; keying the gate on destination made it
// see them and turned every cell's ordinary production traffic into a reason a
// changeover elsewhere in the process could not start.
//
// TestScenario_ReleaseIsChangeoverIndependent is the invariant, and it is a real
// product requirement rather than a test artefact: an operator changing over
// node A must not be blocked by a normal order in flight to node B.
//
// This makes the set equal to cancelNodeSet plus the positions — but they stay two
// functions, because the positions are exactly what must never be cancelled.
//
// plan.participants, not ListChangeoverParticipants: the changeover row does not
// exist yet when the gate runs (Create is below, the gate above it), so there is
// no changeoverID to read by. The in-memory set is built in planChangeover.
func blockNodeSet(plan *changeoverPlan) []string {
	changed := make(map[string]bool, len(plan.diffs))
	for _, d := range plan.diffs {
		if d.CoreNodeName == "" || d.Situation == SituationUnchanged {
			continue
		}
		changed[d.CoreNodeName] = true
	}
	out := make([]string, 0, len(plan.participants))
	for _, p := range plan.participants {
		if p.CoreNodeName == "" {
			continue
		}
		if p.Role == domain.ParticipantRoleIndexedOver || changed[p.CoreNodeName] {
			out = append(out, p.CoreNodeName)
		}
	}
	return out
}

// cancelNodeSet is the CHANGED diff nodes only — deliberately narrower than
// blockNodeSet, and they must stay two separately named functions.
//
// Widening the BLOCK to a position is safe: it says "not yet", the operator presses
// again. Widening the CANCEL to one is the Hopkinsville deadlock — and widening
// it to a SituationUnchanged node is a second, quieter bug: the incoming style
// still claims that payload at that node, so the order is still wanted and
// cancelling it would starve the cell the changeover is meant to keep running.
//
// One shared variable here would be one edit away from either.
func cancelNodeSet(plan *changeoverPlan) []string {
	out := make([]string, 0, len(plan.diffs))
	for _, diff := range plan.diffs {
		if diff.Situation == SituationUnchanged || diff.CoreNodeName == "" {
			continue
		}
		out = append(out, diff.CoreNodeName)
	}
	return out
}

// cancelPreDispatchAtParticipants cancels every PRE-DISPATCH order sitting at a
// node this changeover touches, and returns the ids it terminated.
//
// An operator starting a changeover is direct communication that the line is
// going a different direction. A pre-dispatch order has no carrier assigned, so
// leaving it alive serves nobody: it survives the cutover holding a claim on the
// outgoing style's payload, and eventually delivers material to a cell that
// stopped running that style hours ago.
//
// WHY THIS IS SAFE WHERE THE SWEEP IT RESEMBLES WAS NOT. An AbortNodeOrders
// sweep used to run here and was removed (8553178a) because it cancelled BY
// NODE. On a press-index swap the in-flight legs are frequently carrying the
// very empty carriers the changeover's own index legs are about to pick up, and
// cancelling those mid-delivery leaves the index legs waiting for bins that are
// never coming — a permanent deadlock (HK 2026-07-28: orders 1249/1251 escaped
// it only by accident). This cancels BY STATUS, scoped to the statuses
// protocol.ChangeoverStartActionFor calls Cancel, none of which can hold a
// carrier. Same operation, different rule, and the rule is the entire difference.
//
// THE STATUS SET COMES FROM THE SHARED CLASSIFIER, NOT FROM IsPreDispatch. It
// used to come from IsPreDispatch and that was the SNF2 defect of 30 July: that
// predicate answers the fulfillment scanner's question ("is this a retryable
// acquisition state"), which excludes submitted and acknowledged, and an order
// sitting in either when a changeover started survived it. Two styles were then
// live against one node. The classifier answers THIS question and the block gate
// reads the same function, so the two halves cannot disagree about a status.
//
// NO LOADER CARVE-OUT, AND THERE MUST NOT BE ONE. Earlier revisions of this
// design carried a swap-mode clause to spare threshold L1s and operator
// REQUEST EMPTY orders. It was deleted rather than written: loaders do not
// change over. domain.Loader carries no style, no active_style_id and no swap
// mode; plan.diffs is built from style claims, so a loader node cannot enter the
// set in the first place. The invariant is asserted in
// TestChangeoverPlan_ParticipantsNeverContainALoaderNode — an assertion about
// the node set, which is where the truth lives, rather than a special case here
// that would quietly become load-bearing.
//
// Best-effort per order: one failure is logged and the rest proceed. A changeover
// half-cleared is not worse than one not cleared at all, and refusing to start
// over a single stuck abort would put the operator back where they were.
func (e *Engine) cancelPreDispatchAtParticipants(plan *changeoverPlan) ([]int64, error) {
	var cancelled []int64
	seen := map[int64]bool{}

	abort := func(orderID int64, why string) {
		if seen[orderID] {
			return
		}
		seen[orderID] = true
		order, err := e.db.GetOrder(orderID)
		if err != nil || order == nil || protocol.IsTerminal(order.Status) {
			return
		}
		if err := e.orderMgr.AbortOrderWithReason(orderID, why); err != nil {
			e.logFn("changeover: cancel %s order %d at start: %v", order.Status, orderID, err)
			return
		}
		cancelled = append(cancelled, orderID)
	}

	nodes := cancelNodeSet(plan)
	if len(nodes) == 0 {
		return nil, nil
	}
	live, err := e.db.ListActiveOrders()
	if err != nil {
		return nil, err
	}
	reason := "cancelled at changeover start — the line is changing over"
	for i := range live {
		o := live[i]
		if protocol.ChangeoverStartActionFor(o.Status) != protocol.ChangeoverStartCancel {
			continue
		}
		if !e.orderPlacesBinAtAny(o.ID, o.DeliveryNode, nodes) {
			continue
		}
		abort(o.ID, reason)
		// BOTH LEGS OR NEITHER. A complex consume request is created as a linked
		// pair; cancelling one leg leaves the survivor parked on
		// waiting_for_partner for a partner that will never claim a bin, which is
		// a worse state than the one being cleared. The sibling is aborted even
		// when its own destination is not in the set.
		if o.SiblingOrderID != nil {
			abort(*o.SiblingOrderID, reason+" (sibling of order "+itoa64(o.ID)+")")
		}
	}

	// Clear only the runtime pointers whose orders we terminated. A stale pointer
	// to a cancelled order is the phantom-badge family; clearing slots
	// unconditionally would drop a live order's pointer alongside the dead one.
	// Walked separately from the cancel because the destination surface finds
	// orders that were never in a slot — the pointer is UI state, not truth.
	for _, diff := range plan.diffs {
		node := findNodeByCoreName(plan.nodes, diff.CoreNodeName)
		if node == nil {
			continue
		}
		runtime, rerr := e.db.GetProcessNodeRuntime(node.ID)
		if rerr != nil || runtime == nil {
			continue
		}
		newActive, newStaged := runtime.ActiveOrderID, runtime.StagedOrderID
		if newActive != nil && seen[*newActive] {
			newActive = nil
		}
		if newStaged != nil && seen[*newStaged] {
			newStaged = nil
		}
		if newActive != runtime.ActiveOrderID || newStaged != runtime.StagedOrderID {
			if uerr := e.db.UpdateProcessNodeRuntimeOrders(node.ID, newActive, newStaged); uerr != nil {
				e.logFn("changeover: clear runtime slots for node %s after cancel: %v", node.Name, uerr)
			}
		}
	}
	return cancelled, nil
}

// nodesWithOrdersInFlight returns an operator-readable blocker per participating
// node where LIVE CHOREOGRAPHY is still running. Empty means the changeover is
// clear to start.
//
// The predicate is protocol.BlocksChangeoverStart, NOT !IsTerminal. IsTerminal
// is derived from validTransitions, so its complement is eleven statuses, and
// nine of them are not a carrier doing anything: a queued order has no bin
// assigned, a delivered one has already landed and is waiting on a clerical
// confirm, and acknowledged/submitted are Edge-lifecycle words the fleet never
// emits. Blocking on those refused changeovers the operator was standing at the
// line to run, and in the acknowledged case refused them PERMANENTLY — nothing
// reaps that status and this HMI exposes no operator order cancel, so the only
// exit was an Edge restart. See protocol.BlocksChangeoverStart for the set and
// the Hopkinsville reasoning behind it.
//
// Reads the same two runtime pointers the old AbortNodeOrders sweep did
// (ActiveOrderID, StagedOrderID) — the difference is entirely what we do with
// them: report instead of cancel. Unchanged nodes are skipped; the changeover
// does not touch them.
//
// Fail-open on a read error. A flaky SQLite read must not block a changeover the
// operator is standing at the line to run; the resource-level gates (reservations
// and swapLegHeld) are the real safety net and they hold regardless.
func (e *Engine) nodesWithOrdersInFlight(plan *changeoverPlan) []string {
	nodes := blockNodeSet(plan)
	if len(nodes) == 0 {
		return nil
	}
	live, err := e.db.ListActiveOrders()
	if err != nil {
		// Fail OPEN, as this function always has. A flaky read must not block a
		// changeover the operator is standing at the line to run; the
		// resource-level gates (reservations, swapLegHeld) are the real safety
		// net and they hold regardless.
		e.logFn("changeover: list active orders for the start gate: %v", err)
		return nil
	}
	var blockers []string
	for i := range live {
		o := live[i]
		if !protocol.BlocksChangeoverStart(o.Status) {
			continue
		}
		if !e.orderPlacesBinAtAny(o.ID, o.DeliveryNode, nodes) {
			continue
		}
		// Name the place the way the operator's board does. The destination
		// surface matches on core node names, but those are wiring identifiers —
		// resolve back to the process node's display name when we have one, and
		// fall back to the raw name for a node this process does not own (an
		// indexed_over position on another station, say).
		where := o.DeliveryNode
		if node := findNodeByCoreName(plan.nodes, o.DeliveryNode); node != nil && node.Name != "" {
			where = node.Name
		}
		blockers = append(blockers, changeoverBlockerFor(where, o.ID, o.Status))
	}
	return blockers
}

// Error handling policy: log and continue. Do not add early returns without understanding the caller contract. See 2567plandiscussion.md.
func (e *Engine) StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*processes.Changeover, error) {
	// A new changeover resolves any pending post-cutover verification flag on the
	// process — the operator is acting on the mismatch (e.g. the one-tap corrective
	// changeover offered on the flag). Best-effort; a stale flag is cosmetic.
	if err := e.ClearPostCutoverFlag(processID); err != nil {
		log.Printf("changeover: clear post-cutover flag for process %d: %v", processID, err)
	}

	// Pre-flight inventory gate: refuse to start if Core reports any
	// required payload has zero available bins in the supermarket — the
	// changeover would deadlock at the first retrieve. Run BEFORE
	// planning so planning-side side effects (DB writes, robot aborts)
	// don't fire on a doomed start. preflightChecker is wired in tests
	// that don't care about the gate; nil-skip there.
	var awaitingStock []string
	if e.preflightChecker != nil && e.coreClient != nil && e.coreClient.Available() {
		missing, perr := e.preflightChecker.PreflightInventoryCheck(context.Background(), toStyleID)
		if perr != nil {
			return nil, fmt.Errorf("changeover preflight: %w", perr)
		}
		if len(missing) > 0 {
			// Non-blocking advisory (was a HARD REFUSAL before 2026-06-04).
			// Core queues an unsourceable supply retrieve (5eb0a3a) and holds
			// a two-robot swap's removal leg until its supply sibling claims a
			// bin (0d95521), so a changeover started without stock parks its
			// supply legs as "Awaiting Stock" and self-heals once the operator
			// loads + manifest-confirms the material. Refusing here instead
			// dead-ended the operator with idle robots and no course of action
			// (Springfield NF SPOT 3, 2026-06-03). Surface the missing list as
			// advisory and let the changeover proceed.
			awaitingStock = missing
			e.logFn("changeover: process %d → style %d starting with %d payload(s) not yet in stock; supply legs will queue as Awaiting Stock until loaded: %v",
				processID, toStyleID, len(missing), missing)
		}
	}
	plan, err := e.planChangeover(processID, toStyleID, true)
	if err != nil {
		return nil, err
	}

	// Refuse while a participating node still has an order in flight.
	//
	// This replaces an AbortNodeOrders sweep that used to run just below, after
	// the changeover row was created — it cancelled every non-terminal order on
	// the affected nodes to clear the way. That is worse than rude on a
	// press-index swap: those in-flight legs are often carrying the very empty
	// carriers the new changeover's index legs will need to pick up.
	//
	// Hopkinsville 2026-07-28 is the proof, from the bin audit trail. Orders
	// 1249/1251 (steady-state swaps, started 15:29) held bins 2 and 5 — the
	// carriers bound for PLN_02 and PLN_05. The auto-armed changeover fired at
	// 15:53:35 and its index legs could not reserve, because those carriers were
	// still in transit. They landed at 15:56:53 and 15:58:33, and each index leg
	// claimed its bin ONE SECOND later. The reserve loop did its job perfectly.
	//
	// The sweep missed 1249/1251 only because it walks plan.diffs (the task
	// nodes, PLN_01/PLN_04) while those orders were delivering to the
	// indexed_over nodes (PLN_02/PLN_05). Had it caught them, their carriers
	// would have been cancelled mid-delivery and the index legs would have waited
	// for bins that were never coming — a permanent deadlock. The changeover
	// survived by accident.
	//
	// So: don't cancel the operator's work, and don't invent a changeover-level
	// queue either. The only caller left is a human pressing a button (CATID
	// auto-arm no longer starts changeovers, it completes them), and a human can
	// be told "not yet" and press again. Give-up stays operator-driven, per
	// docs/reservations.md.
	// CANCEL FIRST, THEN GATE. Reversed, the gate would refuse on the very orders
	// the cancel is about to remove and the fix would do nothing. Both read the
	// plan, so both run after planChangeover.
	cancelled, cerr := e.cancelPreDispatchAtParticipants(plan)
	if cerr != nil {
		return nil, cerr
	}
	if len(cancelled) > 0 {
		// Named at INFO, not swallowed: cancelling at initiation destroys a
		// request the process may still need if the changeover is later
		// abandoned, and it self-heals only where the claim has auto-reorder.
		// This line is what the abandon path's operator-facing sentence is
		// eventually built from.
		e.logFn("changeover: process %d → style %d cancelled %d pre-dispatch order(s) at start: %v",
			processID, toStyleID, len(cancelled), cancelled)
	}

	if blockers := e.nodesWithOrdersInFlight(plan); len(blockers) > 0 {
		return nil, fmt.Errorf("cannot start changeover — %s", strings.Join(blockers, "; "))
	}

	if _, err := e.changeoverService.Create(processID, plan.process.ActiveStyleID, toStyleID,
		calledBy, notes, plan.stationIDs, plan.nodeTasks, plan.participants, plan.nodes); err != nil {
		return nil, err
	}

	// Retrieve the changeover we just created so we can link node tasks.
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}

	// Create ALL robot orders up front with embedded wait steps.
	// Operator controls flow by releasing waits, not by triggering individual orders.
	orderPlan := BuildChangeoverPlan(plan.diffs, plan.nodes, e.cfg.Web.AutoConfirm, e.activePullSnapshot(plan.nodes), plan.tooling)
	// The changeover's demand episode, opened once the sourcing plan exists —
	// its order count IS expected_orders — and before any order does. Unlike
	// the cell kinds this is an EVENT trigger: the changeover arming is the
	// edge, so there is no level and no hysteresis.
	e.openChangeoverEpisode(changeover, orderPlan.OrderCount())
	e.applyChangeoverPlan(changeover, orderPlan)

	final, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	// Transient advisory — not persisted. Lets the HMI tell the operator
	// which bins to load; the live per-order "Awaiting Stock" status is the
	// durable signal once orders exist.
	final.AwaitingStock = awaitingStock
	// Advisory, never blocking: participants with no process_nodes row. Logged
	// as well as returned so it lands in the record even when the caller is a
	// script that discards the response body.
	final.UnresolvedParticipants = plan.unresolvedParticipants
	if len(plan.unresolvedParticipants) > 0 {
		e.logFn("changeover: process %d → style %d started with %d participant node(s) that have no process_nodes row: %v — these cannot be gated, rendered or released until added on the process-nodes page",
			processID, toStyleID, len(plan.unresolvedParticipants), plan.unresolvedParticipants)
	}
	return final, nil
}

// binEmptyAtCoreNode returns a closure that reports whether the physical
// bin at a CoreNodeName is empty (RemainingUOPCached == 0) for nodes in
// the given process. The reuse-compatible-bins shortcut uses this to
// skip press-index swaps when the next style produces the same payload
// and reuse_compatible_bins is opted in. Errors collapse to "not empty"
// — defensive, never auto-skip a swap on the basis of a runtime read
// failure.
func (e *Engine) binEmptyAtCoreNode(processID int64) func(coreNodeName string) bool {
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return func(string) bool { return false }
	}
	idByName := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		idByName[n.CoreNodeName] = n.ID
	}
	return func(name string) bool {
		id, ok := idByName[name]
		if !ok {
			return false
		}
		rt, err := e.db.GetProcessNodeRuntime(id)
		if err != nil || rt == nil {
			return false
		}
		return rt.RemainingUOPCached == 0
	}
}
