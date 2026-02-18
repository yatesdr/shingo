package engine

import (
	"fmt"
	"time"

	"warpath/dispatch"
	"warpath/messaging"
	"warpath/rds"
	"warpath/store"
)

func (e *Engine) wireEventHandlers() {
	// When an order is dispatched, track it in the poller
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderDispatchedEvent)
		// On redirect, the order may already have an old RDS order ID tracked in the poller.
		// Look up the order and untrack the old ID if it differs from the new one.
		if order, err := e.db.GetOrder(ev.OrderID); err == nil && order.RDSOrderID != "" && order.RDSOrderID != ev.RDSOrderID {
			e.poller.Untrack(order.RDSOrderID)
			e.logFn("engine: untracked old RDS order %s for order %d (redirect)", order.RDSOrderID, ev.OrderID)
		}
		e.poller.Track(ev.RDSOrderID)
		e.logFn("engine: tracking RDS order %s for order %d", ev.RDSOrderID, ev.OrderID)
	}, EventOrderDispatched)

	// When RDS reports a status change, update our order and notify WarDrop
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderStatusChangedEvent)
		e.handleRDSStatusChange(ev)
	}, EventOrderStatusChanged)

	// When an order fails, log it
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderFailedEvent)
		e.logFn("engine: order %d failed: %s - %s", ev.OrderID, ev.ErrorCode, ev.Detail)
		e.db.AppendAudit("order", ev.OrderID, "failed", "", ev.Detail, "system")
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
		e.logFn("engine: order %d received from %s: %s %s -> %s", ev.OrderID, ev.ClientID, ev.OrderType, ev.PayloadTypeCode, ev.DeliveryNode)
		e.db.AppendAudit("order", ev.OrderID, "received", "", fmt.Sprintf("%s %s from %s", ev.OrderType, ev.PayloadTypeCode, ev.ClientID), "system")
	}, EventOrderReceived)

	// Payload changes: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(PayloadChangedEvent)
		e.db.AppendAudit("payload", ev.PayloadID, ev.Action, "", fmt.Sprintf("type=%s node=%d", ev.PayloadTypeCode, ev.NodeID), "system")
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

func (e *Engine) handleRDSStatusChange(ev OrderStatusChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for status change: %v", ev.OrderID, err)
		return
	}

	// Update robot ID if we got one
	if ev.RobotID != "" && order.RobotID == "" {
		e.db.UpdateOrderRDS(order.ID, order.RDSOrderID, ev.NewStatus, ev.RobotID)

		// Send waybill to WarDrop
		reply := messaging.NewEnvelope("waybill", order.ClientID, e.cfg.FactoryID, messaging.WaybillReply{
			OrderUUID: order.WardropUUID,
			WaybillID: order.RDSOrderID,
			RobotID:   ev.RobotID,
		})
		data, _ := reply.Encode()
		topic := messaging.DispatchTopic(e.cfg.Messaging.DispatchTopicPrefix, order.ClientID)
		e.db.EnqueueOutbox(topic, data, "waybill", order.ClientID)
	}

	newStatus := e.mapRDSState(rds.OrderState(ev.NewStatus))
	if newStatus == order.Status {
		return
	}

	e.db.UpdateOrderStatus(order.ID, newStatus, fmt.Sprintf("RDS: %s -> %s", ev.OldStatus, ev.NewStatus))
	e.db.UpdateOrderRDS(order.ID, order.RDSOrderID, ev.NewStatus, ev.RobotID)

	// Send status update to WarDrop
	reply := messaging.NewEnvelope("update", order.ClientID, e.cfg.FactoryID, messaging.UpdateReply{
		OrderUUID: order.WardropUUID,
		Status:    newStatus,
		Detail:    fmt.Sprintf("RDS state: %s", ev.NewStatus),
	})
	data, _ := reply.Encode()
	topic := messaging.DispatchTopic(e.cfg.Messaging.DispatchTopicPrefix, order.ClientID)
	e.db.EnqueueOutbox(topic, data, "update", order.ClientID)

	// Handle terminal states
	switch rds.OrderState(ev.NewStatus) {
	case rds.StateFinished:
		e.handleOrderDelivered(order)
	case rds.StateFailed:
		e.db.UpdateOrderStatus(order.ID, dispatch.StatusFailed, "RDS order failed")
		e.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
			OrderID:     order.ID,
			WardropUUID: order.WardropUUID,
			ClientID:    order.ClientID,
			ErrorCode:   "rds_failed",
			Detail:      "RDS order failed",
		}})
	case rds.StateStopped:
		e.db.UpdateOrderStatus(order.ID, dispatch.StatusCancelled, "RDS order stopped")
	}
}

func (e *Engine) handleOrderDelivered(order *store.Order) {
	e.db.UpdateOrderStatus(order.ID, dispatch.StatusDelivered, "payload delivered")

	// Send delivered notification to WarDrop
	reply := messaging.NewEnvelope("delivered", order.ClientID, e.cfg.FactoryID, messaging.DeliveredReply{
		OrderUUID:   order.WardropUUID,
		DeliveredAt: time.Now().Format(time.RFC3339),
	})
	data, _ := reply.Encode()
	topic := messaging.DispatchTopic(e.cfg.Messaging.DispatchTopicPrefix, order.ClientID)
	e.db.EnqueueOutbox(topic, data, "delivered", order.ClientID)
}

// handleOrderCompleted moves payloads from source to dest after WarDrop confirms physical receipt.
func (e *Engine) handleOrderCompleted(ev OrderCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for completion: %v", ev.OrderID, err)
		return
	}

	if order.SourceNodeID == nil || order.DestNodeID == nil {
		return
	}

	payloads, _ := e.db.ListPayloadsByClaimedOrder(order.ID)
	for _, p := range payloads {
		e.nodeState.MovePayload(p.ID, *order.DestNodeID)
		e.Events.Emit(Event{Type: EventPayloadChanged, Payload: PayloadChangedEvent{
			Action:          "moved",
			PayloadID:       p.ID,
			PayloadTypeCode: p.PayloadTypeName,
			FromNodeID:      *order.SourceNodeID,
			ToNodeID:        *order.DestNodeID,
			NodeID:          *order.DestNodeID,
		}})
	}
}

func (e *Engine) mapRDSState(state rds.OrderState) string {
	switch state {
	case rds.StateCreated, rds.StateToBeDispatched:
		return dispatch.StatusDispatched
	case rds.StateRunning:
		return dispatch.StatusInTransit
	case rds.StateFinished:
		return dispatch.StatusDelivered
	case rds.StateFailed:
		return dispatch.StatusFailed
	case rds.StateStopped:
		return dispatch.StatusCancelled
	default:
		return dispatch.StatusDispatched
	}
}
