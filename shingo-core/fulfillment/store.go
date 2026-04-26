package fulfillment

import (
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Store is the narrow DB surface the fulfillment scanner depends on.
//
// Declaring it consumer-side does two things:
//
//  1. *store.DB satisfies it for free (Go interface satisfaction is
//     structural), so engine wiring does not change.
//  2. Tests can drop a hand-rolled fake into the scanner and
//     exercise queue-to-dispatch behaviour without a database.
//
// The set below is exactly the methods the scanner calls on the
// store — no more, no less. A lint of `grep 's\.db\.' scanner.go`
// should match one-to-one with the entries here.
//
// This interface is wider than material or dispatch/binresolver
// (fulfillment is orchestration, not pure compute) but the goal is
// the same: make the DB dependency explicit and make the scanner
// unit-testable in isolation.
type Store interface {
	// Order reads.
	ListQueuedOrders() ([]*orders.Order, error)
	GetOrder(id int64) (*orders.Order, error)
	CountInFlightOrdersByDeliveryNode(deliveryNode string) (int, error)

	// Node reads.
	GetNode(id int64) (*nodes.Node, error)
	GetNodeByDotName(name string) (*nodes.Node, error)

	// Bin reads.
	CountBinsByNode(nodeID int64) (int, error)
	FindEmptyCompatibleBin(payloadCode, preferZone string) (*bins.Bin, error)
	FindSourceBinFIFO(payloadCode string) (*bins.Bin, error)

	// Mutations performed on the bin/order during fulfillment.
	ClaimBin(binID, orderID int64) error
	UnclaimOrderBins(orderID int64)
	UpdateOrderBinID(orderID, binID int64) error
	UpdateOrderSourceNode(id int64, sourceNode string) error
	UpdateOrderStatus(id int64, status, detail string) error

	// Structural-error fallback path (see scanner.go: only used when
	// failFn is nil — older tests construct the scanner without it).
	FailOrderAtomic(orderID int64, detail string) error
}

// Compile-time check that *store.DB satisfies Store. If the store
// package drops or renames one of the methods above, this assertion
// catches it before the build fails somewhere further downstream.
var _ Store = (*store.DB)(nil)
