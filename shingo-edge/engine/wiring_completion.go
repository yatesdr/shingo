// wiring_completion.go — Order-completion chain and node-failure handling.
//
// Subscribed via wireEventHandlers (wiring.go) on EventOrderCompleted and
// EventOrderFailed. The completion dispatcher (handleNodeOrderCompleted)
// matches order type and changeover context using an early-return
// pattern: each handler returns true if it matched, false to fall
// through to the next.
//
// Layout:
//   loadOrderCompletionCtx       – shared lookup for order/node/runtime/changeover
//   handleNodeOrderCompleted     – dispatcher: staged → Order B → changeover →
//                                  manual swap → produce ingest → normal replenishment
//   handleStagedDelivery         – Order A → inbound staging
//   handleOrderBCompletion       – Order B (old material release)
//   handleComplexOrderBCompletion / handleKeepStagedOrderBCompletion
//   handleChangeoverRelease      – Order A direct delivery
//   handleManualSwapCompletion   – move order for manual_swap nodes
//   handleProduceIngestCompletion – ingest order for produce nodes
//   handleNormalReplenishment    – standard retrieve/complex
//   maybePreStage                – keep-staged pre-stage hook
//   handleNodeOrderFailed        – changeover error marking (failure
//                                  counterpart to changeover-order setup;
//                                  reads the same node-task context the
//                                  completion handlers do, which is why
//                                  it lives in this file rather than in
//                                  a standalone wiring_failed.go).

package engine

