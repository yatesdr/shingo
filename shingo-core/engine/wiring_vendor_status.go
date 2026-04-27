// wiring_vendor_status.go — Fleet status change handling.
//
// handleVendorStatusChange maps vendor state strings to ShinGo order
// status, writes the transition to the DB, and emits the appropriate
// edge notifications (waybill on first robot assignment, status update,
// staged notification). On terminal states it dispatches to the
// delivery / failure / cancel handlers.

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/orders"
)

func (e *Engine) handleVendorStatusChange(ev OrderStatusChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for status change: %v", ev.OrderID, err)
		return
	}

	// Compute effective robot ID (handles Case D: preserves existing robot when event has empty robotID)
	effectiveRobotID := order.RobotID
	if ev.RobotID != "" {
		effectiveRobotID = ev.RobotID
	}

	// First robot assignment: send waybill only (no DB write here - Option C)
	if ev.RobotID != "" && order.RobotID == "" {
		if err := e.sendToEdge(protocol.TypeOrderWaybill, order.StationID, &protocol.OrderWaybill{
			OrderUUID: order.EdgeUUID,
			WaybillID: order.VendorOrderID,
			RobotID:   ev.RobotID,
		}); err != nil {
			e.logFn("engine: waybill: %v", err)
		}
	}

	newStatus := e.fleet.MapState(ev.NewStatus)
	if newStatus == order.Status {
		// Idempotent path: status unchanged, check if robot ID changed
		if effectiveRobotID != order.RobotID {
			if err := e.db.UpdateOrderRobotID(order.ID, effectiveRobotID); err != nil {
				e.logFn("engine: update order %d robot: %v", order.ID, err)
			}
		}
		return
	}

	// Vendor-state-domain mapping has already produced newStatus (a ShinGo
	// status). Route through the typed lifecycle method matching the
	// target ShinGo status — the vendor terminal-check at line 81 below
	// guards against routing terminal vendor states through MarkInTransit/
	// MarkStaged. Cancel/Fail are dispatched in the post-mapping switch.
	lc := e.dispatcher.Lifecycle()
	switch newStatus {
	case dispatch.StatusInTransit:
		if err := lc.MarkInTransit(order, effectiveRobotID, "fleet"); err != nil {
			e.logFn("engine: mark in_transit order %d: %v", order.ID, err)
		}
	case dispatch.StatusStaged:
		if err := lc.MarkStaged(order, "fleet"); err != nil {
			e.logFn("engine: mark staged order %d: %v", order.ID, err)
		}
	case dispatch.StatusDelivered:
		if err := lc.MarkDelivered(order, "fleet"); err != nil {
			e.logFn("engine: mark delivered order %d: %v", order.ID, err)
		}
	case dispatch.StatusAcknowledged:
		// TODO(dead-code): unreachable with the current seerrds adapter —
		// fleet.MapState (mappers.go:12) never returns StatusAcknowledged.
		// Kept defensive in case a future fleet adapter reports a distinct
		// ACK phase. Verify before the next refactor.
		if err := lc.Acknowledge(order, "fleet"); err != nil {
			e.logFn("engine: acknowledge order %d: %v", order.ID, err)
		}
	case dispatch.StatusDispatched:
		// Fleet shouldn't actually report Dispatched — the dispatcher
		// writes that status itself after backend.CreateOrder returns.
		// If we see it from MapState, log and skip rather than silently
		// re-writing the status.
		e.logFn("engine: unexpected fleet-reported Dispatched for order %d, skipping", order.ID)
	case dispatch.StatusFailed, dispatch.StatusCancelled:
		// Handled by the post-mapping switch below.
	default:
		// Unknown mapped status — should never fire under the current
		// seerrds adapter (MapState in fleet/seerrds/mappers.go produces
		// only the cases handled above). If it does fire, log and skip
		// rather than silently bypassing the state machine. A mapped
		// status outside the typed-method list signals an adapter
		// mismatch that needs investigation, not a generic DB write.
		e.logFn("engine: unknown mapped status %q for order %d (vendor=%q); skipping write — adapter MapState may be out of sync with dispatch.Status* constants", newStatus, order.ID, ev.NewStatus)
	}
	if err := e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, effectiveRobotID); err != nil {
		e.logFn("engine: update order %d vendor state: %v", order.ID, err)
	}

	// Send status update to ShinGo Edge
	if err := e.sendToEdge(protocol.TypeOrderUpdate, order.StationID, &protocol.OrderUpdate{
		OrderUUID: order.EdgeUUID,
		Status:    newStatus,
		Detail:    fmt.Sprintf("fleet state: %s", ev.NewStatus),
	}); err != nil {
		e.logFn("engine: status update: %v", err)
	}

	// Send dedicated staged notification when robot is dwelling
	if newStatus == dispatch.StatusStaged {
		if err := e.sendToEdge(protocol.TypeOrderStaged, order.StationID, &protocol.OrderStaged{
			OrderUUID: order.EdgeUUID,
			Detail:    "robot dwelling at staging node",
		}); err != nil {
			e.logFn("engine: staged notification: %v", err)
		}
	}

	// Non-terminal states are fully handled above — exit early.
	if !e.fleet.IsTerminalState(ev.NewStatus) {
		return
	}

	switch newStatus {
	case dispatch.StatusDelivered:
		e.handleOrderDelivered(order)
	case dispatch.StatusFailed:
		e.handleFleetOrderFailed(order)
	case dispatch.StatusCancelled:
		e.handleFleetOrderCancelled(order)
	}
}

func (e *Engine) handleFleetOrderFailed(order *orders.Order) {
	// lifecycle.Fail handles atomic transition + emit via the action map.
	if err := e.dispatcher.Lifecycle().Fail(order, order.StationID, "fleet_failed", "fleet order failed"); err != nil {
		e.logFn("engine: fail order %d: %v", order.ID, err)
	}
}

func (e *Engine) handleFleetOrderCancelled(order *orders.Order) {
	// lifecycle.CancelOrder handles fleet-cancel + atomic transition + emit.
	// PreviousStatus is captured by transition() before the status flip and
	// passed through to emitCancelled via the Event.
	e.dispatcher.Lifecycle().CancelOrder(order, order.StationID, "fleet order stopped")
}
