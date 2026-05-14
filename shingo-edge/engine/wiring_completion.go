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
//                                  loader/unloader side-cycle → manual swap →
//                                  produce ingest → normal replenishment
//   handleStagedDelivery         – Order A → inbound staging
//   handleOrderBCompletion       – Order B (old material release)
//   handleComplexOrderBCompletion / handleKeepStagedOrderBCompletion
//   handleChangeoverRelease      – Order A direct delivery
//   handleLoaderEmptyInCompletion   – L1 confirm → fire L2 (filled-out)
//   handleUnloaderFullInCompletion  – U1 confirm → fire U2 (empty-out)
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
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
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

	// EmitOrderCompleted fires for any terminal status (confirmed, cancelled,
	// failed) — it's the engine bus's "order reached terminal" signal. Every
	// success-path handler below assumes a delivered bin: UOP reset, runtime
	// turnover, side-cycle L2 dispatch, changeover state advancement,
	// keep-staged pre-stage. Running them on a cancel or fail leaks state
	// forward as if the bin had arrived. Plant 2026-04-28: a cancelled L1
	// retrieve_empty fired the L2 side-cycle move, evicting the empty already
	// parked at the loader (incident orders #483 → #484).
	//
	// Non-success terminal (cancelled or failed) on an order linked to a
	// changeover_node_task takes a separate path so the linked task is
	// stamped to a final disposition instead of being left at
	// staging_requested. Failure cleanup also flows through the existing
	// EventOrderFailed → handleNodeOrderFailed; the orphan handler
	// composes with that for the StatusCancelled gap, where
	// EventOrderFailed does not fire.
	if ctx.order.Status != orders.StatusConfirmed {
		if ctx.order.Status == orders.StatusCancelled || ctx.order.Status == orders.StatusFailed {
			e.handleOrphanedTaskOrderCompleted(ctx.order)
		}
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
	if e.handleUnloaderFullInCompletion(ctx) {
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
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.SetClaimAndCount(ctx.node.ID, &claimID, ctx.runtime.RemainingUOPCached); err != nil {
			log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
		}
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "staged"); err != nil {
		log.Printf("update node task %d to staged: %v", ctx.nodeTask.ID, err)
	}
	if err := e.tryCompleteProcessChangeover(ctx.node.ProcessID); err != nil {
		log.Printf("changeover: try-complete after staged delivery for process %d: %v", ctx.node.ProcessID, err)
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
	// Bin physically left the slot; clear active_bin_id so subsequent
	// PLC ticks don't attribute to a bin that's no longer here.
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.ClearActiveAndReset(ctx.node.ID, ctx.runtime.ActiveClaimID); err != nil {
			log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
		}
	}
	// Drop tasks without the evacuate marker were stamped terminal
	// (line_cleared) at plan time — the cutover never depended on this
	// completion event. Skip the state mutation so we don't churn the
	// updated_at timestamp; the task is already at line_cleared.
	if domain.IsNodeTaskStateTerminal(ctx.nodeTask.State, ctx.nodeTask.Situation) {
		return true
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
// UOP reset happens here on delivery completion (not at release click): the
// release handler only marks the node task "released". Resetting at delivery
// binds the runtime turnover to the moment the new bin is physically present,
// so a robot fault between release and arrival doesn't leave the UI showing
// a "fresh" capacity that the line never received.
//
// For sequential EVAC, OrderB delivers to PairedCoreNode (not to
// ctx.node.CoreNodeName which is the primary). Route the runtime reset
// to the paired physical node so each robot's completion resets its
// own position. handleChangeoverRelease handles the same for OrderA
// (which delivers to the primary).
func (e *Engine) handleComplexOrderBCompletion(ctx *orderCompletionCtx) bool {
	isKeepStaged := false
	if ctx.nodeTask.FromClaimID != nil {
		if fc, err := e.db.GetStyleNodeClaim(*ctx.nodeTask.FromClaimID); err == nil {
			isKeepStaged = fc.KeepStaged
		}
	}

	// KeepStaged is currently short-circuited (the handler returns false
	// as a no-op). Fall through to the standard path so legacy claims
	// with KeepStaged=true behave like normal swaps until the keep-
	// staged path is rewired. See implementer notes' "Known issue —
	// TC-77 latent under CO-0b fall-through" for the rewire-time risk.
	if isKeepStaged && e.handleKeepStagedOrderBCompletion(ctx) {
		return true
	}

	// Runtime UOP cache binding moved out of the completion path:
	// active_bin_id / cached_bin_id / remaining_uop_cached now flip at
	// release-click (incoming supply bin) and at delivery (the
	// physically-arrived bin), not at confirm. Confirm only advances
	// the changeover node task state machine.
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "released"); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	if err := e.tryCompleteProcessChangeover(ctx.node.ProcessID); err != nil {
		log.Printf("changeover: try-complete after complex Order B for process %d: %v", ctx.node.ProcessID, err)
	}
	return true
}

// handleKeepStagedOrderBCompletion is a short-circuited no-op.
//
// KeepStaged is shelved pending a future rewire. The function signature
// is preserved for call sites; returning false makes the dispatcher in
// handleComplexOrderBCompletion fall through to the standard non-
// KeepStaged path (UOP reset on delivery + state → "released"), which
// is the desired behaviour until KeepStaged is rewired. See implementer
// notes' "Known issue — TC-77 latent under CO-0b fall-through" for the
// rewire-time risk this falls through to.
func (e *Engine) handleKeepStagedOrderBCompletion(ctx *orderCompletionCtx) bool {
	_ = ctx
	return false
}

// handleChangeoverRelease handles Order A completing to release staged/replenished
// material into production during a changeover (non-staging delivery path).
//
// UOP reset runs on delivery completion (not at release click). Release only
// flips the node task to "released"; the runtime turnover is bound to the
// arrival event so a fault between release and delivery doesn't leave the
// line UI showing capacity for a bin that hasn't landed.
//
// Sequential SWAP ships as a single complex order with a mid-sequence
// cutover wait. Its terminal step is the ACTIVE-side dropoff: by then,
// both physical positions (CoreNodeName and PairedCoreNode) hold new
// bins, so reset BOTH runtime rows' UOP — otherwise the paired-side
// runtime cache would lie with the old style's UOP value indefinitely.
// Post-flip (6d226d1) Edge is authoritative for at-node bins; no
// reconciler exists to heal a stale cache.
func (e *Engine) handleChangeoverRelease(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.NextMaterialOrderID == nil || *ctx.nodeTask.NextMaterialOrderID != ctx.order.ID {
		return false
	}
	// Runtime UOP cache binding has moved off the completion path —
	// release-click writes the incoming supply bin's UOP, delivery
	// re-affirms with the actually-arrived bin. The state machine
	// transition is the only thing that fires on confirm.
	//
	// Sequential SWAP's paired-position runtime was previously also
	// reset here via resetPairedRuntimeUOPForSequentialSwap. That helper
	// is gone now that each delivered bin gets its own per-slot reset
	// from handleNodeOrderDelivered (each leg of a sequential SWAP
	// terminal step delivers to its own slot; the delivered handler
	// fires for each).
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "released"); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	if err := e.tryCompleteProcessChangeover(ctx.node.ProcessID); err != nil {
		log.Printf("changeover: try-complete after release for process %d: %v", ctx.node.ProcessID, err)
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
	if claim == nil || claim.SwapMode != protocol.SwapModeManualSwap || claim.Role != protocol.ClaimRoleProduce {
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
	// Resolve the loaded payload code so the L2 carries the operator's pick
	// rather than the claim's primary payload. A manual_swap loader's claim
	// can list several allowed_payload_codes; LoadBin set Core's bin to
	// whichever one the operator selected before confirming the L1, so
	// Core's bin state is the authoritative source at this point. Falling
	// back to claim.PayloadCode (via lookupPayloadMeta's empty-code path)
	// is acceptable for single-payload claims but mis-tags the L2 on
	// multi-payload loaders, which then fails to drive the per-tile
	// IN_TRANSIT render in operator-station (tiles filter active orders by
	// o.payload_code === code).
	loadedPayloadCode := ""
	if e.coreClient != nil && e.coreClient.Available() {
		if bins, _ := e.coreClient.FetchNodeBins([]string{ctx.node.CoreNodeName}); len(bins) > 0 {
			loadedPayloadCode = bins[0].PayloadCode
		}
	}
	// L2 always auto-confirms: OutboundDestination is an unattended
	// supermarket node, so without auto-confirm the order sits at
	// `delivered` forever (no operator to tap CONFIRM there). This is
	// independent of claim.AutoConfirm — that flag controls operator-facing
	// orders at THIS loader, not the side-cycle move that Edge owns end-to-
	// end. Pre-fix the L2 stuck delivered on Edge while Core auto-confirmed
	// on its side; the divergence lit up the bin-loader UI as a permanent
	// "Confirm" button on a move that had already physically completed.
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1, claim.CoreNodeName, claim.OutboundDestination, loadedPayloadCode, true)
	if err != nil {
		e.logFn("side-cycle: create L2 (filled-out) for loader %s: %v", ctx.node.Name, err)
		return false
	}
	log.Printf("side-cycle: L2 (filled-out) order %d for loader %s → %s payload=%q", order.ID, ctx.node.Name, claim.OutboundDestination, loadedPayloadCode)
	// Runtime cache binding is owned by the delivered handler — L1's
	// empty bin landing at the loader already wrote active_bin_id /
	// cached_bin_id / remaining_uop_cached. Confirm only swaps the
	// active order pointer so the loader UI shows L2 next.
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, &order.ID, nil); err != nil {
		log.Printf("side-cycle: update runtime orders for loader %d: %v", ctx.node.ID, err)
	}
	return true
}

