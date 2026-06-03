// operator_changeover_cancel.go — cancel an active changeover, with an
// optional redirect to a different target style.

package engine

import (
	"log"

	"shingoedge/domain"
	"shingoedge/orders"
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
	nodeTasks, _ := e.db.ListChangeoverNodeTasks(changeover.ID)
	for _, task := range nodeTasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			order, err := e.db.GetOrder(*orderID)
			if err != nil {
				continue
			}
			if orders.IsTerminal(order.Status) {
				continue
			}
			if err := e.orderMgr.AbortOrder(order.ID); err != nil {
				log.Printf("changeover cancel: abort order %s: %v", order.UUID, err)
			}
		}
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
		if err := e.db.UpdateProcessNodeRuntimeOrders(task.ProcessNodeID, nil, nil); err != nil {
			log.Printf("changeover: update runtime orders for node %d: %v", task.ProcessNodeID, err)
		}
		e.reconcileActiveBinAfterCancel(task.ProcessNodeID, runtime.ActiveClaimID)
	}

	if err := e.db.UpdateProcessChangeoverState(changeover.ID, domain.ChangeoverCancelled); err != nil {
		return err
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}

	// Redirect — start new changeover immediately to a different target style
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
//                      ticks land on the real bin.
//   - confirmed empty → clear the pointer + zero the cache so ticks are held
//                      until the next delivery instead of charging a ghost.
//   - Core unverified → retain the prior value (a transient blip must not zero
//                      a live lineside).
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
		if err := e.db.SetProcessNodeRuntimeWithBinAndEpoch(processNodeID, activeClaimID, &binID, bin.DeltaEpoch, bin.UOPRemaining); err != nil {
			log.Printf("changeover cancel: rebind active bin %d for node %s: %v", bin.BinID, node.Name, err)
		}
		return
	}
	if err := e.db.SetProcessNodeRuntimeWithBin(processNodeID, activeClaimID, nil, 0); err != nil {
		log.Printf("changeover cancel: clear active bin for node %s: %v", node.Name, err)
	}
}
