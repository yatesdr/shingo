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
	// A/B fallthrough tracking: if all paired consume nodes are inactive,
	// decrement the first one found as a safety net ("count to lineside storage").
	var pairedFallbackNode *store.ProcessNode
	var pairedFallbackRuntime *store.ProcessNodeRuntimeState
	pairedConsumeHandled := false

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
			// A/B cycling: if this node is part of an A/B pair, only decrement
			// the active-pull side. The inactive side holds staged material.
			if claim.PairedCoreNode != "" && !runtime.ActivePull {
				// Remember first inactive paired node as fallback
				if pairedFallbackNode == nil {
					nodeCopy := node
					pairedFallbackNode = &nodeCopy
					pairedFallbackRuntime = runtime
				}
				continue
			}
			if claim.PairedCoreNode != "" {
				pairedConsumeHandled = true
			}

			newRemaining := runtime.RemainingUOP - int(delta.Delta)
			if newRemaining < 0 {
				newRemaining = 0
			}
			_ = e.db.UpdateProcessNodeUOP(node.ID, newRemaining)

			// Auto-reorder if threshold reached, enabled, and node can accept orders
			if claim.AutoReorder && newRemaining <= claim.ReorderPoint && newRemaining > 0 {
				if ok, _ := e.CanAcceptOrders(node.ID); ok {
					_, err := e.RequestNodeMaterial(node.ID, 1)
					if err != nil {
						log.Printf("auto-reorder for node %s: %v", node.Name, err)
					}
				}
			}

		case "produce":
			newRemaining := runtime.RemainingUOP + int(delta.Delta)
			_ = e.db.UpdateProcessNodeUOP(node.ID, newRemaining)

			// Auto-relief at capacity: finalize the produce node (manifest + swap)
			if claim.AutoReorder && claim.UOPCapacity > 0 &&
				newRemaining >= claim.UOPCapacity {
				if ok, _ := e.CanAcceptOrders(node.ID); ok {
					_, err := e.FinalizeProduceNode(node.ID)
					if err != nil {
						log.Printf("auto-relief for produce node %s: %v", node.Name, err)
					}
				}
			}
		}
	}

	// A/B fallthrough: if no paired consume node was active but we found
	// an inactive paired node, decrement it as a safety net. This covers
	// the "count to lineside storage" case when neither A nor B is active.
	if !pairedConsumeHandled && pairedFallbackNode != nil && pairedFallbackRuntime != nil {
		log.Printf("A/B fallthrough: no active-pull node for process %d, decrementing fallback node %s",
			delta.ProcessID, pairedFallbackNode.Name)
		newRemaining := pairedFallbackRuntime.RemainingUOP - int(delta.Delta)
		if newRemaining < 0 {
			newRemaining = 0
		}
		_ = e.db.UpdateProcessNodeUOP(pairedFallbackNode.ID, newRemaining)
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

	// Order B completion (OldMaterialReleaseOrderID).
	// Phase 3 swap/evacuate: Order B runs the full swap/evacuate complex order
	// with wait steps — when it completes, both evacuation AND delivery are done,
	// so the node goes straight to "released".
	// Manual path / drop: Order B is a simple release/move — only evacuation,
	// so the node goes to "line_cleared" (operator still needs to release new material).
	if nodeTask != nil && nodeTask.OldMaterialReleaseOrderID != nil && *nodeTask.OldMaterialReleaseOrderID == order.ID &&
		(order.OrderType == orders.TypeMove || order.OrderType == orders.TypeComplex) {
		if (nodeTask.Situation == "swap" || nodeTask.Situation == "evacuate") && order.OrderType == orders.TypeComplex {
			// Phase 3: Order B is a complex order with wait steps — it ran the full
			// swap/evacuate (evacuation + delivery in one order). Node goes to "released".
			if toClaim, err := e.db.GetStyleNodeClaimByNode(e.getChangeoverToStyleID(node.ProcessID), node.CoreNodeName); err == nil {
				claimID := toClaim.ID
				_ = e.db.SetProcessNodeRuntime(node.ID, &claimID, toClaim.UOPCapacity)
				_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released")
				return
			}
		}
		// Manual path or drop: simple move order — only evacuation done, line cleared.
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
			return
		}
	}

	// Bin loader: move order completed — bin has been sent to destination, node is vacant
	if order.OrderType == orders.TypeMove {
		if claim := e.findActiveClaim(node); claim != nil && claim.Role == "bin_loader" {
			claimID := claim.ID
			_ = e.db.SetProcessNodeRuntime(node.ID, &claimID, 0)
			_ = e.db.UpdateProcessNodeRuntimeOrders(node.ID, nil, nil)
			e.tryAutoRequestEmpty(node, claim)
			return
		}
	}

	// Produce node ingest completion — Core now knows the bin's manifest.
	// Reset UOP to 0 and clear order tracking. No auto-request here:
	// simple mode still has the filled bin at the node (can't deliver an
	// empty until it's removed), and swap modes already have complex orders
	// in flight that handle the exchange.
	if order.OrderType == orders.TypeIngest {
		if claim := e.findActiveClaim(node); claim != nil && claim.Role == "produce" {
			claimID := claim.ID
			_ = e.db.SetProcessNodeRuntime(node.ID, &claimID, 0)
			_ = e.db.UpdateProcessNodeRuntimeOrders(node.ID, nil, nil)
			return
		}
	}

	// Normal replenishment completion — reset UOP from active claim
	if order.OrderType == orders.TypeRetrieve || order.OrderType == orders.TypeComplex {
		if claim := e.findActiveClaim(node); claim != nil {
			claimID := claim.ID
			// Produce nodes receive an empty bin → UOP starts at 0.
			// Consume nodes receive a full bin → UOP starts at capacity.
			resetUOP := claim.UOPCapacity
			if claim.Role == "produce" {
				resetUOP = 0
			}
			_ = e.db.SetProcessNodeRuntime(node.ID, &claimID, resetUOP)

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
