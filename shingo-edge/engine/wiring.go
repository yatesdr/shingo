package engine

import (
	"log"

	"shingoedge/orders"
	"shingoedge/store"
)

// wireEventHandlers keeps process ownership in Edge and updates process-node runtime
// from order lifecycle events. Counter deltas still feed hourly production.
func (e *Engine) wireEventHandlers() {
	e.Events.SubscribeTypes(func(evt Event) {
		if delta, ok := evt.Payload.(CounterDeltaEvent); ok {
			e.hourlyTracker.HandleDelta(delta)
			e.handleCounterDelta(delta)
		}
	}, EventCounterDelta)

	e.Events.SubscribeTypes(func(evt Event) {
		if completed, ok := evt.Payload.(OrderCompletedEvent); ok {
			e.handleNodeOrderCompleted(completed)
		}
	}, EventOrderCompleted)

	e.Events.SubscribeTypes(func(evt Event) {
		if failed, ok := evt.Payload.(OrderFailedEvent); ok {
			e.handleNodeOrderFailed(failed)
		}
	}, EventOrderFailed)

	e.Events.SubscribeTypes(func(evt Event) {
		if changed, ok := evt.Payload.(OrderStatusChangedEvent); ok {
			e.handleSequentialBackfill(changed)
		}
	}, EventOrderStatusChanged)
}

// handleCounterDelta processes a production counter tick:
// - For consume nodes: decrement remaining UOP, trigger auto-reorder if at threshold
// - For produce nodes: increment remaining UOP, trigger auto-relief if at capacity
func (e *Engine) handleCounterDelta(delta CounterDeltaEvent) {
	if delta.ProcessID == 0 || delta.StyleID == 0 || delta.Delta <= 0 {
		return
	}
	if delta.Anomaly == "reset" {
		return
	}

	nodes, err := e.db.ListProcessNodesByProcess(delta.ProcessID)
	if err != nil {
		return
	}
	for _, node := range nodes {
		runtime, err := e.db.GetProcessNodeRuntime(node.ID)
		if err != nil || runtime == nil {
			continue
		}

		// Look up active claim for this node
		claim := e.findActiveClaim(&node)
		if claim == nil {
			continue
		}
		// Only process nodes with a claim matching this style
		if claim.StyleID != delta.StyleID {
			continue
		}

		switch claim.Role {
		case "bin_loader":
			continue // bin_loader nodes are operator-driven, not counter-driven
		case "consume":
			newRemaining := runtime.RemainingUOP - int(delta.Delta)
			if newRemaining < 0 {
				newRemaining = 0
			}
			_ = e.db.UpdateProcessNodeUOP(node.ID, newRemaining)

			// Auto-reorder if threshold reached, enabled, and no changeover active
			if claim.AutoReorder && newRemaining <= claim.ReorderPoint &&
				runtime.ActiveOrderID == nil && newRemaining > 0 {
				// Don't auto-reorder during an active changeover — changeover owns material movement
				if _, coErr := e.db.GetActiveProcessChangeover(delta.ProcessID); coErr != nil {
					_, err := e.RequestNodeMaterial(node.ID, 1)
					if err != nil {
						log.Printf("auto-reorder for node %s: %v", node.Name, err)
					}
				}
			}

		case "produce":
			newRemaining := runtime.RemainingUOP + int(delta.Delta)
			_ = e.db.UpdateProcessNodeUOP(node.ID, newRemaining)

			// Auto-relief at capacity (skip during active changeover)
			if claim.AutoReorder && claim.UOPCapacity > 0 &&
				newRemaining >= claim.UOPCapacity && runtime.ActiveOrderID == nil {
				if _, coErr := e.db.GetActiveProcessChangeover(delta.ProcessID); coErr != nil {
					_, err := e.ReleaseNodeEmpty(node.ID)
					if err != nil {
						log.Printf("auto-relief for produce node %s: %v", node.Name, err)
					}
				}
			}
		}
	}
}

