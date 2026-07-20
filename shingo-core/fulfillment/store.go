package fulfillment

import (
	"shingo/protocol"
	"shingocore/store"
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
// The set below is exactly what the scanner needs — no more, no less: the
// methods it calls directly on s.db, PLUS the CapacityDB set that
// CheckDropoffCapacity(s.db, …) requires (GetNodeByDotName, CountBinsByNode,
// CountInFlightOrdersByDeliveryNodeExcluding, ListChildNodes). After the
// SourceFinder collapse the finder owns source lookup and returns the bin's node,
// so the plant-wide finders and the node-by-id read left this interface.
//
// This interface is wider than material or dispatch/binresolver
// (fulfillment is orchestration, not pure compute) but the goal is
// the same: make the DB dependency explicit and make the scanner
// unit-testable in isolation.
type Store interface {
	// Order reads. ListAcquiringOrders is the scanner's scan set — orders in
	// {queued, sourcing} (the acquiring set, widened from queued-only).
	ListAcquiringOrders() ([]*orders.Order, error)
	GetOrder(id int64) (*orders.Order, error)
	// CapacityDB: the capacity gate self-excludes the caller's own order.
	CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode string, excludeID int64) (int, error)

	// Node reads (both are also part of the CapacityDB set).
	GetNodeByDotName(name string) (*nodes.Node, error)
	ListChildNodes(parentID int64) ([]*nodes.Node, error)

	// Bin reads (CapacityDB).
	CountBinsByNode(nodeID int64) (int, error)

	// Mutations performed on the order during fulfillment.
	//
	// ReleaseClaimByOrder is the coupled rollback (clears claimed_by AND releases
	// reservations); re-queue paths that abandon claims without going terminal
	// use it so a re-routed reserve-then-claim can't leak a confirmed reservation.
	ReleaseClaimByOrder(orderID int64) error
	UpdateOrderBinID(orderID, binID int64) error
	UpdateOrderSourceNode(id int64, sourceNode string) error
	// SetOrderQueueDetail records why an order is sitting queued — the generated
	// sentence, its structured queue code, and the engineer-only cause — together.
	// The code is typed so a caller cannot pass free text; the formatter is the
	// only producer of the sentence. Pass empty values to clear (on dispatch).
	SetOrderQueueDetail(id int64, reason string, code protocol.QueueCode, cause string) error
}

// Trimmed to this interface's "no more, no less" contract as the scanner's
// surface shrank:
//   - SourceFinder collapse: ClaimBin, UnclaimOrderBins, UpdateOrderStatus,
//     FailOrderAtomic — the scanner claims via Claimer.ClaimForDispatch, rolls
//     back via ReleaseClaimByOrder, transitions via Lifecycle, fails via failFn.
//   - 3-cleanup: FindSourceBinFIFO + FindEmptyCompatibleBin (the finder owns
//     source lookup now), GetNode (the finder returns the bin's node), and the
//     non-excluding CountInFlightOrdersByDeliveryNode (only the self-excluding
//     variant is used, by the capacity gate).

// Compile-time check that *store.DB satisfies Store. If the store
// package drops or renames one of the methods above, this assertion
// catches it before the build fails somewhere further downstream.
var _ Store = (*store.DB)(nil)
