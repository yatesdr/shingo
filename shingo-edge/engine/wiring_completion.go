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

	"shingo/protocol"
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

// binArrivingAt returns order.BinID iff the order completes a delivery
// to the given core node name. Removal-shaped completions (DeliveryNode
// is the supermarket, not the line) return nil so callers correctly
// mark the slot empty. Multi-bin orders (BinID nil at Core's
// order.delivered envelope today) also return nil — bucket deltas
// govern those flows separately.
func binArrivingAt(order *storeorders.Order, coreNodeName string) *int64 {
	if order == nil || order.BinID == nil {
		return nil
	}
	if order.DeliveryNode != coreNodeName {
		return nil
	}
	return order.BinID
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
	// handler below is a successful-path responder: UOP reset, runtime turn-
	// over, side-cycle L2 dispatch, changeover state advancement, keep-staged
	// pre-stage. Running them on a cancel or fail leaks state forward as if
	// the bin had arrived. Plant 2026-04-28: a cancelled L1 retrieve_empty
	// fired the L2 side-cycle move, evicting the empty already parked at the
	// loader (incident orders #483 → #484). Failure cleanup belongs on
	// EventOrderFailed (handleNodeOrderFailed) and the cancel/fail paths
	// themselves — not here.
	if ctx.order.Status != orders.StatusConfirmed {
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
	if err := e.db.SetProcessNodeRuntime(ctx.node.ID, &claimID, ctx.runtime.RemainingUOPCached); err != nil {
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
	// Bin physically left the slot; clear active_bin_id so subsequent
	// PLC ticks don't attribute to a bin that's no longer here.
	if err := e.db.SetProcessNodeRuntimeWithBin(ctx.node.ID, ctx.runtime.ActiveClaimID, nil, 0); err != nil {
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
	var fromClaim *processes.NodeClaim
	if ctx.nodeTask.FromClaimID != nil {
		if fc, err := e.db.GetStyleNodeClaim(*ctx.nodeTask.FromClaimID); err == nil {
			fromClaim = fc
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

	// UOP reset on delivery (moved here from release-click handler): release
	// only marks the node task "released"; the actual runtime UOP turnover
	// happens when the new bin physically arrives. Always run the reset
	// regardless of nodeTask.State.
	toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, ctx.node.CoreNodeName)
	if err != nil {
		return true // matched the path but claim lookup failed
	}

	// Sequential EVAC OrderB delivers to PairedCoreNode, not the
	// primary. Reset the paired physical node's runtime instead of
	// ctx.node.ID. For other modes (single_robot, two_robot) OrderB
	// delivers (or its choreography ends) at the primary, so the standard
	// path stays unchanged.
	if fromClaim != nil &&
		fromClaim.SwapMode == "sequential" &&
		ctx.nodeTask.Situation == "evacuate" &&
		fromClaim.PairedCoreNode != "" {
		e.resetSequentialEvacOrderBRuntime(ctx, fromClaim, toClaim)
	} else {
		claimID := toClaim.ID
		// binArrivingAt returns nil for removal-shaped Order B (DeliveryNode
		// is the supermarket, not the line) so the slot is correctly marked
		// empty; non-nil for sequential SWAP where the single complex order
		// terminates at the line with the new bin. resolveReplenishUOP
		// returns 0 when binID is nil so the cached UOP matches reality.
		binID := binArrivingAt(ctx.order, ctx.node.CoreNodeName)
		resetUOP := resolveReplenishUOP(toClaim.Role, toClaim.UOPCapacity, binID)
		if err := e.db.SetProcessNodeRuntimeWithBin(ctx.node.ID, &claimID, binID, resetUOP); err != nil {
			log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
		}
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "released"); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// resetSequentialEvacOrderBRuntime resets the runtime UOP for the paired
// physical node at sequential EVAC OrderB completion. Each robot resets
// its own position; OrderA's completion (handleChangeoverRelease)
// resets primary, this fires for OrderB's completion to reset paired.
// Computes binID and resetUOP locally against the paired node so an
// EVAC removal correctly leaves the slot empty (binID nil → resetUOP 0).
func (e *Engine) resetSequentialEvacOrderBRuntime(ctx *orderCompletionCtx, fromClaim, toClaim *processes.NodeClaim) {
	nodes, err := e.db.ListProcessNodesByProcess(ctx.node.ProcessID)
	if err != nil {
		log.Printf("sequential evac OrderB: list process nodes: %v", err)
		return
	}
	var pairedNode *processes.Node
	for i := range nodes {
		if nodes[i].CoreNodeName == fromClaim.PairedCoreNode {
			pairedNode = &nodes[i]
			break
		}
	}
	if pairedNode == nil {
		log.Printf("sequential evac OrderB: paired node %q not found", fromClaim.PairedCoreNode)
		return
	}
	pairedClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, fromClaim.PairedCoreNode)
	if err != nil || pairedClaim == nil {
		pairedClaim = toClaim
	}
	pairedClaimID := pairedClaim.ID
	// Order B might deliver to either the primary (rare) or the paired
	// (usual EVAC pattern); binArrivingAt against the paired node's name
	// captures the latter, returning nil otherwise. resolveReplenishUOP
	// then maps nil → 0 (slot empty) and non-nil → claim capacity.
	pairedBinID := binArrivingAt(ctx.order, fromClaim.PairedCoreNode)
	pairedResetUOP := resolveReplenishUOP(pairedClaim.Role, pairedClaim.UOPCapacity, pairedBinID)
	if err := e.db.SetProcessNodeRuntimeWithBin(pairedNode.ID, &pairedClaimID, pairedBinID, pairedResetUOP); err != nil {
		log.Printf("sequential evac OrderB: reset paired runtime for node %d: %v", pairedNode.ID, err)
	}
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
// runtime cache lies with the old style's UOP value until the
// reconciler heals (~60s).
func (e *Engine) handleChangeoverRelease(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.NextMaterialOrderID == nil || *ctx.nodeTask.NextMaterialOrderID != ctx.order.ID {
		return false
	}
	// UOP reset always runs on delivery — release only marks state="released".
	//
	// For press-index per-position node tasks, the task's CoreNodeName
	// is a back/middle position name that ISN'T the primary CoreNodeName
	// of any claim (only the front position is). GetStyleNodeClaimByNode
	// looks up by primary core_node_name and won't find the parent
	// press-index claim. Fall back to the task-stored ToClaimID, which
	// the fan-out post-processor sets to the parent claim's ID — that's
	// the persisted authoritative claim even when the synthesized
	// in-memory claim used different fields for routing.
	toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, ctx.node.CoreNodeName)
	if err != nil || toClaim == nil {
		if ctx.nodeTask.ToClaimID != nil {
			if c, lookupErr := e.db.GetStyleNodeClaim(*ctx.nodeTask.ToClaimID); lookupErr == nil && c != nil {
				toClaim = c
			} else {
				return false
			}
		} else {
			return false
		}
	}
	claimID := toClaim.ID
	// Order A's terminal step is the active-side dropoff at the line —
	// binArrivingAt picks up the new bin's ID when DeliveryNode matches.
	// resolveReplenishUOP maps nil → 0 (slot empty) and non-nil →
	// claim capacity.
	binID := binArrivingAt(ctx.order, ctx.node.CoreNodeName)
	resetUOP := resolveReplenishUOP(toClaim.Role, toClaim.UOPCapacity, binID)
	if err := e.db.SetProcessNodeRuntimeWithBin(ctx.node.ID, &claimID, binID, resetUOP); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
	}
	// Also reset the paired position's runtime when the from-claim has
	// a PairedCoreNode (sequential SWAP completion delivers to BOTH
	// positions; runtime UOP must reflect that on both rows immediately).
	e.resetPairedRuntimeUOPForSequentialSwap(ctx, toClaim)
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, "released"); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// resetPairedRuntimeUOPForSequentialSwap resets the runtime UOP for the
// paired physical node at sequential SWAP completion. The single complex
// order's terminal step is the active-side dropoff — by then both
// positions have new bins and both runtime rows need the new claim's
// UOP capacity. Without this reset, the paired side would keep reading
// the old style's UOP until the reconciler heals (~60s).
//
// Caller is responsible for ensuring this is invoked ONLY on sequential
// SWAP terminal completion. Sequential EVAC uses a different path —
// each robot's order resets its own position, so paired isn't reset here
// (it would create a TC-77 phantom-inventory window if the paired side
// hadn't been delivered yet).
//
// Looks up the paired physical node by CoreNodeName within the task's
// process. Logs and returns on any lookup failure rather than panicking.
func (e *Engine) resetPairedRuntimeUOPForSequentialSwap(ctx *orderCompletionCtx, toClaim *processes.NodeClaim) {
	if ctx.nodeTask == nil || ctx.nodeTask.FromClaimID == nil {
		return
	}
	fromClaim, err := e.db.GetStyleNodeClaim(*ctx.nodeTask.FromClaimID)
	if err != nil || fromClaim == nil || fromClaim.PairedCoreNode == "" {
		return
	}
	if fromClaim.SwapMode != "sequential" {
		return
	}
	if ctx.nodeTask.Situation != "swap" {
		// Sequential EVAC has each robot's order resetting its own
		// position — paired runtime is reset by OrderB's completion
		// path (resetSequentialEvacOrderBRuntime), not here.
		return
	}
	nodes, err := e.db.ListProcessNodesByProcess(ctx.node.ProcessID)
	if err != nil {
		return
	}
	var pairedNode *processes.Node
	for i := range nodes {
		if nodes[i].CoreNodeName == fromClaim.PairedCoreNode {
			pairedNode = &nodes[i]
			break
		}
	}
	if pairedNode == nil {
		return
	}
	// Sibling claim if the to-style models the pair as two claims;
	// fall back to the primary toClaim if it's modeled as one.
	pairedClaim, err := e.db.GetStyleNodeClaimByNode(ctx.toStyleID, fromClaim.PairedCoreNode)
	if err != nil || pairedClaim == nil {
		pairedClaim = toClaim
	}
	pairedClaimID := pairedClaim.ID
	// Sequential SWAP delivers to BOTH positions in one terminal step;
	// the order's DeliveryNode could be either the primary or the paired
	// depending on dispatch shape. binArrivingAt against the paired name
	// captures the latter case; nil otherwise leaves the paired slot
	// correctly marked empty until its own delivery completion fires.
	pairedBinID := binArrivingAt(ctx.order, fromClaim.PairedCoreNode)
	pairedResetUOP := resolveReplenishUOP(pairedClaim.Role, pairedClaim.UOPCapacity, pairedBinID)
	if err := e.db.SetProcessNodeRuntimeWithBin(pairedNode.ID, &pairedClaimID, pairedBinID, pairedResetUOP); err != nil {
		log.Printf("sequential swap completion: reset paired runtime for node %d: %v", pairedNode.ID, err)
	}
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
	if claim == nil || claim.SwapMode != "manual_swap" || claim.Role != protocol.ClaimRoleProduce {
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
	// L2 always auto-confirms: OutboundDestination is an unattended
	// supermarket node, so without auto-confirm the order sits at
	// `delivered` forever (no operator to tap CONFIRM there). This is
	// independent of claim.AutoConfirm — that flag controls operator-facing
	// orders at THIS loader, not the side-cycle move that Edge owns end-to-
	// end. Pre-fix the L2 stuck delivered on Edge while Core auto-confirmed
	// on its side; the divergence lit up the bin-loader UI as a permanent
	// "Confirm" button on a move that had already physically completed.
	order, err := e.orderMgr.CreateMoveOrder(&nodeID, 1, claim.CoreNodeName, claim.OutboundDestination, true)
	if err != nil {
		e.logFn("side-cycle: create L2 (filled-out) for loader %s: %v", ctx.node.Name, err)
		return false
	}
	log.Printf("side-cycle: L2 (filled-out) order %d for loader %s → %s", order.ID, ctx.node.Name, claim.OutboundDestination)
	// Reset runtime so the loader UI clears the L1 order and can show L2 next.
	// L1's empty bin physically arrived at the loader — capture its BinID so
	// PLC ticks during the L2 outbound move can attribute correctly.
	claimID := claim.ID
	binID := binArrivingAt(ctx.order, ctx.node.CoreNodeName)
	if err := e.db.SetProcessNodeRuntimeWithBin(ctx.node.ID, &claimID, binID, 0); err != nil {
		log.Printf("side-cycle: set runtime for loader %d: %v", ctx.node.ID, err)
	}
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
	if claim == nil || claim.SwapMode != "manual_swap" || claim.Role != protocol.ClaimRoleConsume {
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
	order, err := e.orderMgr.CreateMoveOrder(&nodeID, 1, claim.CoreNodeName, claim.OutboundDestination, true)
	if err != nil {
		e.logFn("side-cycle: create U2 (empty-out) for unloader %s: %v", ctx.node.Name, err)
		return false
	}
	log.Printf("side-cycle: U2 (empty-out) order %d for unloader %s → %s", order.ID, ctx.node.Name, claim.OutboundDestination)
	// U1's full bin arrived at the unloader — capture BinID for PLC tick
	// attribution during the U2 outbound move.
	claimID := claim.ID
	binID := binArrivingAt(ctx.order, ctx.node.CoreNodeName)
	if err := e.db.SetProcessNodeRuntimeWithBin(ctx.node.ID, &claimID, binID, 0); err != nil {
		log.Printf("side-cycle: set runtime for unloader %d: %v", ctx.node.ID, err)
	}
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
	if claim == nil || claim.SwapMode != "manual_swap" {
		return false
	}
	claimID := claim.ID
	// L2/U2 move completed: bin physically left the slot. Clear the bin
	// pointer so subsequent PLC ticks don't attribute to the departed bin.
	if err := e.db.SetProcessNodeRuntimeWithBin(ctx.node.ID, &claimID, nil, 0); err != nil {
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
	if claim == nil || claim.Role != protocol.ClaimRoleProduce {
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
// The reset binds to the delivery event: a fresh bin has physically arrived,
// so the line's UOP tracking should turn over now.
//
// Removal-shaped orders (Order B in two-robot consume, R1 in press-
// index, sequential-removal step) flow through the reset like any
// other completion — runtime briefly resets to claim.UOPCapacity. The
// reconciler observes the now-empty slot via empty-slot detection and
// heals the runtime to 0 within the next pass (~60s). Brief "looks
// like full bin" UI during that window is SME-accepted.
//
// See TestRegression_RemovalOrderHealsToZeroViaReconciler in
// uop_regression_test.go for the mechanism pin.
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
	// Consume nodes receive a bin from the supermarket — could be full
	// or partial (operator-released runouts in particular send the
	// remaining UOP back as a partial). Reset to claim.UOPCapacity
	// here; the reconciler heals the runtime cache to Core's
	// authoritative bin value within the next pass (~60s). Brief "looks
	// like full bin" UI on partial-back returns is SME-accepted.
	// Set active_bin_id from the order's BinID iff the order delivered to
	// this node. Removal-shaped orders (Order B in two-robot consume,
	// sequential-removal step) have DeliveryNode=supermarket so binArrivingAt
	// returns nil, correctly leaving the slot empty post-removal.
	// resolveReplenishUOP maps nil → 0 (no bin → no UOP cached) and
	// non-nil → role-appropriate reset (capacity for consume, 0 for produce).
	binID := binArrivingAt(ctx.order, ctx.node.CoreNodeName)
	resetUOP := resolveReplenishUOP(claim.Role, claim.UOPCapacity, binID)
	if err := e.db.SetProcessNodeRuntimeWithBin(ctx.node.ID, &claimID, binID, resetUOP); err != nil {
		log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
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

