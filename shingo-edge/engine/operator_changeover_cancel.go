// operator_changeover_cancel.go — cancel an active changeover, with an
// optional redirect to a different target style.

package engine

import (
	"errors"
	"log"
	"shingo/protocol"

	"shingoedge/domain"
)

func (e *Engine) CancelProcessChangeover(processID int64) error {
	return e.cancelProcessChangeoverInternal(processID, nil)
}

// CancelProcessChangeoverRedirect cancels the active changeover and immediately
// starts a new one to a different target style. If nextStyleID is nil, behaves
// identically to CancelProcessChangeover (plain revert).
func (e *Engine) CancelProcessChangeoverRedirect(processID int64, nextStyleID *int64) error {
	return e.cancelProcessChangeoverInternal(processID, nextStyleID)
}

func (e *Engine) cancelProcessChangeoverInternal(processID int64, nextStyleID *int64) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}

	// Abort the orders this changeover created — supply + evac legs per
	// node task. Sibling orders that happen to be on the same nodes
	// (manual storage, replenishment, etc.) are owned by other flows
	// and not the changeover-cancel's business to terminate.
	// The keep-staged spots, put back for the style that stays. Decided with the
	// styles reversed — the outgoing style keeps its spots again, the incoming
	// one's are to be left empty. The cancel sends back a spare the staying style
	// does not want and orders nothing for the spot: the keeper refills it for the
	// staying claim once the changeover is gone (keptFor).
	origin := e.changeoverOrigin(changeover.ID)
	spots := e.cancelledChangeoverSpots(processID, changeover)
	// Under the spots' cell locks from the abort to the reconcile: the target
	// style clears in between, and a request on one of those lines may run again
	// from then. Released before a redirect, whose start takes them itself.
	unlock := e.lockCells(spotLines(spots))
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()

	// THE TASKS ARE CANCELLED BEFORE ANY ORDER IS ABORTED. Each abort runs the
	// completion cascade on this goroutine, under the cells' locks. A task still
	// open there reads to the orphan handler as an operator cancelling one order,
	// and that handler runs the changeover's completion, which confirms delivered
	// legs; a confirm's keep-spot kick takes a cell's lock, which this goroutine
	// already holds, and waits for itself (LOCKS.md, E2). A cancelled task is the
	// cancel's own, and the orphan handler leaves it alone.
	nodeTasks, _ := e.db.ListChangeoverNodeTasks(changeover.ID)
	for _, task := range nodeTasks {
		if err := e.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskCancelled); err != nil {
			log.Printf("changeover: update node task %d state to cancelled: %v", task.ID, err)
		}
	}
	live, aborted := e.abortSpotOrdersNotFlown(spots)

	// The line's bin an aborted leg left parked on its way out, and the incoming
	// bin its stage left waiting for it (changeover_cancel_park.go). A keep-staged
	// spot is the spots' own business, decided below.
	var parks []parkedBin
	for _, task := range nodeTasks {
		parks = append(parks, withoutSpots(e.taskLeftOnStaging(task), spots)...)
	}

	// Clear runtime order references AND reconcile the active-bin pointer for
	// each affected node. The abort may have evac'd (or partially moved) the
	// old bin, and the operator may have manually swapped material — so the
	// cached active_bin_id can no longer be trusted. Left stale, consume ticks
	// keep draining a bin that has left the slot (Springfield 2026-06-02:
	// aborted RH→LH at ALN_003 drained bin 18 in the supermarket until a manual
	// cycle_count). Re-resolve each node against Core's physical bin-at-node.
	for _, task := range nodeTasks {
		runtime, err := e.db.GetProcessNodeRuntime(task.ProcessNodeID)
		if err != nil || runtime == nil {
			continue
		}
		if err := e.db.ClearProcessNodeRuntimeOrders(task.ProcessNodeID); err != nil {
			log.Printf("changeover: update runtime orders for node %d: %v", task.ProcessNodeID, err)
		}
		e.reconcileActiveBinAfterCancel(task.ProcessNodeID)
	}

	if err := e.db.UpdateProcessChangeoverState(changeover.ID, domain.ChangeoverCancelled); err != nil {
		return err
	}
	// The episode ends CANCELLED, not complete. They are different outcomes and
	// merging them would hide every abandoned changeover behind the successful
	// ones. Cancel-and-redirect then inserts a fresh changeover row, which
	// correctly opens a NEW episode rather than continuing this one.
	e.closeChangeoverEpisode(changeover.ID, protocol.CloseReasonCancelled, protocol.ClosedByNotification)
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}

	// Redirect — start new changeover immediately to a different target style
	// A redirect starts the next changeover at once, and its start decides every
	// spot itself; the style being reverted to is not staying.
	//
	// The parks ride the spots' read; a redirect, which reads no spots here,
	// reads the parks alone, and only when there are any.
	var parkRows map[string]protocol.NodeBinsRow
	if nextStyleID == nil || *nextStyleID == 0 {
		var reads map[string]spotRead
		reads, parkRows = e.readSpotsAnd(spots, parkNames(parks))
		reads = readsLessLeaving(reads, live, spots, aborted)
		e.applyChangeoverSpots(spotReturns(spots, reads), reads, origin)
	} else {
		_, parkRows = e.readSpotsAnd(nil, parkNames(parks))
	}
	release()
	// After the spots' locks are let go: a park is decided under its own line's
	// lock, which the spots may already have held.
	e.finishParkedTrips(parks, parkRows, origin)

	if nextStyleID != nil && *nextStyleID != 0 {
		_, err := e.StartProcessChangeover(processID, *nextStyleID,
			"changeover-redirect", "redirected from cancelled changeover")
		return err
	}

	return nil
}