import (
	"log"

	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// orderCompletionCtx holds shared lookups for order completion handling.
// Loaded once by loadOrderCompletionCtx and passed to each handler.
type orderCompletionCtx struct {
	order     *storeorders.Order
	node      *processes.Node
	runtime   *processes.RuntimeState
	toStyleID int64
	nodeTask  *processes.NodeTask // nil when no active changeover
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
	if e.handleLoaderEmptyInCompletion(ctx) {
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
//
// Phase 3 (lineside): ReleaseOrderWithLineside normally resets UOP + advances the
// task to "released" at the operator's release click. When that path ran, this
// handler no-ops on the mutation — it's only a safety net for paths that bypass
// the release handler (e.g. changeover restore, auto-confirm edge cases).
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

	// Release handler already ran — nothing to do. Return true so we
	// don't fall through to handleNormalReplenishment.
	if ctx.nodeTask.State == "released" {
		return true
	}

	// Safety-net path (release handler didn't run): Order B did
	// evacuation + delivery in one order.
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
//
// Phase 3 (lineside): the "released" transition is now primarily driven by the
// operator release handler. This handler still fires if the release never ran
// (safety net) or if only Order A has completed (→ "line_cleared").
func (e *Engine) handleKeepStagedOrderBCompletion(ctx *orderCompletionCtx) bool {
	// Release handler already ran — nothing to do.
	if ctx.nodeTask.State == "released" {
		return true
	}

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
//
// Phase 3 (lineside): the operator release click now normally runs the UOP
// reset + "released" state transition before this event fires. When that
// happened, this handler no-ops (state is already "released"). It remains as
// a safety net for restore / auto-release paths that bypass the release
// handler.
func (e *Engine) handleChangeoverRelease(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.NextMaterialOrderID == nil || *ctx.nodeTask.NextMaterialOrderID != ctx.order.ID {
		return false
	}
	// Release handler already ran — nothing to do.
	if ctx.nodeTask.State == "released" {
		return true
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

// handleLoaderEmptyInCompletion fires the side-cycle L2 when the L1
// empty-in retrieve_empty order is confirmed at a manual_swap producer
// (loader) node. L1 brought an empty to the loader; the operator filled
// it; CONFIRM means the bin is ready to send back to the supermarket.
// L2 = a move order from the loader to claim.OutboundDestination.
//
// See SHINGO_TODO.md "Bin loader as active workflow participant".
// Returns true if it handled the order (regardless of L2 success).
func (e *Engine) handleLoaderEmptyInCompletion(ctx *orderCompletionCtx) bool {
	if !ctx.order.RetrieveEmpty {
		return false
	}
	claim := findActiveClaim(e.db, ctx.node)
	if claim == nil || claim.SwapMode != "manual_swap" || claim.Role != "produce" {
		return false
	}
	if claim.OutboundDestination == "" {
		e.logFn("side-cycle: loader %s has no OutboundDestination — cannot create L2 (filled bin will sit until operator manually moves it)", ctx.node.Name)
		return false
	}
	if claim.OutboundDestination == claim.CoreNodeName {
		e.logFn("side-cycle: loader %s OutboundDestination same as CoreNode — skipping L2 (would be a same-node move)", ctx.node.Name)
		return false
	}
	nodeID := ctx.node.ID
	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm
	order, err := e.orderMgr.CreateMoveOrder(&nodeID, 1, claim.CoreNodeName, claim.OutboundDestination, autoConfirm)
	if err != nil {
		e.logFn("side-cycle: create L2 (filled-out) for loader %s: %v", ctx.node.Name, err)
		return false
	}
	log.Printf("side-cycle: L2 (filled-out) order %d for loader %s → %s", order.ID, ctx.node.Name, claim.OutboundDestination)
	// Reset runtime so the loader UI clears the L1 order and can show L2 next.
	claimID := claim.ID
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, 0); err != nil {
		log.Printf("side-cycle: set runtime for loader %d: %v", ctx.node.ID, err)
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, &order.ID, nil); err != nil {
		log.Printf("side-cycle: update runtime orders for loader %d: %v", ctx.node.ID, err)
	}
	return true
}

// handleManualSwapCompletion handles a move order completing for manual_swap nodes.
// The bin has been sent to destination, node is vacant. Pre-side-cycle
// this also queued a follow-up empty-in via tryAutoRequest; that path was
// removed when MaybeCreateLoaderEmptyIn became the only empty-in source.
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
	// tryAutoRequest call removed in side-cycle refactor (commit 4f9212b
	// + this one). Loader empties are now driven by line REQUESTs through
	// MaybeCreateLoaderEmptyIn, not by post-completion kanban auto-requests.
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
//
// Phase 3 (lineside): for complex orders on consume nodes, the release click
// handler (ReleaseOrderWithLineside) already reset UOP to capacity before
// dispatch. Re-resetting on completion would wipe any counter ticks that
// happened during the bots-home leg — that's the drift we're fixing. So the
// reset is gated: consume + complex with runtime already pointing at this
// claim and RemainingUOP > 0 is treated as "release already ran, don't
// clobber." Produce nodes still reset (empty bin arriving), and simple
// retrieve orders still reset (no release-click prompt exists for them).
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

	// Consume + complex: the release-click handler
	// (ReleaseOrderWithLineside) already reset UOP to capacity before
	// dispatch. Resetting again here would wipe any counter ticks the
	// operator ran off during the bots-home leg — exactly the drift
	// this phase is fixing. Skip the reset for that case; other paths
	// (retrieve, produce) keep the existing completion-time reset.
	skipReset := claim.Role == "consume" && ctx.order.OrderType == orders.TypeComplex
	if !skipReset {
		if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, resetUOP); err != nil {
			log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
		}
	}

	// manual_swap nodes: clear order slots so CanAcceptOrders and the
	// multi-order queue don't see stale IDs. Standard consume/produce
	// nodes manage order slots via complex order progression.
	if claim.SwapMode == "manual_swap" {
		if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, nil, nil); err != nil {
			log.Printf("update runtime orders for node %d: %v", ctx.node.ID, err)
		}
		// Pre-side-cycle, this called e.tryAutoRequest to re-evaluate
		// kanban demand and queue a new empty-in. Removed: the
		// side-cycle drives empty-in creation from line REQUESTs via
		// MaybeCreateLoaderEmptyIn, not from completion-time sweeps.
	}

	// Keep-staged: immediately pre-populate inbound staging for next swap
	e.maybePreStage(ctx.node, claim)
}

// maybePreStage orders the next bin to inbound staging if the claim has
// keep_staged enabled. This ensures the staging node always has material
// ready for a fast swap.
func (e *Engine) maybePreStage(node *processes.Node, claim *processes.NodeClaim) {
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

// ── Order failure ───────────────────────────────────────────────────

// handleNodeOrderFailed marks a changeover node task as "error" when one
// of its tracked orders fails, leaving the order in runtime tracking so
// auto-reorder doesn't loop. Lives in this file because the failure
// branch reads the same active-changeover / node-task context as the
// completion handlers above; folding the two negative- and positive-path
// counterparts together keeps changeover orchestration in one place.
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
