// wiring_completion.go — Order-completion chain and node-failure handling.
//
// Subscribed via wireEventHandlers (wiring.go) on EventOrderCompleted and
// EventOrderFailed. handleNodeOrderCompleted walks completionChain (see
// completion_table.go) and dispatches the first matching row's Apply.
//
// Layout:
//   loadOrderCompletionCtx                       – shared lookup for order/node/runtime/changeover
//   orderCompletionCtx + Claim() / ToClaim()     – cascade-scope cache for expensive lookups
//   handleNodeOrderCompleted                     – table-driven dispatcher
//   match* / apply* per cascade row              – the table rows (see completion_table.go)
//   handleKeepStagedOrderBCompletion             – intentionally disabled no-op (rewire seam preserved)
//   handleNormalReplenishment                    – terminal-row handler (void; adapted via applyNormalReplenishmentTerminal)
//   maybePreStage                                – intentionally disabled no-op (called from handleNormalReplenishment)
//   handleOrphanedTaskOrderCompleted             – non-success terminal path (cancelled / failed)
//   handleNodeOrderFailed                        – EventOrderFailed counterpart (lives here because
//                                                  it reads the same node-task context the completion
//                                                  rows do; folding the two negative- and positive-path
//                                                  counterparts together keeps changeover orchestration
//                                                  in one place rather than in a standalone wiring_failed.go).

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
// Loaded once by loadOrderCompletionCtx and passed to each table row's
// Match and Apply. Cascade-scope cache for expensive lookups (claim,
// toClaim) so multiple Match predicates that need the same value don't
// each hit the DB.
type orderCompletionCtx struct {
	order     *storeorders.Order
	node      *processes.Node
	runtime   *processes.RuntimeState
	toStyleID int64
	nodeTask  *processes.NodeTask // nil when no active changeover

	// e is the engine reference used by lazy field accessors below to
	// perform their DB lookups on first access.
	e *Engine

	// Lazy field — the active NodeClaim at ctx.node. Resolved by Claim().
	// claimResolved distinguishes "not fetched yet" from "fetched and nil";
	// both are valid states (nil = no active style or claim row missing).
	claim         *processes.NodeClaim
	claimResolved bool

	// Lazy field — the to-style NodeClaim at ctx.node during a changeover.
	// Resolved by ToClaim(). toClaim==nil with toClaimResolved==true means
	// no active changeover (toStyleID==0) or the lookup failed.
	toClaim         *processes.NodeClaim
	toClaimResolved bool

	// Lazy field — the from-style NodeClaim attached to ctx.nodeTask via
	// FromClaimID. Distinct from Claim() (active claim by node) and
	// ToClaim() (to-style claim by node + style). FromClaim is a direct
	// lookup by claim id; used by the complex Order B path to read the
	// from-claim's KeepStaged flag. fromClaim==nil with fromClaimResolved==true
	// means no changeover, no FromClaimID on the task, or the lookup failed.
	fromClaim         *processes.NodeClaim
	fromClaimResolved bool
}

// Claim returns the active NodeClaim at ctx.node, caching the lookup so
// repeated calls within one cascade don't re-query. Returns nil if no
// active claim is set (no active style, or the claim row is missing).
func (c *orderCompletionCtx) Claim() *processes.NodeClaim {
	if !c.claimResolved {
		c.claim = findActiveClaim(c.e.db, c.node)
		c.claimResolved = true
	}
	return c.claim
}

// ToClaim returns the to-style NodeClaim at ctx.node, looked up by
// (toStyleID, CoreNodeName). Caches the lookup. Returns nil if there's
// no active changeover or the lookup failed.
func (c *orderCompletionCtx) ToClaim() *processes.NodeClaim {
	if !c.toClaimResolved {
		if c.toStyleID != 0 {
			if tc, err := c.e.db.GetStyleNodeClaimByNode(c.toStyleID, c.node.CoreNodeName); err == nil {
				c.toClaim = tc
			}
		}
		c.toClaimResolved = true
	}
	return c.toClaim
}

