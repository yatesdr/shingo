package engine

import (
	"shingo/protocol"
	"shingoedge/plc"
)

// plcEmitter adapts the engine's EventBus to the plc.EventEmitter interface.
// eng answers AttributeTick; nil attributes every tick to its point's style.
type plcEmitter struct {
	bus *EventBus
	eng *Engine
}

func (e *plcEmitter) EmitCounterRead(rpID int64, plcName, tagName string, value int64) {
	e.bus.Emit(Event{Type: EventCounterRead, Payload: CounterReadEvent{
		ReportingPointID: rpID, PLCName: plcName, TagName: tagName, Value: value,
	}})
}

func (e *plcEmitter) AttributeTick(processID, pointStyleID int64) plc.TickIdentity {
	if e.eng == nil {
		return plc.TickIdentity{StyleID: pointStyleID, Kind: plc.TickToday}
	}
	return e.eng.attributeTick(processID, pointStyleID)
}

func (e *plcEmitter) EmitCounterDelta(rpID, processID, delta, newCount int64, anomaly string, id plc.TickIdentity) {
	e.bus.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ReportingPointID: rpID, ProcessID: processID, StyleID: id.StyleID, Delta: delta, NewCount: newCount, Anomaly: anomaly,
		CATID: id.CATID, Kind: id.Kind,
	}})
	if id.Kind == plc.TickTarget && e.eng != nil {
		e.eng.noteTargetTick(processID, id.CATID)
	}
}

func (e *plcEmitter) EmitPLCConnected(plcName string) {
	e.bus.Emit(Event{Type: EventPLCConnected, Payload: PLCEvent{PLCName: plcName}})
}

func (e *plcEmitter) EmitPLCDisconnected(plcName string, err error) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	e.bus.Emit(Event{Type: EventPLCDisconnected, Payload: PLCEvent{PLCName: plcName, Error: errStr}})
}

func (e *plcEmitter) EmitPLCHealthAlert(plcName string, errMsg string) {
	e.bus.Emit(Event{Type: EventPLCHealthAlert, Payload: PLCHealthAlertEvent{PLCName: plcName, Error: errMsg}})
}

func (e *plcEmitter) EmitPLCHealthRecover(plcName string) {
	e.bus.Emit(Event{Type: EventPLCHealthRecover, Payload: PLCHealthRecoverEvent{PLCName: plcName}})
}

func (e *plcEmitter) EmitCounterReadError(rpID int64, plcName, tagName, errMsg string) {
	e.bus.Emit(Event{Type: EventCounterReadError, Payload: CounterReadErrorEvent{
		ReportingPointID: rpID, PLCName: plcName, TagName: tagName, Error: errMsg,
	}})
}

func (e *plcEmitter) EmitWarLinkConnected() {
	e.bus.Emit(Event{Type: EventWarLinkConnected, Payload: WarLinkEvent{Connected: true}})
}

func (e *plcEmitter) EmitWarLinkDisconnected(err error) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	e.bus.Emit(Event{Type: EventWarLinkDisconnected, Payload: WarLinkEvent{Connected: false, Error: errStr}})
}

// orderEmitter adapts the engine's EventBus to the orders.EventEmitter interface.
type orderEmitter struct {
	bus *EventBus
}

func (e *orderEmitter) EmitOrderCreated(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
	e.bus.Emit(Event{Type: EventOrderCreated, Payload: OrderCreatedEvent{
		OrderID: orderID, OrderUUID: orderUUID, OrderType: orderType, ProcessNodeID: processNodeID,
	}})
}

func (e *orderEmitter) EmitOrderStatusChanged(orderID int64, orderUUID string, orderType protocol.OrderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64) {
	e.bus.Emit(Event{Type: EventOrderStatusChanged, Payload: OrderStatusChangedEvent{
		OrderID: orderID, OrderUUID: orderUUID, OrderType: orderType, OldStatus: oldStatus, NewStatus: newStatus, ETA: eta, ProcessNodeID: processNodeID,
	}})
}

func (e *orderEmitter) EmitOrderCompleted(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
	e.bus.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID: orderID, OrderUUID: orderUUID, OrderType: orderType, ProcessNodeID: processNodeID,
	}})
}

func (e *orderEmitter) EmitOrderDelivered(orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID, binID *int64, binUOP *int, binPayloadCode *string, binEpoch int64, binDestNode, deliveryNode string, landingSent bool) {
	e.bus.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID: orderID, OrderUUID: orderUUID, OrderType: orderType, ProcessNodeID: processNodeID, BinID: binID, BinUOP: binUOP, BinPayloadCode: binPayloadCode, BinEpoch: binEpoch, BinDestNode: binDestNode, DeliveryNode: deliveryNode,
		LandingSent: landingSent,
	}})
}

func (e *orderEmitter) EmitOrderDeliveredFallback(binID int64, binUOP *int, binPayloadCode *string, binEpoch int64, deliveryNode string, landingSent bool) {
	e.bus.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		BinID: &binID, BinUOP: binUOP, BinPayloadCode: binPayloadCode, BinEpoch: binEpoch, DeliveryNode: deliveryNode,
		LandingSent: landingSent,
	}})
}

func (e *orderEmitter) EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string) {
	e.bus.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID: orderID, OrderUUID: orderUUID, OrderType: orderType, Reason: reason,
	}})
}

// EmitOrderTouched: the order handler's last write landed (orders.Manager
// AnnounceOrder); the nudge names the order, so only its boards rebuild.
func (e *orderEmitter) EmitOrderTouched(orderID int64, orderUUID string, processNodeID *int64) {
	e.bus.Emit(Event{Type: EventBoardNudge, Payload: BoardNudgeEvent{
		OrderID: orderID, OrderUUID: orderUUID, ProcessNodeID: processNodeID,
	}})
}

// EmitCurtainChanged: a curtain tag a station view reads changed its reading
// in the PLC cache (plc.Manager WatchCurtainTag).
func (e *plcEmitter) EmitCurtainChanged() { e.bus.Emit(Event{Type: EventBoardNudge}) }

// nudgeBoards says a station view's input changed with no scoped event.
func (e *Engine) nudgeBoards() { e.Events.Emit(Event{Type: EventBoardNudge}) }

func (e *orderEmitter) EmitOrderCancelledByCore(orderID int64, orderUUID string, orderType protocol.OrderType, reason string) {
	e.bus.Emit(Event{Type: EventOrderCancelledByCore, Payload: OrderCancelledByCoreEvent{
		OrderID: orderID, OrderUUID: orderUUID, OrderType: orderType, Reason: reason,
	}})
}

func (e *orderEmitter) EmitOrderFaulted(orderID int64, orderUUID, reason string) {
	e.bus.Emit(Event{Type: EventOrderFaulted, Payload: OrderFaultedEvent{
		OrderID: orderID, OrderUUID: orderUUID, Reason: reason,
	}})
}
