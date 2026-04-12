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

// isInactivePairedNode reports whether a node is part of an A/B pair and is
// not the active-pull side. Both consume and produce branches skip processing
// for the inactive half to avoid double-counting.
func isInactivePairedNode(claim *store.StyleNodeClaim, runtime *store.ProcessNodeRuntimeState) bool {
	return claim.PairedCoreNode != "" && !runtime.ActivePull
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
		claim := findActiveClaim(e.db, &node)
		if claim == nil {
			continue
		}
		// Only process nodes with a claim matching this style
		if claim.StyleID != delta.StyleID {
			continue
		}

		switch claim.Role {
		case "consume":
			// A/B cycling: only decrement the active-pull side.
			// The inactive side holds staged material.
			if isInactivePairedNode(claim, runtime) {
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
			if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
				log.Printf("update UOP for node %d: %v", node.ID, err)
			}

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
			// A/B cycling: only increment the active-pull side.
			// The inactive side holds its current production.
			if isInactivePairedNode(claim, runtime) {
				continue
			}

			newRemaining := runtime.RemainingUOP + int(delta.Delta)
			if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
				log.Printf("update UOP for node %d: %v", node.ID, err)
			}

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
		if err := e.db.UpdateProcessNodeUOP(pairedFallbackNode.ID, newRemaining); err != nil {
			log.Printf("update UOP for node %d: %v", pairedFallbackNode.ID, err)
		}
	}
}

// orderCompletionCtx holds shared lookups for order completion handling.
// Loaded once by loadOrderCompletionCtx and passed to each handler.
type orderCompletionCtx struct {
	order     *store.Order
	node      *store.ProcessNode
	runtime   *store.ProcessNodeRuntimeState
	toStyleID int64
	nodeTask  *store.ChangeoverNodeTask // nil when no active changeover
}

// loadOrderCompletionCtx fetches the order, node, runtime, and changeover context.
// Returns nil if any required lookup fails (order, node, runtime).
// nodeTask may be nil when no active changeover exists — callers must check.
func (e *Engine) loadOrderCompletionCtx(completed OrderCompletedEvent) *orderCompletionCtx {
	if completed.ProcessNodeID == nil {
		return nil
	}
	order, err := e.db.GetOrder(completed.OrderID)
	if err != nil {
		return nil
	}
	node, err := e.db.GetProcessNode(*completed.ProcessNodeID)
	if err != nil {
		return nil
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(node.ID)
	if err != nil {
		return nil
	}

	ctx := &orderCompletionCtx{order: order, node: node, runtime: runtime}

	if changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
		ctx.toStyleID = changeover.ToStyleID
		if t, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, node.ID); err == nil {
			ctx.nodeTask = t
		}
	}
	return ctx
}

func (e *Engine) handleNodeOrderCompleted(completed OrderCompletedEvent) {
	ctx := e.loadOrderCompletionCtx(completed)
	if ctx == nil {
		return
	}

	if e.handleStagedDelivery(ctx) {
		return
	}
	if e.handleOrderBCompletion(ctx) {
		return
	}
	if e.handleChangeoverRelease(ctx) {
		return
	}
	if e.handleManualSwapCompletion(ctx) {
		return
	}
	if e.handleProduceIngestCompletion(ctx) {
		return
	}
	e.handleNormalReplenishment(ctx)
}