// FromClaim returns the from-style NodeClaim attached to ctx.nodeTask
// via FromClaimID, caching the direct-by-id lookup. Returns nil when
// there's no node task, no FromClaimID, or the lookup failed.
func (c *orderCompletionCtx) FromClaim() *processes.NodeClaim {
	if !c.fromClaimResolved {
		if c.nodeTask != nil && c.nodeTask.FromClaimID != nil {
			if fc, err := c.e.db.GetStyleNodeClaim(*c.nodeTask.FromClaimID); err == nil {
				c.fromClaim = fc
			}
		}
		c.fromClaimResolved = true
	}
	return c.fromClaim
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

	ctx := &orderCompletionCtx{order: order, node: node, runtime: runtime, e: e}

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
			// A plain abort leaves the node's active-bin pointer stale: the
			// bin may still be present (cancel-before-pickup) or already gone
			// (post-pickup). Reconcile against Core's physical truth — the
			// same tri-state rebind/clear/retain the changeover-cancel path
			// uses. Best-effort; never blocks the bail. The nodeTask==nil case
			// is vacuous here, so the only guard is a process-node-linked order.
			if ctx.order.ProcessNodeID != nil {
				e.reconcileActiveBinAfterCancel(*ctx.order.ProcessNodeID, ctx.runtime.ActiveClaimID)
			}
		}
		return
	}

	for _, c := range completionChain {
		if !c.Match(ctx) {
			continue
		}
		if c.Apply(e, ctx) {
			return
		}
	}
}

// matchStagedDelivery matches Order A delivering to inbound staging
// during a runout-style changeover. Predicate: this order is the linked
// next-material order on an active node task, the to-style claim has an
// InboundStaging slot configured, and the order's delivery node is that
// staging slot.
func matchStagedDelivery(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.NextMaterialOrderID == nil || *ctx.nodeTask.NextMaterialOrderID != ctx.order.ID {
		return false
	}
	toClaim := ctx.ToClaim()
	return toClaim != nil &&
		toClaim.InboundStaging != "" &&
		ctx.order.DeliveryNode == toClaim.InboundStaging
}

func applyStagedDelivery(e *Engine, ctx *orderCompletionCtx) bool {
	toClaim := ctx.ToClaim() // cached in Match
	claimID := toClaim.ID
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.SetClaimAndCount(ctx.node.ID, &claimID, ctx.runtime.RemainingUOPCached); err != nil {
			log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
		}
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, domain.NodeTaskStaged); err != nil {
		log.Printf("update node task %d to staged: %v", ctx.nodeTask.ID, err)
	}
	if err := e.tryCompleteProcessChangeover(ctx.node.ProcessID); err != nil {
		log.Printf("changeover: try-complete after staged delivery for process %d: %v", ctx.node.ProcessID, err)
	}
	return true
}

// matchOrderBComplex matches the completion of Order B for the complex-
// evacuate path: linked OldMaterialReleaseOrderID + complex order type +
// swap-or-evacuate situation. Apply transitions the task to "released"
// after delegating to the shelved KeepStaged no-op hook.
func matchOrderBComplex(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.OldMaterialReleaseOrderID == nil || *ctx.nodeTask.OldMaterialReleaseOrderID != ctx.order.ID {
		return false
	}
	return ctx.order.OrderType == orders.TypeComplex &&
		(ctx.nodeTask.Situation == "swap" || ctx.nodeTask.Situation == "evacuate")
}

