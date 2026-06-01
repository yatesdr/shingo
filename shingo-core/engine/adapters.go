package engine

import (
	"shingo/protocol"
	"shingocore/countgroup"
	"shingocore/fleet"
	"shingocore/store"
)

// dispatchEmitter bridges the dispatch package's emitter interface to the EventBus.
type dispatchEmitter struct {
	bus *EventBus
}

func (e *dispatchEmitter) EmitOrderReceived(orderID int64, edgeUUID, stationID string, orderType protocol.OrderType, payloadCode, deliveryNode string) {
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
		StationID: stationID,
		ErrorCode: errorCode,
		Detail:    detail,
	}})
}

func (e *dispatchEmitter) EmitOrderSkipped(orderID int64, edgeUUID, stationID, errorCode, detail string) {
	e.bus.Emit(Event{Type: EventOrderSkipped, Payload: OrderSkippedEvent{
		OrderID:   orderID,
		EdgeUUID:  edgeUUID,
		StationID: stationID,
		ErrorCode: errorCode,
		Detail:    detail,
	}})
}

func (e *dispatchEmitter) EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason, previousStatus string) {
	e.bus.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:        orderID,
		EdgeUUID:       edgeUUID,
		StationID:      stationID,
		Reason:         reason,
		PreviousStatus: previousStatus,
	}})
}

func (e *dispatchEmitter) EmitOrderCompleted(orderID int64, edgeUUID, stationID string) {
	e.bus.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:   orderID,
		EdgeUUID:  edgeUUID,
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

func (e *dispatchEmitter) EmitOrderFaulted(orderID int64, edgeUUID, stationID, reason string) {
	e.bus.Emit(Event{Type: EventOrderFaulted, Payload: OrderFaultedEvent{
		OrderID:   orderID,
		EdgeUUID:  edgeUUID,
		StationID: stationID,
		Reason:    reason,
	}})
}

func (e *dispatchEmitter) EmitOrderFaultedRecovered(orderID int64, edgeUUID, stationID, robotID string) {
	e.bus.Emit(Event{Type: EventOrderFaultedRecovered, Payload: OrderFaultedRecoveredEvent{
		OrderID:   orderID,
		EdgeUUID:  edgeUUID,
		StationID: stationID,
		RobotID:   robotID,
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

func (e *pollerEmitter) EmitGraceExpired(orderID int64, vendorOrderID string) {
	e.bus.Emit(Event{Type: EventGraceExpired, Payload: GraceExpiredEvent{
		OrderID:       orderID,
		VendorOrderID: vendorOrderID,
	}})
}

func (e *pollerEmitter) EmitBlockCompleted(orderID int64, vendorOrderID, blockID, location, binTask string) {
	e.bus.Emit(Event{Type: EventBlockCompleted, Payload: BlockCompletedEvent{
		OrderID:       orderID,
		VendorOrderID: vendorOrderID,
		BlockID:       blockID,
		Location:      location,
		BinTask:       binTask,
	}})
}

// countGroupEventEmitter bridges the countgroup package's Emitter interface to the EventBus.
type countGroupEventEmitter struct {
	bus *EventBus
}

func (e *countGroupEventEmitter) Emit(t countgroup.Transition) {
	e.bus.Emit(Event{
		Type: EventCountGroupTransition,
		Payload: CountGroupTransitionEvent{
			Group:             t.Group,
			Desired:           t.Desired,
			Robots:            t.Robots,
			FailSafeTriggered: t.FailSafeTriggered,
			Timestamp:         t.Timestamp,
		},
	})
}

// orderResolver implements fleet.OrderIDResolver â€” the tracker looks
// up the internal order ID for a vendor order ID when it emits a
// status-change event. Lives here because it's the same shape as the
// other fleet/countgroup adapters: a tiny struct wrapping a
// dependency with one method that satisfies an external interface.
type orderResolver struct {
	db *store.DB
}

func (r *orderResolver) ResolveVendorOrderID(vendorOrderID string) (int64, error) {
	order, err := r.db.GetOrderByVendorID(vendorOrderID)
	if err != nil {
		return 0, err
	}
	return order.ID, nil
}
