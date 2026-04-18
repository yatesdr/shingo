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
	"shingocore/store"
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

	if err := e.db.UpdateOrderStatus(order.ID, newStatus, fmt.Sprintf("fleet: %s -> %s", ev.OldStatus, ev.NewStatus)); err != nil {
		e.logFn("engine: update order %d status to %s: %v", order.ID, newStatus, err)
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

func (e *Engine) handleFleetOrderFailed(order *store.Order) {
	if err := e.db.FailOrderAtomic(order.ID, "fleet order failed"); err != nil {
		e.logFn("engine: atomic fail order %d: %v", order.ID, err)
	}
	e.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
		ErrorCode: "fleet_failed",
		Detail:    "fleet order failed",
	}})
}

func (e *Engine) handleFleetOrderCancelled(order *store.Order) {
	// order.Status is the in-memory value loaded before UpdateOrderStatus ran,
	// so it reflects the status prior to the cancellation update.
	previousStatus := order.Status
	if err := e.db.CancelOrderAtomic(order.ID, "fleet order stopped"); err != nil {
		e.logFn("engine: atomic cancel order %d: %v", order.ID, err)
	}
	e.Events.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:        order.ID,
		EdgeUUID:       order.EdgeUUID,
		StationID:      order.StationID,
		Reason:         "fleet order stopped",
		PreviousStatus: previousStatus,
	}})
}
