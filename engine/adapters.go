package engine

// dispatchEmitter bridges the dispatch package's emitter interface to the EventBus.
type dispatchEmitter struct {
	bus *EventBus
}

func (e *dispatchEmitter) EmitOrderReceived(orderID int64, wardropUUID, clientID, orderType, materialCode, deliveryNode string) {
	e.bus.Emit(Event{Type: EventOrderReceived, Payload: OrderReceivedEvent{
		OrderID:      orderID,
		WardropUUID:  wardropUUID,
		ClientID:     clientID,
		OrderType:    orderType,
		MaterialCode: materialCode,
		DeliveryNode: deliveryNode,
	}})
}

func (e *dispatchEmitter) EmitOrderDispatched(orderID int64, rdsOrderID, sourceNode, destNode string) {
	e.bus.Emit(Event{Type: EventOrderDispatched, Payload: OrderDispatchedEvent{
		OrderID:    orderID,
		RDSOrderID: rdsOrderID,
		SourceNode: sourceNode,
		DestNode:   destNode,
	}})
}

func (e *dispatchEmitter) EmitOrderFailed(orderID int64, wardropUUID, clientID, errorCode, detail string) {
	e.bus.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID:     orderID,
		WardropUUID: wardropUUID,
		ClientID:    clientID,
		ErrorCode:   errorCode,
		Detail:      detail,
	}})
}

func (e *dispatchEmitter) EmitOrderCancelled(orderID int64, wardropUUID, clientID, reason string) {
	e.bus.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:     orderID,
		WardropUUID: wardropUUID,
		ClientID:    clientID,
		Reason:      reason,
	}})
}

func (e *dispatchEmitter) EmitOrderCompleted(orderID int64, wardropUUID, clientID string) {
	e.bus.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:     orderID,
		WardropUUID: wardropUUID,
		ClientID:    clientID,
	}})
}

// pollerEmitter bridges the RDS poller's status change events to the EventBus.
type pollerEmitter struct {
	bus *EventBus
}

func (e *pollerEmitter) EmitOrderStatusChanged(orderID int64, rdsOrderID, oldStatus, newStatus, robotID, detail string) {
	e.bus.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID:    orderID,
		RDSOrderID: rdsOrderID,
		OldStatus:  oldStatus,
		NewStatus:  newStatus,
		RobotID:    robotID,
		Detail:     detail,
	}})
}
