package fulfillment

import (
	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
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
	// OwnsNoCargo distinguishes a COORDINATOR (owns legs, NULL bin_id
	// permanently and correctly) from a defective single-bin order. Shadowed
	// at dispatchHeldBin for one window before the spelling is cut over.
	OrderOwnsNoCargo(orderID int64) (bool, error)
	// CapacityDB: the capacity gate self-excludes the caller's own order.
	CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode string, excludeID int64) (int, error)

	// Node reads (both are also part of the CapacityDB set).
	GetNodeByDotName(name string) (*nodes.Node, error)
	ListChildNodes(parentID int64) ([]*nodes.Node, error)
	// The two maintained-group level reads. Here because the scanner hands this
	// interface to dispatch.CheckDropoffCapacity, which consults the level so the
	// gate and the resolver cannot disagree about whether a group can take a
	// carrier (MG4-3). The scanner itself reads neither.
	ListMaintainLevels(groupNodeID int64) ([]nodes.MaintainLevel, error)
	CountEmptyBinsOfTypeInGroup(binTypeCode string, groupNodeID int64) (int, error)

	// Bin reads (CapacityDB).
	CountBinsByNode(nodeID int64) (int, error)

	// Mutations performed on the order during fulfillment.
	//
	// ReleaseReservation releases the order's SOFT pending bin reservation only —
	// the rollback for a bin soft-acquired but not yet hard-claimed at dispatch
	// (Rule 1: soft until complete). No claimed_by column is touched because none
	// was written yet.
	ReleaseReservation(orderID, binID int64) error
	UpdateOrderBinID(orderID, binID int64) error
	// ClearOrderBinID forgets a stale held-bin pointer so the order re-finds.
	ClearOrderBinID(orderID int64) error
	// ListReservationsByOrder is how the held-bin arm asks whether the soft hold
	// it is retrying against still exists. Release DELETES the row, so any row
	// returned here is a live hold.
	ListReservationsByOrder(orderID int64) ([]reservations.Reservation, error)
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
//     FailOrderAtomic — the scanner claims via the Claimer (soft reserve then
//     confirm at dispatch), rolls back via the dispatcher's fleet-refusal door
//     (hard — armor off, paper demoted) or ReleaseReservation (soft),
//     transitions via Lifecycle, fails via failFn.
//   - 3-cleanup: FindSourceBinFIFO + FindEmptyCompatibleBin (the finder owns
//     source lookup now), GetNode (the finder returns the bin's node), and the
//     non-excluding in-flight count (only the self-excluding
//     variant is used, by the capacity gate).

// Compile-time check that *store.DB satisfies Store. If the store
// package drops or renames one of the methods above, this assertion
// catches it before the build fails somewhere further downstream.
var _ Store = (*store.DB)(nil)
