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
	//
	// fromNotice says an order.delivered is behind the event (the notice's
	// handler sets it; false for a version-0 reconcile snapshot).
	EmitOrderDelivered(orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID, binID *int64, binDestNode, deliveryNode string, fromNotice bool)
	// EmitOrderDeliveredFallback is the order.delivered of a Core-admin order
	// with no Edge order row (HandleDeliveredNotice did not find the UUID). The
	// engine resolves the process node from deliveryNode for the check. The
	// carrier's identity comes with its landing.
	EmitOrderDeliveredFallback(binID int64, deliveryNode string)
	// EmitOrderFailed says the order failed: code is the code Core failed it
	// with (protocol.OrderUpdate.ErrorCode, from Core's versioned tell), "" when
	// Core sent none or the failure is not Core's tell; reason is the history
	// row's sentence.
	EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, code, reason string)
	// EmitOrderCancelledByCore says Core cancelled an order that was still live
	// here, with Core's reason (Manager.HandleDispatchReply). Not emitted for a
	// cancel this station sent: that order is terminal before Core's echo lands.
	EmitOrderCancelledByCore(orderID int64, orderUUID string, orderType protocol.OrderType, reason string)
	EmitOrderFaulted(orderID int64, orderUUID, reason string)
	// EmitOrderTouched says an order's fields were written and no scoped event
	// announced them (Manager.AnnounceOrder): the boards that draw the order
	// or its process node refetch.
	EmitOrderTouched(orderID int64, orderUUID string, processNodeID *int64)
}
