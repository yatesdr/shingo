package engine

import "shingocore/fleet"

// dispatchEmitter bridges the dispatch package's emitter interface to the EventBus.
type dispatchEmitter struct {
	bus *EventBus
}

func (e *dispatchEmitter) EmitOrderReceived(orderID int64, edgeUUID, stationID, orderType, payloadCode, deliveryNode string) {
	e.bus.Emit(Event{Type: EventOrderReceived, Payload: OrderReceivedEvent{
		OrderID:      orderID,
		EdgeUUID:     edgeUUID,
		StationID:    stationID,
		OrderType:    orderType,
		PayloadCode:  payloadCode,
		DeliveryNode: deliveryNode,
	}})
}

func (e *dispatchEmitter) EmitOrderDispatched(orderID int64, vendorOrderID, sourceNode, destNode string) {
	e.bus.Emit(Event{Type: EventOrderDispatched, Payload: OrderDispatchedEvent{
		OrderID:       orderID,
		VendorOrderID: vendorOrderID,
		SourceNode:    sourceNode,
		DestNode:      destNode,
	}})
}

func (e *dispatchEmitter) EmitOrderFailed(orderID int64, edgeUUID, stationID, errorCode, detail string) {
	e.bus.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID:   orderID,
		EdgeUUID:  edgeUUID,
		StationID:  stationID,
		ErrorCode: errorCode,
		Detail:    detail,
	}})
}

func (e *dispatchEmitter) EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason string) {
	e.bus.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:  orderID,
		EdgeUUID: edgeUUID,
		StationID: stationID,
		Reason:   reason,
	}})
}

func (e *dispatchEmitter) EmitOrderCompleted(orderID int64, edgeUUID, stationID string) {
	e.bus.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:  orderID,
		EdgeUUID: edgeUUID,
		StationID: stationID,
	}})
}

func (e *dispatchEmitter) EmitOrderQueued(orderID int64, edgeUUID, stationID, payloadCode string) {
	e.bus.Emit(Event{Type: EventOrderQueued, Payload: OrderQueuedEvent{
		OrderID:     orderID,
		EdgeUUID:    edgeUUID,
		StationID:   stationID,
		PayloadCode: payloadCode,
	}})
}

// pollerEmitter bridges the fleet tracker's status change events to the EventBus.
type pollerEmitter struct {
	bus *EventBus
}

func (e *pollerEmitter) EmitOrderStatusChanged(orderID int64, vendorOrderID, oldStatus, newStatus, robotID, detail string, snapshot *fleet.OrderSnapshot) {
	e.bus.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID:       orderID,
		VendorOrderID: vendorOrderID,
		OldStatus:     oldStatus,
		NewStatus:     newStatus,
		RobotID:       robotID,
		Detail:        detail,
		Snapshot:      snapshot,
	}})
}
