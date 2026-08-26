package engine

import (
	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
)

// dispatchEmitter bridges the dispatch package's emitter interface to the EventBus.
//
// engine is set only for ProjectOrder, which is the one method that goes on the
// wire rather than onto the bus. Nil in the unit tests that exercise the bus
// methods, and ProjectOrder tolerates that rather than requiring every such test
// to build an engine.
type dispatchEmitter struct {
	bus    *EventBus
	engine *Engine
}

// ProjectOrder pushes an order row down to the station that owns it.
//
// BEST EFFORT, and the failure path is the design rather than a shortcut. A
// projection that does not reach the Edge means the order is missing from that
// board until the next order status reconcile, which carries the same row back
// and heals it. Failing the ORDER because its label did not send would be a far
// worse trade: the transport work is what matters and it is already admitted by
// the time this runs.
func (e *dispatchEmitter) ProjectOrder(stationID string, projection protocol.OrderProjection) {
	if e.engine == nil || stationID == "" || projection.OrderUUID == "" {
		return
	}
	if err := e.engine.SendDataToEdge(protocol.SubjectOrderProjected, stationID, &projection); err != nil {
		e.engine.logFn("order projection for %s to station %s not sent: %v — the order stands; the status reconcile will carry it",
			projection.OrderUUID, stationID, err)
	}
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

func (e *dispatchEmitter) EmitOrderResumed(orderID int64, edgeUUID, stationID string) {
	e.bus.Emit(Event{Type: EventOrderResumed, Payload: OrderResumedEvent{
		OrderID:   orderID,
		EdgeUUID:  edgeUUID,
		StationID: stationID,
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

func (e *pollerEmitter) EmitBlockCompleted(orderID int64, vendorOrderID, blockID, location, binTask string, startTime, terminateTime int64) {
	e.bus.Emit(Event{Type: EventBlockCompleted, Payload: BlockCompletedEvent{
		OrderID:       orderID,
		VendorOrderID: vendorOrderID,
		BlockID:       blockID,
		Location:      location,
		BinTask:       binTask,
		StartTime:     startTime,
		TerminateTime: terminateTime,
	}})
}

// orderResolver implements fleet.OrderIDResolver — the tracker looks
// up the internal order ID for a vendor order ID when it emits a
// status-change event. Lives here because it's the same shape as the
// other fleet adapters: a tiny struct wrapping a
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
