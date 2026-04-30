package orders

import "shingo/protocol"

// EventEmitter is the interface the orders package uses to emit events.
type EventEmitter interface {
	EmitOrderCreated(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64)
	EmitOrderStatusChanged(orderID int64, orderUUID string, orderType protocol.OrderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64)
	EmitOrderCompleted(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64)
	EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string)
}
