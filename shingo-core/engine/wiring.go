package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store"
)

// sendToEdge builds a protocol envelope and enqueues it for dispatch to an edge station.
func (e *Engine) sendToEdge(msgType string, stationID string, payload any) error {
	coreAddr := protocol.Address{Role: protocol.RoleCore, Station: e.cfg.Messaging.StationID}
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	env, err := protocol.NewEnvelope(msgType, coreAddr, edgeAddr, payload)
	if err != nil {
		return fmt.Errorf("build %s: %w", msgType, err)
	}
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode %s: %w", msgType, err)
	}
	if err := e.db.EnqueueOutbox(e.cfg.Messaging.DispatchTopic, data, msgType, stationID); err != nil {
		e.dbg("EnqueueOutbox %s error (silently dropped): %v", msgType, err)
	}
	return nil
}

func (e *Engine) wireEventHandlers() {
	// When an order is dispatched, track it in the tracker
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderDispatchedEvent)
		if e.tracker == nil {
			return
		}
		// On redirect, the order may already have an old vendor order ID tracked.
		// Look up the order and untrack the old ID if it differs from the new one.
		if order, err := e.db.GetOrder(ev.OrderID); err == nil && order.VendorOrderID != "" && order.VendorOrderID != ev.VendorOrderID {
			e.tracker.Untrack(order.VendorOrderID)
			e.logFn("engine: untracked old vendor order %s for order %d (redirect)", order.VendorOrderID, ev.OrderID)
		}
		e.tracker.Track(ev.VendorOrderID)
		e.logFn("engine: tracking vendor order %s for order %d", ev.VendorOrderID, ev.OrderID)
	}, EventOrderDispatched)

	// When the fleet reports a status change, update our order and notify ShinGo Edge
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderStatusChangedEvent)
		e.dbg("vendor status change: order=%d vendor=%s %s->%s robot=%s", ev.OrderID, ev.VendorOrderID, ev.OldStatus, ev.NewStatus, ev.RobotID)
		e.handleVendorStatusChange(ev)
	}, EventOrderStatusChanged)

	// When an order fails, log it and handle compound orders
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderFailedEvent)
		e.logFn("engine: order %d failed: %s - %s", ev.OrderID, ev.ErrorCode, ev.Detail)
		e.db.AppendAudit("order", ev.OrderID, "failed", "", ev.Detail, "system")

		// If child of a compound order, handle parent failure
		if order, err := e.db.GetOrder(ev.OrderID); err == nil && order.ParentOrderID != nil && e.dispatcher != nil {
			e.dispatcher.HandleChildOrderFailure(*order.ParentOrderID, ev.OrderID)
		}
	}, EventOrderFailed)

	// When an order is completed, update inventory and audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCompletedEvent)
		e.logFn("engine: order %d completed", ev.OrderID)
		e.db.AppendAudit("order", ev.OrderID, "completed", "", "", "system")
		e.handleOrderCompleted(ev)
	}, EventOrderCompleted)

	// When an order is cancelled, audit it
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCancelledEvent)
		e.logFn("engine: order %d cancelled: %s", ev.OrderID, ev.Reason)
		e.db.AppendAudit("order", ev.OrderID, "cancelled", "", ev.Reason, "system")
	}, EventOrderCancelled)

	// When an order is received, audit it
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderReceivedEvent)
		e.logFn("engine: order %d received from %s: %s %s -> %s", ev.OrderID, ev.StationID, ev.OrderType, ev.StyleCode, ev.DeliveryNode)
		e.db.AppendAudit("order", ev.OrderID, "received", "", fmt.Sprintf("%s %s from %s", ev.OrderType, ev.StyleCode, ev.StationID), "system")
	}, EventOrderReceived)

	// Payload changes: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(PayloadChangedEvent)
		e.db.AppendAudit("payload", ev.InstanceID, ev.Action, "", fmt.Sprintf("style=%s node=%d", ev.StyleCode, ev.NodeID), "system")
	}, EventPayloadChanged)

	// Node updates: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(NodeUpdatedEvent)
		e.db.AppendAudit("node", ev.NodeID, ev.Action, "", ev.NodeName, "system")
	}, EventNodeUpdated)

	// Corrections: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(CorrectionAppliedEvent)
		e.db.AppendAudit("correction", ev.CorrectionID, ev.CorrectionType, "", ev.Reason, ev.Actor)
	}, EventCorrectionApplied)
}

func (e *Engine) handleVendorStatusChange(ev OrderStatusChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for status change: %v", ev.OrderID, err)
		return
	}

	// Update robot ID if we got one
	if ev.RobotID != "" && order.RobotID == "" {
		e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, ev.RobotID)

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
		return
	}

	e.db.UpdateOrderStatus(order.ID, newStatus, fmt.Sprintf("fleet: %s -> %s", ev.OldStatus, ev.NewStatus))
	e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, ev.RobotID)

	// Send status update to ShinGo Edge
	if err := e.sendToEdge(protocol.TypeOrderUpdate, order.StationID, &protocol.OrderUpdate{
		OrderUUID: order.EdgeUUID,
		Status:    newStatus,
		Detail:    fmt.Sprintf("fleet state: %s", ev.NewStatus),
	}); err != nil {
		e.logFn("engine: status update: %v", err)
	}

	// Handle terminal states
	if e.fleet.IsTerminalState(ev.NewStatus) {
		switch newStatus {
		case dispatch.StatusDelivered:
			e.handleOrderDelivered(order)
		case dispatch.StatusFailed:
			e.db.UpdateOrderStatus(order.ID, dispatch.StatusFailed, "fleet order failed")
			e.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
				OrderID:   order.ID,
				EdgeUUID:  order.EdgeUUID,
				StationID: order.StationID,
				ErrorCode: "fleet_failed",
				Detail:    "fleet order failed",
			}})
		case dispatch.StatusCancelled:
			e.db.UpdateOrderStatus(order.ID, dispatch.StatusCancelled, "fleet order stopped")
		}
	}
}

func (e *Engine) handleOrderDelivered(order *store.Order) {
	e.db.UpdateOrderStatus(order.ID, dispatch.StatusDelivered, "payload delivered")

	if err := e.sendToEdge(protocol.TypeOrderDelivered, order.StationID, &protocol.OrderDelivered{
		OrderUUID:   order.EdgeUUID,
		DeliveredAt: time.Now().UTC(),
	}); err != nil {
		e.logFn("engine: delivered notification: %v", err)
	}
}

// handleOrderCompleted moves payloads from source to dest after ShinGo Edge confirms physical receipt.
func (e *Engine) handleOrderCompleted(ev OrderCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for completion: %v", ev.OrderID, err)
		return
	}

	// If this is a child of a compound order, advance the parent
	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	if order.PickupNode == "" || order.DeliveryNode == "" {
		return
	}

	destNode, err := e.db.GetNodeByName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for completion: %v", order.DeliveryNode, err)
		return
	}

	sourceNode, _ := e.db.GetNodeByName(order.PickupNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	instances, _ := e.db.ListInstancesByClaimedOrder(order.ID)
	for _, p := range instances {
		e.nodeState.MoveInstance(p.ID, destNode.ID)
		e.Events.Emit(Event{Type: EventPayloadChanged, Payload: PayloadChangedEvent{
			Action:     "moved",
			InstanceID: p.ID,
			StyleCode:  p.StyleName,
			FromNodeID: sourceNodeID,
			ToNodeID:   destNode.ID,
			NodeID:     destNode.ID,
		}})
	}
}