// handleUnloaderFullInCompletion fires the side-cycle U2 when the U1
// full-in retrieve order is confirmed at a manual_swap consumer
// (unloader) node. U1 brought a full bin to the unloader; the operator
// processed its contents; CONFIRM means the (now-empty) bin is ready to
// send back to the supermarket. U2 = a move order from the unloader to
// claim.OutboundDestination.
//
// Symmetric to handleLoaderEmptyInCompletion (L2). The discriminator
// between L1 and U1 retrieve orders is the role on the active claim:
// producer (loader, L1, RetrieveEmpty=true) vs consumer (unloader, U1,
// full-bin retrieve with PayloadCode).
func (e *Engine) handleUnloaderFullInCompletion(ctx *orderCompletionCtx) bool {
	if ctx.order.OrderType != orders.TypeRetrieve || ctx.order.RetrieveEmpty {
		return false
	}
	claim := findActiveClaim(e.db, ctx.node)
	if claim == nil || claim.SwapMode != protocol.SwapModeManualSwap || claim.Role != protocol.ClaimRoleConsume {
		return false
	}
	if claim.OutboundDestination == "" {
		e.logFn("side-cycle: unloader %s has no OutboundDestination — cannot create U2 (empty bin will sit until operator manually moves it)", ctx.node.Name)
		return false
	}
	if claim.OutboundDestination == claim.CoreNodeName {
		e.logFn("side-cycle: unloader %s OutboundDestination same as CoreNode — skipping U2 (would be a same-node move)", ctx.node.Name)
		return false
	}
	nodeID := ctx.node.ID
	// U2 always auto-confirms: OutboundDestination is an unattended supermarket
	// node, no operator there to tap CONFIRM. Same rationale as L2.
	// U1 (the order being completed) carries the specific payload code that
	// arrived in the now-empty bin — thread it onto U2 so the operator
	// station can match the empty-out move to the right tile (otherwise
	// claim.PayloadCode wins and multi-payload unloaders mis-render).
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1, claim.CoreNodeName, claim.OutboundDestination, ctx.order.PayloadCode, true)
	if err != nil {
		e.logFn("side-cycle: create U2 (empty-out) for unloader %s: %v", ctx.node.Name, err)
		return false
	}
	log.Printf("side-cycle: U2 (empty-out) order %d for unloader %s → %s payload=%q", order.ID, ctx.node.Name, claim.OutboundDestination, ctx.order.PayloadCode)
	// Runtime cache binding is owned by the delivered handler — U1's
	// full bin landing at the unloader already wrote active_bin_id /
	// cached_bin_id / remaining_uop_cached. Confirm only swaps the
	// active order pointer so the unloader UI shows U2 next.
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, &order.ID, nil); err != nil {
		log.Printf("side-cycle: update runtime orders for unloader %d: %v", ctx.node.ID, err)
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
	if claim == nil || claim.SwapMode != protocol.SwapModeManualSwap {
		return false
	}
	// L2/U2 move arrived at the supermarket — the bin physically left
	// the loader/unloader some time ago (HandleBinPickedUp already
	// nulled active_bin_id at pickup). Confirm here only clears the
	// runtime order pointers so CanAcceptOrders and the multi-order
	// queue don't see stale IDs. Cache state stays as last set by the
	// delivered handler / release click; the next L1/U1 cycle's
	// delivery rebinds it.
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, nil, nil); err != nil {
		log.Printf("update runtime orders for node %d: %v", ctx.node.ID, err)
	}
	// tryAutoRequest call removed in side-cycle refactor (commit 4f9212b
	// + this one). Loader empties are now driven by line REQUESTs through
	// MaybeCreateLoaderEmptyIn, not by post-completion kanban auto-requests.
	//
	// Push-driven unloader: U2 just landed (empty returned to supermarket),
	// the unloader window is confirmed free. Fire the next U1 if the claim
	// is auto-push. MaybePushUnloader gates internally on AutoPush so this
	// is a no-op for kanban-driven unloaders.
	if claim.Role == protocol.ClaimRoleConsume && claim.AutoPush {
		e.MaybePushUnloader(ctx.node.ID)
	}
	return true
}