// applyOrderBComplex executes the complex Order B side effects:
// state transition to "released" and a try-complete on the parent
// changeover. UOP reset binds at delivery via handleNodeOrderDelivered,
// not here — release-click writes the incoming supply bin's UOP, the
// delivery handler re-affirms with the actually-arrived bin.
//
// For sequential EVAC, OrderB delivers to PairedCoreNode (not
// ctx.node.CoreNodeName which is the primary). Per-slot resets fire
// from handleNodeOrderDelivered for each leg of a sequential SWAP
// terminal step; applyOrderBComplex only advances the state machine.
//
// KeepStaged claims invoke the shelved no-op handler first; it returns
// false (no-op) so legacy claims with KeepStaged=true fall through to
// the standard "released" path. See implementer notes' "Known issue —
// phantom-inventory pin latent under CO-0b fall-through" for the
// rewire-time risk this falls through to. The function call is
// preserved as a one-line rewire seam.
func applyOrderBComplex(e *Engine, ctx *orderCompletionCtx) bool {
	if fc := ctx.FromClaim(); fc != nil && fc.KeepStaged {
		if e.handleKeepStagedOrderBCompletion(ctx) {
			return true
		}
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, domain.NodeTaskReleased); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	if err := e.tryCompleteProcessChangeover(ctx.node.ProcessID); err != nil {
		log.Printf("changeover: try-complete after complex Order B for process %d: %v", ctx.node.ProcessID, err)
	}
	return true
}

// matchOrderBSimple matches the completion of Order B for the simple-move
// path: manual path or drop task. Order type Move (any situation), or
// Complex order type that ISN'T in the swap/evacuate situation handled
// by matchOrderBComplex. The non-complex-evac discrimination is encoded
// explicitly rather than left to cascade ordering, mirroring the
// matchChangeoverRelease pattern.
func matchOrderBSimple(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil || ctx.nodeTask.OldMaterialReleaseOrderID == nil || *ctx.nodeTask.OldMaterialReleaseOrderID != ctx.order.ID {
		return false
	}
	if ctx.order.OrderType == orders.TypeMove {
		return true
	}
	if ctx.order.OrderType == orders.TypeComplex {
		// Complex in a non-swap/evacuate situation — order_b_complex won't
		// match; this row picks it up.
		return ctx.nodeTask.Situation != "swap" && ctx.nodeTask.Situation != "evacuate"
	}
	return false
}

// applyOrderBSimple executes the simple-move Order B side effects:
// clear active_bin_id at the slot (the bin physically left) and stamp
// the task to line_cleared unless the task is already terminal at plan
// time (drop tasks without the evacuate marker land at line_cleared
// during planning — skip the redundant state write to avoid churning
// updated_at).
func applyOrderBSimple(e *Engine, ctx *orderCompletionCtx) bool {
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.ClearActiveAndReset(ctx.node.ID, ctx.runtime.ActiveClaimID); err != nil {
			log.Printf("set runtime for node %d: %v", ctx.node.ID, err)
		}
	}
	if domain.IsNodeTaskStateTerminal(ctx.nodeTask.State, ctx.nodeTask.Situation) {
		return true
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, domain.NodeTaskLineCleared); err != nil {
		log.Printf("update node task %d to line_cleared: %v", ctx.nodeTask.ID, err)
	}
	return true
}

// Intentionally disabled — handleKeepStagedOrderBCompletion is a short-
// circuited no-op preserved as the one-line rewire seam for the shelved
// KeepStaged path. Called from applyOrderBComplex when the from-claim has
// KeepStaged=true.
//
// Returning false makes applyOrderBComplex fall through to the standard
// "released" path, which is the desired behaviour until KeepStaged is
// rewired. See implementer notes' "Known issue — phantom-inventory pin
// latent under CO-0b fall-through" for the rewire-time risk this falls
// through to.
func (e *Engine) handleKeepStagedOrderBCompletion(ctx *orderCompletionCtx) bool {
	_ = ctx
	return false
}

