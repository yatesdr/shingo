// wiring_status_changed.go — handlers subscribed to EventOrderStatusChanged.
//
// Two handlers fire on every order status transition:
//   handleSequentialBackfill   – auto-create Order B (backfill) when
//                                Order A enters in_transit on a sequential
//                                swap-mode node.
//   handleAutoReleaseOnStaged  – close the two-robot consolidated-release
//                                timing window: when one robot's order
//                                arrives at staged after the operator has
//                                already triggered the consolidated path
//                                via its sibling, auto-release the late
//                                arrival rather than wait for a second click.
//
// Both are wired by wireEventHandlers (wiring.go).

package engine

import (
	"log"

	"shingoedge/orders"
)

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

// handleAutoReleaseOnStaged closes the timing window in the two-robot
// consolidated release. Pre-fix, the operator had to click RELEASE during the
// (often non-existent) instant where BOTH tracked orders were simultaneously
// in "staged" status. In production the two robots arrive seconds apart, so
// the window often did not exist when the operator looked, and they fell back
// to the admin orders page (which had its own disposition bug — see kanbans.js
// item 1.1).
//
// New behavior: the consolidated RELEASE button (computeSwapReady) shows when
// at least one tracked order is staged + both non-terminal. Clicking it
// releases the staged sibling immediately and skips the not-yet-staged one
// (releaseIfStaged tolerates that). When the late-arriving sibling later
// transitions to "staged", THIS handler observes the event, sees that the
// sibling has already moved past staged (proving the operator already
// triggered the consolidated path), and auto-releases the late arrival with
// the default `capture_lineside` disposition.
//
// Disposition choice: capture_lineside is the safe production default (clears
// the bin's manifest at Core, captures any pulled lineside parts as buckets).
// Matches what the admin Release button now sends (kanbans.js fix 1.1) and
// what the operator most commonly intends. If a non-default disposition was
// originally meant, the operator can still recover by aborting and re-issuing
// — but in practice the auto-release path is the common case.
//
// Idempotency / safety:
//   - Only fires when the just-staged order is one of the runtime's tracked
//     pair AND the sibling is non-terminal AND past "staged". That sibling-
//     past-staged check is the implicit "operator already clicked" marker
//     (per Dev C's hasConsolidatedReleasePending design).
//   - Re-uses ReleaseOrderWithLineside, which is idempotent on already-released
//     orders — but the sibling-past-staged guard prevents double-firing in
//     practice.
//   - claim.SwapMode != "two_robot" → skip. Sequential / single_robot don't
//     use the consolidated release path.
func (e *Engine) handleAutoReleaseOnStaged(changed OrderStatusChangedEvent) {
	if changed.NewStatus != orders.StatusStaged || changed.ProcessNodeID == nil {
		return
	}
	nodeID := *changed.ProcessNodeID
	runtime, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || runtime == nil {
		return
	}
	// Identify which slot this order is in and pick out the sibling.
	var siblingID *int64
	switch {
	case runtime.ActiveOrderID != nil && *runtime.ActiveOrderID == changed.OrderID:
		siblingID = runtime.StagedOrderID
	case runtime.StagedOrderID != nil && *runtime.StagedOrderID == changed.OrderID:
		siblingID = runtime.ActiveOrderID
	default:
		return // not a tracked order on this node
	}
	if siblingID == nil {
		return // single-order cycle, no consolidated path
	}
	sibling, err := e.db.GetOrder(*siblingID)
	if err != nil || sibling == nil {
		return
	}
	// Predicate: only fire if the sibling has actually been RELEASED from
	// staged — operator press, or a prior auto-release. Detected via
	// OrderHistory (a row with old=staged, new=in_transit), NOT current
	// status, because status alone is unreliable.
	//
	// Why status alone is wrong: in_transit is ALSO reached on Core's
	// waybill ack (manager.go:511, HandleDispatchReply ReplyWaybill). That
	// fires automatically when Core dispatches the fleet — no operator
	// involvement. With a status-only predicate, both swap orders flip to
	// in_transit on dispatch ack, then the first to stage triggers auto-
	// release on the second — both robots release without operator consent
	// and immediately pick the bin right back up. Plant-test failure
	// 2026-04-27 (AMR-03 / AMR-05).
	//
	// Pre-Round-4 the predicate was "!staged && !terminal" (admitted
	// dispatched/etc.); Round 4 narrowed to {in_transit, delivered}; both
	// share the same flaw — status doesn't carry release intent. The
	// staged → in_transit history row does, because Manager.ReleaseOrder
	// is the only emitter of that exact transition (manager.go:331,
	// detail "released from staging").
	if !e.siblingWasReleasedFromStaging(*siblingID) {
		return
	}
	// Confirm two_robot mode via the active claim — defense in depth, since
	// the runtime slot pattern is also used by sequential mode.
	if runtime.ActiveClaimID == nil {
		return
	}
	claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil || claim == nil || claim.SwapMode != "two_robot" {
		return
	}
	// Determine which leg we're auto-releasing. Order B (StagedOrderID) is the
	// evac and gets the full disposition; Order A (ActiveOrderID) is the supply
	// and gets the empty disposition (matches ReleaseStagedOrders' split). This
	// preserves the "don't re-clear the freshly-loaded supply bin" invariant.
	disp := ReleaseDisposition{CalledBy: "auto-release-on-staged"}
	if runtime.StagedOrderID != nil && *runtime.StagedOrderID == changed.OrderID {
		disp.Mode = DispositionCaptureLineside
	}
	if err := e.ReleaseOrderWithLineside(changed.OrderID, disp); err != nil {
		log.Printf("auto-release-on-staged: order %d on node %d: %v", changed.OrderID, nodeID, err)
		return
	}
	log.Printf("auto-release-on-staged: released order %d on node %d (sibling %d status=%s)", changed.OrderID, nodeID, *siblingID, sibling.Status)
}

// siblingWasReleasedFromStaging returns true if the given order has at some
// point transitioned staged → in_transit. That exact transition is the
// unique fingerprint of Manager.ReleaseOrder firing (manager.go:331, detail
// "released from staging"). The waybill-ack path goes acknowledged →
// in_transit, and other paths don't reach in_transit at all, so this row
// only exists when an operator press (or a prior auto-release) actually
// triggered the consolidated release path.
func (e *Engine) siblingWasReleasedFromStaging(siblingID int64) bool {
	hist, err := e.db.ListOrderHistory(siblingID)
	if err != nil {
		log.Printf("auto-release-on-staged: list sibling %d history: %v", siblingID, err)
		return false
	}
	for _, h := range hist {
		if h.OldStatus == orders.StatusStaged && h.NewStatus == orders.StatusInTransit {
			return true
		}
	}
	return false
}
