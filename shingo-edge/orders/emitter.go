package orders

import "shingo/protocol"

// EventEmitter is the interface the orders package uses to emit events.
type EventEmitter interface {
	EmitOrderCreated(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64)
	EmitOrderStatusChanged(orderID int64, orderUUID string, orderType protocol.OrderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64)
	EmitOrderCompleted(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64)
	// EmitOrderDelivered carries deliveryNode alongside processNodeID because an
	// Edge order row may legitimately have NO process node — a manual move order
	// is created against a Core node name, not an Edge process node. Without the
	// name the engine handler has no way left to resolve the destination, and the
	// delivery binds nothing (HK 2026-07-28: two move orders landed carriers on
	// PLN_01/PLN_04, bound neither, and the presses counted into pending_uop_delta
	// with no bin). Pass order.DeliveryNode; the handler prefers processNodeID and
	// falls back to the name.
	EmitOrderDelivered(orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID, binID *int64, binUOP *int, binEpoch int64, binDestNode, deliveryNode string)
	// EmitOrderDeliveredFallback binds the runtime cache for Core-admin orders
	// that have no Edge order row. ProcessNodeID is resolved from deliveryNode by
	// the engine handler. Called when HandleDeliveredWithExpiry can't find the UUID.
	EmitOrderDeliveredFallback(binID int64, binUOP *int, binEpoch int64, deliveryNode string)
	EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string)
	EmitOrderFaulted(orderID int64, orderUUID, reason string)
}