// matchChangeoverRelease matches Order A completing to release staged or
// replenished material into production during a changeover (non-staging
// delivery path). Two-part predicate:
//
//  1. The order is the node task's linked next-material order (shared
//     with matchStagedDelivery's first guard).
//  2. The delivery is NOT to the to-claim's InboundStaging slot — that
//     case belongs to staged_delivery.
//
// The non-staging discrimination is encoded here explicitly rather than
// being left to cascade ordering. Pre-fix, the predicate was just (1),
// relying on staged_delivery running first in completionChain. That meant
// a future commit reordering the slice (or inserting a new row between
// staged_delivery and changeover_release) would silently change behavior.
// With (2) made explicit, the row is self-describing and reorder-safe.
//
// UOP reset runs on delivery completion (not at release click). Release
// only flips the node task to "released"; the runtime turnover is bound
// to the arrival event so a fault between release and delivery doesn't
// leave the line UI showing capacity for a bin that hasn't landed.
//
// Sequential SWAP ships as a single complex order with a mid-sequence
// cutover wait. Its terminal step is the ACTIVE-side dropoff: by then,
// both physical positions (CoreNodeName and PairedCoreNode) hold new
// bins. Per-slot resets fire from handleNodeOrderDelivered for each leg;
// release here only advances the task state machine.
func matchChangeoverRelease(ctx *orderCompletionCtx) bool {
	if ctx.nodeTask == nil ||
		ctx.nodeTask.NextMaterialOrderID == nil ||
		*ctx.nodeTask.NextMaterialOrderID != ctx.order.ID {
		return false
	}
	// Reject the staged-delivery shape: to-claim configured with an
	// InboundStaging slot AND the order delivers to that slot.
	toClaim := ctx.ToClaim()
	if toClaim != nil &&
		toClaim.InboundStaging != "" &&
		ctx.order.DeliveryNode == toClaim.InboundStaging {
		return false
	}
	return true
}

func applyChangeoverRelease(e *Engine, ctx *orderCompletionCtx) bool {
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, domain.NodeTaskReleased); err != nil {
		log.Printf("update node task %d to released: %v", ctx.nodeTask.ID, err)
	}
	if err := e.tryCompleteProcessChangeover(ctx.node.ProcessID); err != nil {
		log.Printf("changeover: try-complete after release for process %d: %v", ctx.node.ProcessID, err)
	}
	return true
}

// matchLoaderEmptyIn matches an L1 retrieve_empty order completing at a
// manual_swap producer (loader) node. L1 brought an empty to the loader;
// the operator filled it; CONFIRM means the bin is ready to send back to
// the supermarket. Apply fires the L2 (filled-out) move.
//
// The OutboundDestination validity checks live in Apply rather than Match
// to preserve the pre-table fall-through-with-warning behavior: a malformed
// claim logs a diagnostic and lets the cascade continue to
// handleNormalReplenishment for default cleanup. PR 3 may revisit.
func matchLoaderEmptyIn(ctx *orderCompletionCtx) bool {
	if !ctx.order.RetrieveEmpty {
		return false
	}
	claim := ctx.Claim()
	return claim != nil &&
		claim.SwapMode == protocol.SwapModeManualSwap &&
		claim.Role == protocol.ClaimRoleProduce
}

func applyLoaderEmptyIn(e *Engine, ctx *orderCompletionCtx) bool {
	claim := ctx.Claim() // cached in Match
	// L2 outbound routing comes from the loader AGGREGATE (the config source of truth),
	// not the legacy style_node_claim — this severs the completion handler's dependency
	// on style_node_claims-as-loader-config (keystone step 5). Fall back to the claim
	// when the loader can't be resolved (cutover flag off / cache cold), so this is
	// behaviour-preserving across the cutover (loader.OutboundDest() == the claim's
	// outbound under both the aggregate and the legacy claim projection).
	outbound := claim.OutboundDestination
	if l, err := e.loaders().LoaderAt(domain.NodeID(ctx.node.CoreNodeName), domain.RoleProduce); err == nil && l != nil && l.OutboundDest() != "" {
		outbound = l.OutboundDest()
	}
	if outbound == "" {
		e.logFn("side-cycle: loader %s has no OutboundDestination — cannot create L2 (filled bin will sit until operator manually moves it)", ctx.node.Name)
		return false
	}
	if outbound == ctx.node.CoreNodeName {
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
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1, ctx.node.CoreNodeName, outbound, loadedPayloadCode, true)
	if err != nil {
		e.logFn("side-cycle: create L2 (filled-out) for loader %s: %v", ctx.node.Name, err)
		return false
	}
	log.Printf("side-cycle: L2 (filled-out) order %d for loader %s → %s payload=%q", order.ID, ctx.node.Name, outbound, loadedPayloadCode)
	// Runtime cache binding is owned by the delivered handler — L1's
	// empty bin landing at the loader already wrote active_bin_id /
	// remaining_uop_cached. Confirm only swaps the active order pointer
	// so the loader UI shows L2 next.
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, &order.ID, nil); err != nil {
		log.Printf("side-cycle: update runtime orders for loader %d: %v", ctx.node.ID, err)
	}
	return true
}

