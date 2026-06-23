package orders

import "shingo/protocol"

// EventEmitter is the interface the orders package uses to emit events.
type EventEmitter interface {
	EmitOrderCreated(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64)
	EmitOrderStatusChanged(orderID int64, orderUUID string, orderType protocol.OrderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64)
	EmitOrderCompleted(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64)
	EmitOrderDelivered(orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID, binID *int64, binUOP *int, binEpoch int64)
	// EmitOrderDeliveredFallback binds the runtime cache for Core-admin orders
	// that have no Edge order row. ProcessNodeID is resolved from deliveryNode by
	// the engine handler. Called when HandleDeliveredWithExpiry can't find the UUID.
	EmitOrderDeliveredFallback(binID int64, binUOP *int, binEpoch int64, deliveryNode string)
	EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string)
	EmitOrderFaulted(orderID int64, orderUUID, reason string)
}