func (e *Engine) handleNodeOrderCompleted(completed OrderCompletedEvent) {
	if completed.ProcessNodeID == nil {
		return
	}
	order, err := e.db.GetOrder(completed.OrderID)
	if err != nil {
		return
	}
	node, err := e.db.GetProcessNode(*completed.ProcessNodeID)
	if err != nil {
		return
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(node.ID)
	if err != nil {
		return
	}

	var changeoverID *int64
	if changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
		changeoverID = &changeover.ID
	}
	var nodeTask *store.ChangeoverNodeTask
	if changeoverID != nil {
		if t, err := e.db.GetChangeoverNodeTaskByNode(*changeoverID, node.ID); err == nil {
			nodeTask = t
		}
	}

	// Staged delivery during runout phase.
	if nodeTask != nil && nodeTask.NextMaterialOrderID != nil && *nodeTask.NextMaterialOrderID == order.ID {
		if toClaim, err := e.db.GetStyleNodeClaimByNode(e.getChangeoverToStyleID(node.ProcessID), node.CoreNodeName); err == nil {
			if toClaim.InboundStaging != "" && order.DeliveryNode == toClaim.InboundStaging {
				claimID := toClaim.ID
				_ = e.db.SetProcessNodeRuntime(node.ID, &claimID, runtime.RemainingUOP)
				_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staged")
				return
			}
		}
	}

	// Empty line / clear access for tool change.
	if nodeTask != nil && nodeTask.OldMaterialReleaseOrderID != nil && *nodeTask.OldMaterialReleaseOrderID == order.ID &&
		order.OrderType == orders.TypeMove {
		_ = e.db.SetProcessNodeRuntime(node.ID, runtime.ActiveClaimID, 0)
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "line_cleared")
		return
	}

	// Release staged or replenished material into production.
	if nodeTask != nil && nodeTask.NextMaterialOrderID != nil && *nodeTask.NextMaterialOrderID == order.ID {
		if toClaim, err := e.db.GetStyleNodeClaimByNode(e.getChangeoverToStyleID(node.ProcessID), node.CoreNodeName); err == nil {
			claimID := toClaim.ID
			_ = e.db.SetProcessNodeRuntime(node.ID, &claimID, toClaim.UOPCapacity)
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released")
			_ = e.tryCompleteProcessChangeover(node.ProcessID)
			return
		}
	}

	// Normal replenishment completion — reset UOP from active claim
	if order.OrderType == orders.TypeRetrieve || order.OrderType == orders.TypeComplex {
		if claim := e.findActiveClaim(node); claim != nil {
			claimID := claim.ID
			_ = e.db.SetProcessNodeRuntime(node.ID, &claimID, claim.UOPCapacity)

			// Keep-staged: immediately pre-populate inbound staging for next swap
			e.maybePreStage(node, claim)
		}
	}
}

// maybePreStage orders the next bin to inbound staging if the claim has
// keep_staged enabled. This ensures the staging node always has material
// ready for a fast swap.
func (e *Engine) maybePreStage(node *store.ProcessNode, claim *store.StyleNodeClaim) {
	if !claim.KeepStaged || claim.InboundStaging == "" {
		return
	}
	steps := BuildStageSteps(claim)
	if steps == nil {
		return
	}
	nodeID := node.ID
	order, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.InboundStaging, steps)
	if err != nil {
		log.Printf("keep-staged pre-stage for node %s: %v", node.Name, err)
		return
	}
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &order.ID)
}

// getChangeoverToStyleID returns the to_style_id of the active changeover for a process.
func (e *Engine) getChangeoverToStyleID(processID int64) int64 {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return 0
	}
	return changeover.ToStyleID
}

func (e *Engine) handleNodeOrderFailed(failed OrderFailedEvent) {
	order, err := e.db.GetOrder(failed.OrderID)
	if err != nil || order.ProcessNodeID == nil {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil {
		return
	}

	// IMPORTANT: Do NOT clear the failed order from runtime tracking.
	// Keeping the order ID prevents auto-reorder from re-triggering in a loop.
	// The operator must use the material page to manually clear and retry.

	// If this order was part of a changeover, mark node task as failed (requires manual retry)
	changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil {
		return
	}
	nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, node.ID)
	if err != nil {
		return
	}
	if (nodeTask.NextMaterialOrderID != nil && *nodeTask.NextMaterialOrderID == order.ID) ||
		(nodeTask.OldMaterialReleaseOrderID != nil && *nodeTask.OldMaterialReleaseOrderID == order.ID) {
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error")
		log.Printf("changeover: order failed for node %s, marked as error — manual retry needed", node.Name)
	}
}

// handleSequentialBackfill watches for sequential Order A going in_transit
// and auto-creates Order B (backfill) to deliver replacement material.
func (e *Engine) handleSequentialBackfill(changed OrderStatusChangedEvent) {
	if changed.NewStatus != "in_transit" || changed.ProcessNodeID == nil {
		return
	}
	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil || order.ProcessNodeID == nil {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil {
		return
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(node.ID)
	if err != nil {
		return
	}

	// Only act on the active order (Order A) for this node
	if runtime.ActiveOrderID == nil || *runtime.ActiveOrderID != order.ID {
		return
	}
	// Don't create backfill if one already exists
	if runtime.StagedOrderID != nil {
		return
	}

	claim := e.findActiveClaim(node)
	if claim == nil || claim.SwapMode != "sequential" {
		return
	}

	steps := BuildSequentialBackfillSteps(claim)
	nodeID := node.ID
	orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.CoreNodeName, steps) // delivery_node = CoreNodeName → resets UOP
	if err != nil {
		log.Printf("sequential backfill for node %s: %v", node.Name, err)
		return
	}
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, runtime.ActiveOrderID, &orderB.ID)
	log.Printf("sequential backfill: created Order B %d for node %s (Order A %d in_transit)", orderB.ID, node.Name, order.ID)
}
