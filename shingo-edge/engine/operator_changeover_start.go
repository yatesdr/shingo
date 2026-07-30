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
// it only by accident). This cancels BY STATUS, scoped to protocol.IsPreDispatch,
// which provably cannot hold a carrier. Same operation, different predicate,
// and the predicate is the entire difference.
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

	reason := "cancelled at changeover start — the line is changing over"
	for _, diff := range plan.diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(plan.nodes, diff.CoreNodeName)
		if node == nil {
			continue
		}
		runtime, err := e.db.GetProcessNodeRuntime(node.ID)
		if err != nil || runtime == nil {
			continue
		}
		active, staged := runtime.ActiveOrderID, runtime.StagedOrderID
		for _, slot := range []*int64{active, staged} {
			if slot == nil {
				continue
			}
			order, err := e.db.GetOrder(*slot)
			if err != nil || order == nil || !protocol.IsPreDispatch(order.Status) {
				continue
			}
			abort(order.ID, reason)
			// BOTH LEGS OR NEITHER. A complex consume request is created as a
			// linked pair; cancelling one leg leaves the survivor parked on
			// waiting_for_partner for a partner that will never claim a bin,
			// which is a worse state than the one being cleared. The sibling is
			// aborted even when it is not in a slot of its own.
			if order.SiblingOrderID != nil {
				abort(*order.SiblingOrderID, reason+" (sibling of order "+itoa64(order.ID)+")")
			}
		}
		// Clear only the pointers whose orders we terminated. A stale pointer to
		// a cancelled order is the phantom-badge family, and clearing both slots
		// unconditionally would drop a live order's pointer alongside it.
		newActive, newStaged := active, staged
		if newActive != nil && seen[*newActive] {
			newActive = nil
		}
		if newStaged != nil && seen[*newStaged] {
			newStaged = nil
		}
		if newActive != active || newStaged != staged {
			if err := e.db.UpdateProcessNodeRuntimeOrders(node.ID, newActive, newStaged); err != nil {
				e.logFn("changeover: clear runtime slots for node %s after cancel: %v", node.Name, err)
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
	var blockers []string
	for _, diff := range plan.diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(plan.nodes, diff.CoreNodeName)
		if node == nil {
			continue
		}
		runtime, err := e.db.GetProcessNodeRuntime(node.ID)
		if err != nil || runtime == nil {
			continue
		}
		for _, orderID := range []*int64{runtime.ActiveOrderID, runtime.StagedOrderID} {
			if orderID == nil {
				continue
			}
			order, err := e.db.GetOrder(*orderID)
			if err != nil || order == nil || !protocol.BlocksChangeoverStart(order.Status) {
				continue
			}
			blockers = append(blockers, changeoverBlockerFor(node.Name, order.ID, order.Status))
		}
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
	plan, err := e.planChangeover(processID, toStyleID)
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
	orderPlan := BuildChangeoverPlan(plan.diffs, plan.nodes, e.cfg.Web.AutoConfirm, e.activePullSnapshot(plan.nodes))
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
