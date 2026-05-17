// Package testutil provides test helpers for code that depends on the
// shingo-edge orders package. The single inhabitant is NoOpOrderEmitter,
// a stub implementation of orders.EventEmitter for tests that need to
// wire up an OrderManager but don't care about emitted events.
//
// Lives here rather than shingo/protocol/testutil so we can import
// protocol.OrderType without creating an import cycle with the
// protocol-package internal tests.
package testutil

import "shingo/protocol"

// NoOpOrderEmitter satisfies shingoedge/orders.EventEmitter with empty
// method bodies. Tests pass it to orders.NewManager when they don't
// care about emitted events. The interface is duck-typed; if its
// signature changes, the compile error surfaces at the call site —
// no silent skip.
type NoOpOrderEmitter struct{}

func (NoOpOrderEmitter) EmitOrderCreated(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
}

func (NoOpOrderEmitter) EmitOrderStatusChanged(orderID int64, orderUUID string, orderType protocol.OrderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64) {
}

func (NoOpOrderEmitter) EmitOrderCompleted(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
}

func (NoOpOrderEmitter) EmitOrderDelivered(orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID, binID *int64) {
}

func (NoOpOrderEmitter) EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string) {
}

func (NoOpOrderEmitter) EmitOrderFaulted(orderID int64, orderUUID, reason string) {}
