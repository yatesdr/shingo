// wiring_status_changed.go — handlers subscribed to EventOrderStatusChanged.
//
// One handler:
//   handleSequentialBackfill   – auto-create Order B (backfill) when
//                                Order A enters in_transit on a sequential
//                                swap-mode node.
//
// Wired by wireEventHandlers (wiring.go).
//
// Note: handleAutoReleaseOnStaged was removed 2026-04-27 along with the
// auto-release coordination layer. The simplified consolidated-release path
// (ReleaseStagedOrders) now fans out to both legs of a two-robot swap on
// the operator's single click, so there's no late-sibling auto-release to
// coordinate. See shingo_todo.md and the retrospective for context.

package engine

import "log"

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
