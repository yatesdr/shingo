// operator_changeover_cancel.go — cancel an active changeover, with an
// optional redirect to a different target style.

package engine

import (
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
	// one's are to be left empty — and the changeover's plan left out, since
	// nothing lifts a spare now.
	origin := e.changeoverOrigin(changeover.ID)
	spots := e.cancelledChangeoverSpots(processID, changeover)
	// Under the spots' cell locks from the abort to the reconcile: the target
	// style clears in between, and a request on one of those lines may run again
	// from then. Released before a redirect, whose start takes them itself.
	unlock := e.lockKeepStagedCells(spotLines(spots))
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()
	flows := e.abortSpotOrdersNotFlown(spots)

	nodeTasks, _ := e.db.ListChangeoverNodeTasks(changeover.ID)
	// The line's bin an aborted leg left parked on its way out, and the incoming
	// bin its stage left waiting for it (changeover_cancel_park.go). A keep-staged
	// spot is the spots' own business, decided below.
	var parks []parkedBin
	for _, task := range nodeTasks {
		parks = append(parks, withoutSpots(e.taskLeftOnStaging(task), spots)...)
		if err := e.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskCancelled); err != nil {
			log.Printf("changeover: update node task %d state to cancelled: %v", task.ID, err)
		}
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
		e.reconcileActiveBinAfterCancel(task.ProcessNodeID, runtime.ActiveClaimID)
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
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}

	// Redirect — start new changeover immediately to a different target style
	// A redirect starts the next changeover at once, and its start reconciles
	// every spot itself; the style being reverted to is not staying.
	//
	// The parks ride the spots' read; a redirect, which reads no spots here,
	// reads the parks alone, and only when there are any.
	var parkRows map[string]NodeBinInfo
	if nextStyleID == nil || *nextStyleID == 0 {
		var reads map[string]spotRead
		reads, parkRows = e.readSpotsAnd(spots, parkNames(parks))
		e.applyChangeoverSpots(spots, reads, flows, origin)
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

// reconcileActiveBinAfterCancel re-binds a node's active-bin pointer to the bin
// physically at its slot after a changeover cancel, so a stale pointer (old bin
// evac'd/moved, or operator material swap) can't keep absorbing consume ticks.
// The companion handler_bin_picked_up fix clears the pointer when a clean
// pickup event names the active bin; this covers the rest — an evac whose bin
// departed without a slot pickup event reaching Edge, and the manual-swap case
// where physical reality diverged from Edge's cache.
//
// Re-resolve from Core's physical bin-at-node (BinAtLineside tri-state):
//   - bin present    → rebind to it with Core's authoritative count + epoch so
//     ticks land on the real bin.
//   - confirmed empty → clear the pointer + zero the cache so ticks are held
//     until the next delivery instead of charging a ghost.
//   - Core unverified → retain the prior value (a transient blip must not zero
//     a live lineside).
//
// Best-effort and defensive: every error is logged, never returned — a
// reconcile failure must not block the cancel. The claim pointer is threaded
// through unchanged; the cancel has already reverted the process to the
// from-style, and the active claim was never flipped (cutover never ran).
func (e *Engine) reconcileActiveBinAfterCancel(processNodeID int64, activeClaimID *int64) {
	if e.coreClient == nil {
		return // no Core client wired (e.g. test contexts) — nothing to reconcile against
	}
	node, err := e.db.GetProcessNode(processNodeID)
	if err != nil || node == nil {
		return
	}
	bin, known, err := e.coreClient.BinAtLineside(node.CoreNodeName)
	if err != nil || !known {
		log.Printf("changeover cancel: active-bin reconcile skipped for node %s (core unverified): %v",
			node.Name, err)
		return
	}
	if bin != nil {
		binID := bin.BinID
		// Rebind through the verb: count + stamp from Core's physical view.
		if e.inventoryDelta != nil {
			if err := e.inventoryDelta.BindFromCore(processNodeID, activeClaimID, &binID, bin.DeltaEpoch, bin.UOPRemaining); err != nil {
				log.Printf("changeover cancel: rebind active bin %d for node %s: %v", bin.BinID, node.Name, err)
			}
		}
		return
	}
	// Confirmed empty: the carrier left. Pointer, count and identity go
	// through the one verb (carrier_left.go); no sink, no write.
	if err := e.carrierLeft(processNodeID, node.CoreNodeName); err != nil {
		log.Printf("changeover cancel: clear active bin for node %s: %v", node.Name, err)
	}
}
