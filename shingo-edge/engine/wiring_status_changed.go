// wiring_status_changed.go — handlers subscribed to EventOrderStatusChanged.
//
// Two handlers:
//   handleSequentialBackfill    – auto-create Order B (backfill) when
//                                 Order A enters in_transit on a sequential
//                                 swap-mode node.
//   handleSiblingReleaseRefire  – fire a two-robot swap leg's release the
//                                 moment it reaches staged, when that leg was
//                                 deferred by ReleaseStagedOrders (Core would
//                                 have refused it) while its sibling released
//                                 on the same operator click (hop A4-ii).
//
// Wired by wireEventHandlers (wiring.go).
//
// History: handleAutoReleaseOnStaged was removed 2026-04-27 along with the
// auto-release coordination layer, on the theory that ReleaseStagedOrders
// fanning out to both legs unconditionally made a late-sibling auto-release
// unnecessary. The Hopkinsville press-index hang (2026-07-23) showed that
// unconditional fan-out desyncs a not-yet-releasable leg instead; hop A4-i
// makes the fan-out skip such a leg, and handleSiblingReleaseRefire is the
// TARGETED revival of the removed hook — scoped to a leg whose sibling already
// released, it re-fires (never cancels, never re-plans, no timer).

package engine

import (
	"log"

	"shingo/protocol"
)

// handleSequentialBackfill watches for sequential Order A going in_transit
// and auto-creates Order B (backfill) to deliver replacement material.
func (e *Engine) handleSequentialBackfill(changed OrderStatusChangedEvent) {
	if changed.NewStatus != string(protocol.StatusInTransit) || changed.ProcessNodeID == nil {
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
	if claim == nil || claim.SwapMode != protocol.SwapModeSequential {
		return
	}

	steps := BuildSequentialBackfillSteps(claim)
	nodeID := node.ID
	// ATTRIBUTED, and it was not. This order is the plant continuing to serve the
	// demand that produced Order A, so it belongs to that demand's episode — see
	// cellEpisodeOrigin for what an unattributed one costs downstream. It JOINS
	// and never mints: a backfill is never itself the origin of a demand.
	orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.CoreNodeName, claim.CoreNodeName, steps,
		e.cellEpisodeOrigin(node, claim)) // delivery_node = CoreNodeName → resets UOP
	if err != nil {
		log.Printf("sequential backfill for node %s: %v", node.Name, err)
		return
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, runtime.ActiveOrderID, &orderB.ID); err != nil {
		log.Printf("update runtime orders for node %d: %v", nodeID, err)
	}
	// LinkOrderSiblings is log-and-continue here (unlike the three
	// operator-initiated sites which return-error). Rationale:
	//   - This runs in the OrderStatusChanged event handler loop; one
	//     handler failing must not abort message processing.
	//   - The backfill is opportunistic; if linkage fails, the L1/L2
	//     side-cycle still works for the operator (only the consolidated
	//     swap_ready RELEASE coordinates via siblings, and sequential
	//     mode is excluded from that path by the SwapMode gate).
	if err := e.db.LinkOrderSiblings(order.ID, orderB.ID); err != nil {
		log.Printf("link sequential siblings %d↔%d: %v", order.ID, orderB.ID, err)
	}
	log.Printf("sequential backfill: created Order B %d for node %s (Order A %d in_transit)", orderB.ID, node.Name, order.ID)
}

// handleSiblingReleaseRefire fires a two-robot swap leg's release the moment it
// reaches staged, when that leg's consolidated RELEASE was deferred by
// ReleaseStagedOrders because Core would have refused it while its sibling was
// released on the same operator click (hop A4-ii). It completes a release the
// operator ALREADY requested — never a reaper, never an auto-cancel — and fires
// only for a leg whose sibling already went (rememberDeferredSiblingRelease
// records the entry only in that case).
//
// On any terminal transition it drops a stale entry, so a deferred leg that is
// cancelled or fails (rather than reaching staged) can't leak the map.
func (e *Engine) handleSiblingReleaseRefire(changed OrderStatusChangedEvent) {
	newStatus := protocol.Status(changed.NewStatus)

	// Cleanup: a deferred leg that went terminal without ever staging will
	// never re-fire; drop it so the map can't grow without bound.
	if protocol.IsTerminal(newStatus) {
		e.pendingSiblingReleaseMu.Lock()
		delete(e.pendingSiblingRelease, changed.OrderID)
		e.pendingSiblingReleaseMu.Unlock()
		return
	}

	if newStatus != protocol.StatusStaged {
		return
	}

	e.pendingSiblingReleaseMu.Lock()
	disp, ok := e.pendingSiblingRelease[changed.OrderID]
	if ok {
		delete(e.pendingSiblingRelease, changed.OrderID)
	}
	e.pendingSiblingReleaseMu.Unlock()
	if !ok {
		return
	}

	// The leg is at staged now, so it is releasable at Core; releaseIfReleasable
	// is still the gate (defends against a race that moved it again) and reports
	// whether the release actually queued.
	released, err := e.releaseIfReleasable(changed.OrderID, "sibling-release-refire", disp)
	if err != nil {
		e.logFn("sibling-release-refire: order %d reached staged but release failed: %v", changed.OrderID, err)
		return
	}
	if released {
		e.logFn("sibling-release-refire: order %d reached staged — released (sibling already released on the operator's click)", changed.OrderID)
	} else {
		e.logFn("sibling-release-refire: order %d reached staged but was not releasable — dropped", changed.OrderID)
	}
}