// handleStagedDelivery handles Order A delivering to inbound staging during runout.
// Returns true if this order matched the staged delivery path.
func (e *Engine) handleStagedDelivery(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.NextMaterialOrderID == nil || *ctx.nodeTask.NextMaterialOrderID != ctx.order.ID {
		return false
	}
	toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, ctx.node.CoreNodeName)
	if err != nil || toClaim.InboundStaging == "" || ctx.order.DeliveryNode != toClaim.InboundStaging {
		return false
	}
	claimID := toClaim.ID
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, ctx.runtime.RemainingUOP); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "staged"); err != nil {
		log.Printf("update node task %d to staged: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// handleOrderBCompletion handles Order B (OldMaterialReleaseOrderID) completing.
// Phase 3 swap/evacuate: complex Order B does evacuation + delivery → "released".
// Keep-staged: complex Order B only evacuates → "line_cleared" or "released" if Order A also done.
// Manual/drop: simple move Order B → "line_cleared".
func (e *Engine) handleOrderBCompletion(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.OldMaterialReleaseOrderID == nil || *ctx.nodeTask.OldMaterialReleaseOrderID != ctx.order.ID {
		return false
	}
	if ctx.order.OrderType != orders.TypeMove && ctx.order.OrderType != orders.TypeComplex {
		return false
	}

	// Complex Order B in swap/evacuate situations
	if (ctx.nodeTask.Situation == "swap" || ctx.nodeTask.Situation == "evacuate") && ctx.order.OrderType == orders.TypeComplex {
		return e.handleComplexOrderBCompletion(ctx)
	}

	// Manual path or drop: simple move order — only evacuation done, line cleared.
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, ctx.runtime.ActiveClaimID, 0); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "line_cleared"); err != nil {
		log.Printf("update node task %d to line_cleared: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// handleComplexOrderBCompletion handles complex Order B in swap/evacuate changeovers.
// Regular: evacuation + delivery in one order → "released".
// Keep-staged: only evacuates → depends on whether Order A (delivery) also completed.
func (e *Engine) handleComplexOrderBCompletion(ctx *orderCompletionCtx) bool {
	isKeepStaged := false
	if ctx.nodeTask.FromClaimID != nil {
		if fromClaim, err := e.db.GetStyleNodeClaim(*ctx.nodeTask.FromClaimID); err == nil {
			isKeepStaged = fromClaim.KeepStaged
		}
	}

	if isKeepStaged {
		return e.handleKeepStagedOrderBCompletion(ctx)
	}

	// Regular swap/evacuate: Order B did evacuation + delivery in one order.
	toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, ctx.node.CoreNodeName)
	if err != nil {
		return true // matched the path but claim lookup failed
	}
	claimID := toClaim.ID
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, toClaim.UOPCapacity); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "released"); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// handleKeepStagedOrderBCompletion handles keep-staged changeovers where Order B
// only evacuated old material (no delivery steps).
// If Order A (delivery) also completed → "released". Otherwise → "line_cleared".
func (e *Engine) handleKeepStagedOrderBCompletion(ctx *orderCompletionCtx) bool {
	orderADone := true
	if ctx.nodeTask.NextMaterialOrderID != nil {
		if orderA, err := e.db.GetOrder(*ctx.nodeTask.NextMaterialOrderID); err == nil && !orders.IsTerminal(orderA.Status) {
			orderADone = false
		}
	}

	if orderADone {
		if toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, ctx.node.CoreNodeName); err == nil {
			claimID := toClaim.ID
			if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, toClaim.UOPCapacity); err != nil {
				log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
			}
			if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "released"); err != nil {
				log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
			}
			return true
		}
	}

	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, ctx.runtime.ActiveClaimID, 0); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "line_cleared"); err != nil {
		log.Printf("update node task %d to line_cleared: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// handleChangeoverRelease handles Order A completing to release staged/replenished
// material into production during a changeover (non-staging delivery path).
func (e *Engine) handleChangeoverRelease(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.NextMaterialOrderID == nil || *ctx.nodeTask.NextMaterialOrderID != ctx.order.ID {
		return false
	}
	toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, ctx.node.CoreNodeName)
	if err != nil {
		return false
	}
	claimID := toClaim.ID
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, toClaim.UOPCapacity); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "released"); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// handleManualSwapCompletion handles a move order completing for manual_swap nodes.
// The bin has been sent to destination, node is vacant — triggers tryAutoRequest.
func (e *Engine) handleManualSwapCompletion(ctx *orderCompletionCtx) bool {
	if ctx.order.OrderType != orders.TypeMove {
		return false
	}
	claim := findActiveClaim(e.db, ctx.node)
	if claim == nil || claim.SwapMode != "manual_swap" {
		return false
	}
	claimID := claim.ID
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, 0); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, nil, nil); err != nil {
		log.Printf("update runtime orders for node %d: %v", ctx.node.ID, err)
	}
	e.tryAutoRequest(ctx.node, claim)
	return true
}

// handleProduceIngestCompletion handles ingest order completing for produce nodes.
// Core now knows the bin's manifest. Reset UOP to 0 and clear order tracking.
// No auto-request here: simple mode still has the filled bin at the node,
// and swap modes already have complex orders in flight.
func (e *Engine) handleProduceIngestCompletion(ctx *orderCompletionCtx) bool {
	if ctx.order.OrderType != orders.TypeIngest {
		return false
	}
	claim := findActiveClaim(e.db, ctx.node)
	if claim == nil || claim.Role != "produce" {
		return false
	}
	claimID := claim.ID
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, 0); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, nil, nil); err != nil {
		log.Printf("update runtime orders for node %d: %v", ctx.node.ID, err)
	}
	return true
}

// handleNormalReplenishment handles standard retrieve/complex order completion.
// Resets UOP from the active claim (capacity for consume, 0 for produce).
func (e *Engine) handleNormalReplenishment(ctx *orderCompletionCtx) {
	if ctx.order.OrderType != orders.TypeRetrieve && ctx.order.OrderType != orders.TypeComplex {
		return
	}
	claim := findActiveClaim(e.db, ctx.node)
	if claim == nil {
		return
	}
	claimID := claim.ID
	// Produce nodes receive an empty bin → UOP starts at 0.
	// Consume nodes receive a full bin → UOP starts at capacity.
	resetUOP := claim.UOPCapacity
	if claim.Role == "produce" {
		resetUOP = 0
	}
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, resetUOP); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}

	// manual_swap nodes: clear order slots so CanAcceptOrders and the
	// multi-order queue don't see stale IDs. Standard consume/produce
	// nodes manage order slots via complex order progression.
	if claim.SwapMode == "manual_swap" {
		if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, nil, nil); err != nil {
			log.Printf("update runtime orders for node %d: %v", ctx.node.ID, err)
		}
	}

	// Keep-staged: immediately pre-populate inbound staging for next swap
	e.maybePreStage(ctx.node, claim)
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
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &order.ID); err != nil {
		log.Printf("update runtime orders for node %d: %v", nodeID, err)
	}
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
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error"); err != nil {
			log.Printf("update node task %d to error: %v", nodeTask.ID, err)
		}
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

	claim := findActiveClaim(e.db, node)
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
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, runtime.ActiveOrderID, &orderB.ID); err != nil {
		log.Printf("update runtime orders for node %d: %v", nodeID, err)
	}
	log.Printf("sequential backfill: created Order B %d for node %s (Order A %d in_transit)", orderB.ID, node.Name, order.ID)
}