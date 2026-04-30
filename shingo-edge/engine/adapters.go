package engine

import "shingo/protocol"

// plcEmitter adapts the engine's EventBus to the plc.EventEmitter interface.
type plcEmitter struct {
	bus *EventBus
}

func (e *plcEmitter) EmitCounterRead(rpID int64, plcName, tagName string, value int64) {
	e.bus.Emit(Event{Type: EventCounterRead, Payload: CounterReadEvent{
		ReportingPointID: rpID, PLCName: plcName, TagName: tagName, Value: value,
	}})
}

func (e *plcEmitter) EmitCounterDelta(rpID, processID, styleID, delta, newCount int64, anomaly string) {
	e.bus.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ReportingPointID: rpID, ProcessID: processID, StyleID: styleID, Delta: delta, NewCount: newCount, Anomaly: anomaly,
	}})
}

func (e *plcEmitter) EmitCounterAnomaly(snapshotID, rpID int64, plcName, tagName string, oldVal, newVal int64, anomalyType string) {
	e.bus.Emit(Event{Type: EventCounterAnomaly, Payload: CounterAnomalyEvent{
		SnapshotID: snapshotID, ReportingPointID: rpID,
		PLCName: plcName, TagName: tagName,
		OldValue: oldVal, NewValue: newVal, AnomalyType: anomalyType,
	}})
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

func (e *orderEmitter) EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string) {
	e.bus.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID: orderID, OrderUUID: orderUUID, OrderType: orderType, Reason: reason,
	}})
}
