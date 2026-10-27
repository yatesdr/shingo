package fulfillment

import (
	"shingo/protocol"
	"shingocore/dispatch/plan"
	"shingocore/domain"
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
// methods it calls directly on s.db, and the capacity gate's read set, embedded
// (plan.CapacityDB), because the scanner hands s.db whole to
// dispatch.CheckDropoffCapacity. The gate's reads are declared once, there, with
// the reason each is read; of them the scanner itself calls only
// GetNodeByDotName. After the SourceFinder collapse the finder owns source
// lookup and returns the bin's node, so the plant-wide finders and the
// node-by-id read left this interface.
//
// This interface is wider than material or dispatch/binresolver
// (fulfillment is orchestration, not pure compute) but the goal is
// the same: make the DB dependency explicit and make the scanner
// unit-testable in isolation.
type Store interface {
	plan.CapacityDB

	// Order reads. ListAcquiringOrders is the scanner's scan set — top-level
	// orders in {pending, queued} (the acquiring set).
	ListAcquiringOrders() ([]*orders.Order, error)
	GetOrder(id int64) (*orders.Order, error)
	// GetOrderByUUID finds a pair leg's partner (orders.sibling_order_uuid).
	GetOrderByUUID(uuid string) (*orders.Order, error)
	// PlannedPickStands is how acquiring reads a birth pick (orders.plan): the
	// planned bin's node while the pick stands for this asker, nil when it is
	// stale and the scanner picks again (SYNTH F4).
	PlannedPickStands(p *domain.PlannedPick, asker domain.DigAsker) (*nodes.Node, error)
	// OwnsNoCargo distinguishes a COORDINATOR (owns legs, NULL bin_id
	// permanently and correctly) from a defective single-bin order. Shadowed
	// at dispatchHeldBin for one window before the spelling is cut over.
	OrderOwnsNoCargo(orderID int64) (bool, error)
	// GetNodePropertyByName is how the capacity gate's park asks whether the
	// group it parked an order on names an overflow destination
	// (nodes.PropOverflowDestination), one read before any resolve
	// (overflowsWhileParked).
	GetNodePropertyByName(name, key string) string

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