// matchManualSwap matches a move order completion on a manual_swap node.
// The bin has been sent to destination, the node is vacant.
func matchManualSwap(ctx *orderCompletionCtx) bool {
	if ctx.order.OrderType != orders.TypeMove {
		return false
	}
	claim := ctx.Claim()
	return claim != nil && claim.SwapMode == protocol.SwapModeManualSwap
}

func applyManualSwap(e *Engine, ctx *orderCompletionCtx) bool {
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
	claim := ctx.Claim() // cached on first access in Match
	if claim.Role == protocol.ClaimRoleConsume && claim.AutoPush {
		e.MaybePushUnloader(ctx.node.ID)
	}
	// Push-driven loader (transitional): L2 just landed at the market, so the
	// loader window is confirmed free — stage the next empty. MaybePushLoader
	// gates internally on transitional, so this is a no-op for ordinary
	// (threshold/legacy-supplied) loaders.
	if claim.Role == protocol.ClaimRoleProduce {
		e.MaybePushLoader(ctx.node.ID)
	}
	return true
}

// matchProduceIngest matches an ingest order completion at a produce node.
// Core has just received the bin's manifest; this row's Apply clears the
// runtime order pointers so the next produce cycle starts clean.
//
// Cache state is owned elsewhere (resetProduceRuntime at FinalizeProduceNode
// and handleNodeOrderDelivered when the next empty bin lands). Confirm is
// a no-op for cache.
func matchProduceIngest(ctx *orderCompletionCtx) bool {
	if ctx.order.OrderType != orders.TypeIngest {
		return false
	}
	claim := ctx.Claim()
	return claim != nil && claim.Role == protocol.ClaimRoleProduce
}

func applyProduceIngest(e *Engine, ctx *orderCompletionCtx) bool {
	if err := e.db.UpdateProcessNodeRuntimeOrders(ctx.node.ID, nil, nil); err != nil {
		log.Printf("update runtime orders for node %d: %v", ctx.node.ID, err)
	}
	return true
}

// handleNormalReplenishment handles standard retrieve/complex order completion.
// Cache binding is owned by handleNodeOrderDelivered — the delivered
// bin's authoritative UOP arrives on its OrderDelivered envelope and
// seeds active_bin_id + remaining_uop_cached. Confirm only does
// order-pointer bookkeeping for manual_swap nodes and fires the
// keep-staged hook (currently a no-op).
func (e *Engine) handleNormalReplenishment(ctx *orderCompletionCtx) {
	if ctx.order.OrderType != orders.TypeRetrieve && ctx.order.OrderType != orders.TypeComplex {
		return
	}
	claim := ctx.Claim()
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

// Intentionally disabled — maybePreStage is a short-circuited no-op.
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
	if changeoverState.IsTerminal() {
		log.Printf("orphan: skip task %d stamp — changeover %d already %s", task.ID, task.ProcessChangeoverID, changeoverState)
		return
	}
	newState := domain.NodeTaskCancelled
	if order.Status == orders.StatusFailed {
		newState = domain.NodeTaskError
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
		newState := domain.NodeTaskError
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
		//
		// Round-3 Item C addition: dropoff-side capacity exhaustion
		// (destination full, NGRP saturated) is operationally distinct
		// from "no bin at pickup" — the changeover hasn't failed, the
		// system is waiting for storage space to open up. Tag those as
		// NodeTaskCapacityBlocked so the HMI renders amber rather than
		// red, and so a downstream consumer can distinguish "retry
		// when capacity opens" from "operator must intervene."
		if nodeTask.Situation == "drop" {
			switch {
			case isNoBinFailure(failed.Reason):
				newState = domain.NodeTaskLineCleared
			case isCapacityBlocked(failed.Reason):
				newState = domain.NodeTaskCapacityBlocked
			}
		}
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, newState); err != nil {
			log.Printf("update node task %d to %s: %v", nodeTask.ID, newState, err)
		}
		switch newState {
		case domain.NodeTaskLineCleared:
			note := "evac auto-cleared: no bin to remove at " + nodeTask.NodeName
			if err := e.db.SetChangeoverNodeTaskSkipNote(nodeTask.ID, note); err != nil {
				log.Printf("changeover: set skip_note on drop auto-clear for task %d: %v", nodeTask.ID, err)
			}
			log.Printf("changeover: drop auto-cleared for node %s — order %d failed with %q", node.Name, order.ID, failed.Reason)
		case domain.NodeTaskCapacityBlocked:
			note := "drop waiting for downstream capacity: " + failed.Reason
			if err := e.db.SetChangeoverNodeTaskSkipNote(nodeTask.ID, note); err != nil {
				log.Printf("changeover: set skip_note on capacity-blocked task %d: %v", nodeTask.ID, err)
			}
			// Distinct log tag — counts let us validate whether the
			// typed ReasonCode refactor (Item D) is needed in practice
			// (Dev A's note on the Item C plan). Grep "changeover:
			// drop capacity-blocked" in production logs to size the
			// occurrence rate.
			log.Printf("changeover: drop capacity-blocked for node %s — order %d queued with reason=%q", node.Name, order.ID, failed.Reason)
		default:
			log.Printf("changeover: order failed for node %s, marked as error — manual retry needed", node.Name)
		}
		if err := e.tryCompleteProcessChangeover(node.ProcessID); err != nil {
			log.Printf("changeover: try-complete after order-failure for process %d: %v", node.ProcessID, err)
		}
	}
}