// handleProduceIngestCompletion handles ingest order completing for produce nodes.
// Core now knows the bin's manifest. Clears the runtime order pointers so the
// next produce cycle starts clean. Cache state is owned by:
//   - resetProduceRuntime at FinalizeProduceNode (operator's "I'm done" click)
//   - handleNodeOrderDelivered when the next empty bin lands at this node
// Confirm itself is a no-op for cache.
func (e *Engine) handleProduceIngestCompletion(ctx *orderCompletionCtx) bool {
	if ctx.order.OrderType != orders.TypeIngest {
		return false
	}
	claim := findActiveClaim(e.db, ctx.node)
	if claim == nil || claim.Role != protocol.ClaimRoleProduce {
		return false
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, nil, nil); err != nil {
		log.Printf("update runtime orders for node %d: %v", ctx.node.ID, err)
	}
	return true
}

// handleNormalReplenishment handles standard retrieve/complex order completion.
// Cache binding is owned by:
//   - operator release-click (incoming supply bin's UOP via SetProcessNodeCachedBin)
//   - handleNodeOrderDelivered (the actually-arrived bin's UOP)
// Confirm only does order-pointer bookkeeping for manual_swap nodes and
// fires the keep-staged hook (currently a no-op).
func (e *Engine) handleNormalReplenishment(ctx *orderCompletionCtx) {
	if ctx.order.OrderType != orders.TypeRetrieve && ctx.order.OrderType != orders.TypeComplex {
		return
	}
	claim := findActiveClaim(e.db, ctx.node)
	if claim == nil {
		return
	}

	// manual_swap nodes: clear order slots so CanAcceptOrders and the
	// multi-order queue don't see stale IDs. Standard consume/produce
	// nodes manage order slots via complex order progression.
	if claim.SwapMode == protocol.SwapModeManualSwap {
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

// maybePreStage is a short-circuited no-op.
//
// KeepStaged is shelved pending a future rewire. The function signature
// is preserved for call sites; schema column, planner branches, and
// step builders stay intact so the rewire is a one-line restore. The
// previous body fired an automatic pre-stage order after every release;
// that path was the broken behaviour SME asked to stop.
func (e *Engine) maybePreStage(node *processes.Node, claim *processes.NodeClaim) {
	_ = node
	_ = claim
}

// handleOrphanedTaskOrderCompleted reconciles a changeover_node_task
// whose linked order reached cancelled/failed outside
// cancelProcessChangeoverInternal — for example the operator's per-order
// cancel button at apiCancelOrder, or a Core-side cancellation. Without
// this handler the task is left at its prior non-terminal state
// (staging_requested, release_requested, …) because the success-path
// handler bails on Status != Confirmed and EventOrderFailed only fires
// for StatusFailed.
//
// Direct order-ID lookup (FindChangeoverNodeTaskByOrderID) bypasses
// GetActiveProcessChangeover so historical orphans whose changeover
// already finalized are still reconcilable. The completed/cancelled
// changeover-state guard (B.6 in the v2 plan) lives here rather than on
// handleNodeOrderFailed, where GetActiveProcessChangeover would already
// have filtered finalized rows.
func (e *Engine) handleOrphanedTaskOrderCompleted(order *storeorders.Order) {
	task, changeoverState, err := e.db.FindChangeoverNodeTaskByOrderID(order.ID)
	if err != nil {
		return
	}
	if changeoverState == "completed" || changeoverState == "cancelled" {
		log.Printf("orphan: skip task %d stamp — changeover %d already %s", task.ID, task.ProcessChangeoverID, changeoverState)
		return
	}
	newState := "cancelled"
	if order.Status == orders.StatusFailed {
		newState = "error"
	}
	if err := e.db.UpdateChangeoverNodeTaskState(task.ID, newState); err != nil {
		log.Printf("orphan: update task %d to %s: %v", task.ID, newState, err)
		return
	}
	log.Printf("orphan: task %d stamped %s for order %d (%s)", task.ID, newState, order.ID, order.Status)
	node, err := e.db.GetProcessNode(task.ProcessNodeID)
	if err != nil {
		return
	}
	if err := e.tryCompleteProcessChangeover(node.ProcessID); err != nil {
		log.Printf("orphan: try-complete after stamp for process %d: %v", node.ProcessID, err)
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
		newState := "error"
		// Drop auto-skip: for a drop, "could not claim a bin at the
		// pickup" satisfies the changeover's intent (or there was
		// nothing to remove). Core's no_source_bin code already routes
		// to lifecycle.Skip via HandleOrderSkipped; this branch covers
		// the parallel no_bin case (bins present but unclaimable —
		// e.g. payload mismatch, orphan claim from a previous
		// changeover) which Core still routes to Fail. Pre-fix this
		// stranded the operator at an error tile that required hunting
		// down the /changeover supervisor page to clear. Swap-evac
		// failures stay at "error" because the new bin can't move in
		// until the old one leaves.
		if nodeTask.Situation == "drop" && isNoBinFailure(failed.Reason) {
			newState = "line_cleared"
		}
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, newState); err != nil {
			log.Printf("update node task %d to %s: %v", nodeTask.ID, newState, err)
		}
		if newState == "line_cleared" {
			note := "evac auto-cleared: no bin to remove at " + nodeTask.NodeName
			if err := e.db.SetChangeoverNodeTaskSkipNote(nodeTask.ID, note); err != nil {
				log.Printf("changeover: set skip_note on drop auto-clear for task %d: %v", nodeTask.ID, err)
			}
			log.Printf("changeover: drop auto-cleared for node %s — order %d failed with %q", node.Name, order.ID, failed.Reason)
		} else {
			log.Printf("changeover: order failed for node %s, marked as error — manual retry needed", node.Name)
		}
		if err := e.tryCompleteProcessChangeover(node.ProcessID); err != nil {
			log.Printf("changeover: try-complete after order-failure for process %d: %v", node.ProcessID, err)
		}
	}
}

// isNoBinFailure recognizes Core's bin-claim failure shape on
// OrderFailedEvent.Reason. Core's planning error codes ("no_bin",
// "no_source_bin") are not preserved end-to-end through the wire — only
// the detail string is — so we match on the detail. Substrings cover
// both shapes:
//
//   - no_bin     "no available bin at pickup node(s) for order N"
//   - no_source_bin (defensive — Core routes this to Skip today, but
//     a future regression could route it here)
//     "no bin at pickup node(s) for order N — source was emptied externally"
//
// Strings are stable in complex_claims.go:139/141. If those move, add
// the new wording here or plumb the code through OrderFailedEvent.
func isNoBinFailure(reason string) bool {
	return strings.Contains(reason, "no available bin at pickup") ||
		strings.Contains(reason, "no bin at pickup")
}