// reconcileActiveBinAfterCancel re-seats a node after a changeover cancel or a
// plain abort, so a stale active-bin pointer (old bin evac'd/moved, or an
// operator material swap) can't keep absorbing consume ticks. The companion
// handler_bin_picked_up fix clears the pointer when a clean pickup event names
// the active bin; this covers the rest — an evac whose bin departed without a
// slot pickup event reaching Edge, and the manual-swap case where physical
// reality diverged from Edge's cache.
//
// IT ASKS CORE TO RESEAT THE NODE (protocol.AskReseat, site 6 of the reads)
// and binds nothing itself: binding happens only on a landing. The answer is
// applied by its kind, waiter or none (askAppliersByKind), so nothing here
// waits on Core:
//   - a carrier at the node → Core re-announces it through the landing verb,
//     ahead of its answer on Core's outbox, and the seat binds on the landing
//     (HandleBinLanded): its stamp is newer than the seat's, the same carrier
//     at an equal or newer generation keeps the seat's own count, and the
//     identity is the landing's;
//   - none there → the answer says so and the carrier left (carrierLeft);
//   - Core not answering the kind, a refusal (a node Core does not know), or
//     no answer → the seat is left as it is (a transient blip must not zero a
//     live lineside).
//
// Best-effort and defensive: every error is logged, never returned — a
// reconcile failure must not block the cancel.
func (e *Engine) reconcileActiveBinAfterCancel(processNodeID int64) {
	node, err := e.db.GetProcessNode(processNodeID)
	if err != nil || node == nil {
		return
	}
	if !e.CoreAnswers(protocol.AskReseat) {
		log.Printf("changeover cancel: active-bin reconcile skipped for node %s (Core does not answer %s)",
			node.Name, protocol.AskReseat)
		return
	}
	if _, err := e.Ask(protocol.AskReseat, protocol.ReseatRequest{Node: node.CoreNodeName}, 0); err != nil &&
		!errors.Is(err, ErrAskPending) {
		log.Printf("changeover cancel: active-bin reconcile skipped for node %s: %v", node.Name, err)
	}
}

// applyReseat is the reseat answer's applier: with no carrier at the node, the
// seat's carrier left (carrierLeft: pointer, count and identity through the
// one verb). A carrier there was bound by its landing, which reached this
// Edge ahead of the answer, so a landed answer changes nothing; a refusal
// leaves the seat as it is.
func applyReseat(e *Engine, a *protocol.EdgeAnswer) {
	r, err := askInto[protocol.ReseatAnswer](a)
	if err != nil {
		log.Printf("changeover cancel: reseat refused: %v — the seat is left as it is", err)
		return
	}
	if r.Landed || r.Node == "" {
		return
	}
	node, err := e.db.GetProcessNodeByCoreNodeName(r.Node)
	if err != nil || node == nil {
		return
	}
	if err := e.carrierLeft(node.ID, node.CoreNodeName); err != nil {
		log.Printf("changeover cancel: clear active bin for node %s: %v", node.Name, err)
	}
}