// isNoBinFailure recognizes Core's bin-claim failure shape on
// OrderFailedEvent.Reason for PICKUP-side bin unavailability — the
// "slot was already vacant" semantics where the drop can be auto-
// cleared as LineCleared because there's nothing physically to move.
//
// Core's planning error codes ("no_bin", "no_source_bin") are not
// preserved end-to-end through the wire — only the detail string is —
// so we match on the detail. Substrings cover both pickup-shaped reasons:
//
//   - no_bin         "no available bin at pickup node(s) for order N"
//   - no_source_bin  "no bin at pickup node(s) for order N — source was emptied externally"
//
// Strings are stable in complex_claims.go:139/141. If those move, add
// the new wording here or plumb the code through OrderFailedEvent.
//
// IMPORTANT: order of matchers in the call site matters — this matcher
// MUST be checked before isCapacityBlocked. The capacity matcher uses a
// looser "no available bin at " prefix that would otherwise swallow
// pickup-side failures.
func isNoBinFailure(reason string) bool {
	return strings.Contains(reason, "no available bin at pickup") ||
		strings.Contains(reason, "no bin at pickup")
}

// isCapacityBlocked recognizes Core's DROPOFF-side capacity failure
// shapes — destination occupied, NGRP saturated, no slot available.
// These are operationally retriable: the changeover hasn't failed,
// the system is waiting for storage capacity to open up. The HMI
// renders these as amber tiles (NodeTaskCapacityBlocked), distinct
// from green (LineCleared/Switched) and red (Error).
//
// Substrings match the dispatch error shapes produced by:
//   - dispatch/binresolver/group_resolver.go:440,518
//     "no available slot in node group %s"
//   - dispatch/binresolver/group_resolver.go:298,323
//     "no bin of requested payload in node group %s"
//   - dispatch/planning_service.go:552 "no available bin at %s"
//     (planStore source-side; the source line has no claimable bin
//     so the changeover's drop can't physically run — practically
//     a capacity-style condition from the operator's POV)
//
// Order-of-matching contract — isNoBinFailure runs FIRST in the caller
// so the looser "no available bin at " prefix here doesn't reclassify
// pickup-side failures as capacity blocks.
func isCapacityBlocked(reason string) bool {
	return strings.Contains(reason, "no available slot in node group") ||
		strings.Contains(reason, "no available bin at ") ||
		strings.Contains(reason, "no bin of requested payload in node group")
}
