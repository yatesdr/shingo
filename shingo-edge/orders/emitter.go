package orders

// EventEmitter is the interface the orders package uses to emit events.
type EventEmitter interface {
	EmitOrderCreated(orderID int64, orderUUID, orderType string, payloadID, opNodeID *int64)
	EmitOrderStatusChanged(orderID int64, orderUUID, orderType, oldStatus, newStatus, eta string, payloadID, opNodeID *int64)
	EmitOrderCompleted(orderID int64, orderUUID, orderType string, payloadID, opNodeID *int64)
	EmitOrderFailed(orderID int64, orderUUID, orderType, reason string)
}
