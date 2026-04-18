// wiring_auto_return.go — Auto-return order creation.
//
// maybeCreateReturnOrder creates STORE orders to send bins back to their
// origin when an in-flight order is cancelled or fails. Each bin is
// routed to the root parent of its pickup node so the group resolver
// can pick the best slot. Currently short-circuited via
// autoReturnEnabled = false — see the constant docstring for context.

package engine

import (
	"fmt"

	"shingocore/dispatch"
	"shingocore/store"
)

// autoReturnEnabled gates maybeCreateReturnOrder. SHORT-CIRCUITED 2026-04-14:
// auto-return has not been observed to complete successfully in production.
// The created store orders sit in `pending` status indefinitely because
// EventOrderReceived is audit-only and the fulfillment scanner doesn't process
// store-type orders. The bin claim from the never-dispatched return order
// strands the bin until the orphan claim sweep runs.
//
// Re-enable only after the dispatch path for internally-created store orders
// is verified end-to-end. See shingobugs414-stephen-questions.md for the full
// investigation. Function body preserved so this can be flipped back with a
// single edit if/when the underlying issue is fixed.
const autoReturnEnabled = false

// maybeCreateReturnOrder creates STORE orders to return bins to their origins
// when an in-flight order is cancelled or fails. Each bin is routed to the
// root parent of its pickup node so the group resolver can pick the best slot.
//
// For multi-bin orders (junction table populated), a separate return order is
// created for each bin. For single-bin orders, the legacy path creates one.
//
// Currently short-circuited — see autoReturnEnabled.
func (e *Engine) maybeCreateReturnOrder(order *store.Order, reason string) {
	if !autoReturnEnabled {
		e.logFn("engine: auto-return short-circuited for order %d (%s)", order.ID, reason)
		return
	}

	// If the fleet never accepted the order (no vendor order ID), the bin
	// never left its origin — no return needed. This prevents spurious
	// auto_return orders when dispatch fails at the fleet API level.
	if order.VendorOrderID == "" {
		e.logFn("engine: order %d failed before fleet accepted it, skipping auto-return", order.ID)
		return
	}

	switch order.Status {
	case dispatch.StatusDispatched, dispatch.StatusInTransit, dispatch.StatusStaged,
		dispatch.StatusFailed, dispatch.StatusCancelled:
		// These are states where the bin may have left its origin
	default:
		return
	}

	// Don't create return orders for return orders (prevent infinite loops)
	if order.PayloadDesc == "auto_return" {
		e.logFn("engine: order %d is already a return order, skipping auto-return", order.ID)
		return
	}

	// Skip auto-return for complex orders when the bin position is uncertain.
	// ApplyBinArrival only fires on FINISHED, so on failure mid-transit the DB
	// still shows the original pickup node while the bin is physically wherever
	// the robot stopped. A return order with the wrong source just sits forever.
	//
	// Exception: StatusStaged means the robot reached a wait node and dropped
	// the bin — the DB knows the bin's actual position, so a return is safe.
	if order.OrderType == dispatch.OrderTypeComplex && order.Status != dispatch.StatusStaged {
		e.logFn("engine: order %d is complex (status=%s), skipping auto-return (bin position uncertain)", order.ID, order.Status)
		return
	}

	// Don't create return orders for compound/reshuffle children
	if order.ParentOrderID != nil {
		return
	}

	// Multi-bin path: check junction table first.
	// Each bin gets its own return order with SourceNode set to the bin's
	// original pickup node (ob.NodeName), not the order's global SourceNode.
	//
	// INVARIANT: bins are still at their original pickup positions from the
	// DB's perspective because ApplyBinArrival never fires on cancelled/failed
	// orders. If partial-completion tracking (per-block receipts) is added
	// later, this assumption breaks and bins may be at intermediate positions.
	// Revisit this function if that feature is implemented.
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		for _, ob := range orderBins {
			e.createSingleReturnOrder(order, ob.BinID, ob.NodeName, reason)
		}
		e.db.DeleteOrderBins(order.ID)
		return
	}

	// Legacy single-bin path
	if order.BinID == nil {
		return
	}
	if order.SourceNode == "" {
		e.logFn("engine: order %d has no source node, cannot create return order", order.ID)
		return
	}
	e.createSingleReturnOrder(order, *order.BinID, order.SourceNode, reason)
}

// createSingleReturnOrder creates one STORE order to return a specific bin
// from its current location (sourceNodeName) to the root parent of that node.
func (e *Engine) createSingleReturnOrder(order *store.Order, binID int64, sourceNodeName, reason string) {
	sourceNode, err := e.db.GetNodeByDotName(sourceNodeName)
	if err != nil {
		e.logFn("engine: resolve source node %q for return order: %v", sourceNodeName, err)
		return
	}

	rootNode, err := e.db.GetRootNode(sourceNode.ID)
	if err != nil {
		e.logFn("engine: resolve root node for %q: %v", sourceNodeName, err)
		return
	}

	returnOrder := &store.Order{
		StationID:    order.StationID,
		OrderType:    dispatch.OrderTypeStore,
		Status:       dispatch.StatusPending,
		SourceNode:   sourceNodeName, // bin is still at origin — ApplyBinArrival never fires on failed/cancelled orders
		DeliveryNode: rootNode.Name,
		BinID:        &binID,
		PayloadDesc:  "auto_return",
	}

	if err := e.db.CreateOrder(returnOrder); err != nil {
		e.logFn("engine: create return order for order %d bin %d: %v", order.ID, binID, err)
		return
	}

	// Claim the bin for the return order. The bin was already unclaimed by
	// UnclaimOrderBins on the cancel/fail path, so claimed_by IS NULL.
	if err := e.db.ClaimBin(binID, returnOrder.ID); err != nil {
		e.logFn("engine: claim bin %d for return order %d: %v", binID, returnOrder.ID, err)
	}

	e.logFn("engine: created return order %d (store %s to %s) for %s order %d bin %d",
		returnOrder.ID, sourceNodeName, rootNode.Name, reason, order.ID, binID)
	e.db.AppendAudit("order", returnOrder.ID, "auto_return", "",
		fmt.Sprintf("returning bin %d from %s order %d", binID, reason, order.ID), "system")

	e.Events.Emit(Event{Type: EventOrderReceived, Payload: OrderReceivedEvent{
		OrderID:      returnOrder.ID,
		StationID:    returnOrder.StationID,
		OrderType:    returnOrder.OrderType,
		DeliveryNode: returnOrder.DeliveryNode,
	}})
}
