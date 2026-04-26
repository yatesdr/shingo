package service

import (
	"fmt"

	"shingocore/fleet"
	"shingocore/store"
	"shingocore/store/orders"
)

// OrderService centralizes order-facing mutations that the www/ handlers
// used to drive directly against *store.DB. Audit logging and event
// emission stay at the handler layer — the service owns validation +
// the "call fleet and DB in the right order" composition.
//
// Stage 4 of the architecture plan introduces OrderService alongside
// NodeService as the follow-up to BinService. Queries (GetOrder,
// ListOrders, etc.) remain on the engine; this service is only for
// mutations and composite flows where more than one subsystem is
// touched.
type OrderService struct {
	db    *store.DB
	fleet fleet.Backend
}

func NewOrderService(db *store.DB, f fleet.Backend) *OrderService {
	return &OrderService{db: db, fleet: f}
}

// --- Creation -------------------------------------------------------------

// Create inserts a new order. Thin delegate kept here so handlers that
// already hold an *OrderService for other mutations don't have to plumb
// a second engine accessor through just to insert the row.
func (s *OrderService) Create(o *orders.Order) error {
	return s.db.CreateOrder(o)
}

// --- Status & vendor transitions -----------------------------------------

// UpdateStatus changes the order's status field and detail string.
func (s *OrderService) UpdateStatus(orderID int64, status, detail string) error {
	return s.db.UpdateOrderStatus(orderID, status, detail)
}

// UpdateVendor records vendor-side identifiers on the order (vendor
// order id, current vendor state, and the assigned robot id).
func (s *OrderService) UpdateVendor(orderID int64, vendorOrderID, vendorState, robotID string) error {
	return s.db.UpdateOrderVendor(orderID, vendorOrderID, vendorState, robotID)
}

// --- Priority -------------------------------------------------------------

// SetPriority updates priority on the fleet (if a vendor order exists)
// and then in the DB. The resolved order is returned so callers that
// need to log or emit events don't have to re-fetch it.
//
// Returns an "order not found" error (suitable for 404) when the order
// id does not exist. Fleet errors are returned unwrapped so the caller
// can map them to whichever HTTP status it prefers.
func (s *OrderService) SetPriority(orderID int64, priority int) (*orders.Order, error) {
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return nil, fmt.Errorf("order not found")
	}
	if order.VendorOrderID != "" {
		if err := s.fleet.SetOrderPriority(order.VendorOrderID, priority); err != nil {
			return order, err
		}
	}
	if err := s.db.UpdateOrderPriority(order.ID, priority); err != nil {
		return order, err
	}
	return order, nil
}

// --- Bin claims -----------------------------------------------------------

// ClaimBin reserves a bin for an order. Thin delegate centralized here
// so the spot-order submission flow does not have to reach into a
// separate bin accessor purely to attach a bin to the order it just
// created.
func (s *OrderService) ClaimBin(binID, orderID int64) error {
	return s.db.ClaimBin(binID, orderID)
}

// UnclaimBin releases an order's claim on a bin. Used by the spot-order
// rollback path when dispatch rejects the order after the claim was
// already written.
func (s *OrderService) UnclaimBin(binID int64) error {
	return s.db.UnclaimBin(binID)
}

// --- Queries --------------------------------------------------------------

// GetOrder loads an order by ID. Absorbed from engine_db_methods.go as
// part of the www-handler service migration (PR 3a.3a).
func (s *OrderService) GetOrder(id int64) (*orders.Order, error) {
	return s.db.GetOrder(id)
}

// GetOrderByUUID loads an order by its edge UUID. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.3a).
func (s *OrderService) GetOrderByUUID(uuid string) (*orders.Order, error) {
	return s.db.GetOrderByUUID(uuid)
}

// ListActiveOrders returns every order currently in an active (not
// terminal) status. Absorbed from engine_db_methods.go as part of the
// www-handler service migration (PR 3a.3a).
func (s *OrderService) ListActiveOrders() ([]*orders.Order, error) {
	return s.db.ListActiveOrders()
}

// ListOrders returns orders filtered by status (empty string = all),
// capped at limit rows. Absorbed from engine_db_methods.go as part of
// the www-handler service migration (PR 3a.3a).
func (s *OrderService) ListOrders(status string, limit int) ([]*orders.Order, error) {
	return s.db.ListOrders(status, limit)
}

// ListOrderHistory returns the historical status transitions for a
// single order. Absorbed from engine_db_methods.go as part of the
// www-handler service migration (PR 3a.3a).
func (s *OrderService) ListOrderHistory(orderID int64) ([]*orders.History, error) {
	return s.db.ListOrderHistory(orderID)
}

// ListChildOrders returns the sequenced child orders for a compound
// parent order. Absorbed from engine_db_methods.go as part of the
// www-handler service migration (PR 3a.3a).
func (s *OrderService) ListChildOrders(parentOrderID int64) ([]*orders.Order, error) {
	return s.db.ListChildOrders(parentOrderID)
}

// ListOrdersByStation returns the most recent orders originated by a
// specific station id, capped at limit rows. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.3b).
func (s *OrderService) ListOrdersByStation(stationID string, limit int) ([]*orders.Order, error) {
	return s.db.ListOrdersByStation(stationID, limit)
}

// ── PR 3a.6 additions: remaining www-reachable queries + terminals ───────

// ListActiveBySourceRef returns active (non-terminal) orders whose
// source_ref matches any of the supplied names. Used by the
// node-group-shutdown flow to find orders that still reference a
// group about to be torn down. Absorbed from engine_db_methods.go as
// part of the Phase 3a closeout (PR 3a.6).
func (s *OrderService) ListActiveBySourceRef(names []string) ([]*orders.Order, error) {
	return s.db.ListActiveOrdersBySourceRef(names)
}

// ListByBin returns the most recent orders that touched a single bin,
// capped at limit rows. Absorbed from engine_db_methods.go as part of
// the Phase 3a closeout (PR 3a.6).
func (s *OrderService) ListByBin(binID int64, limit int) ([]*orders.Order, error) {
	return s.db.ListOrdersByBin(binID, limit)
}

// FailAtomic transitions an order to "failed" and releases every bin
// claim in a single transaction. Absorbed from engine_db_methods.go
// as part of the Phase 3a closeout (PR 3a.6). Internal engine and
// dispatch flows still call *store.DB.FailOrderAtomic directly — this
// method is the handler-layer entry point only.
func (s *OrderService) FailAtomic(orderID int64, detail string) error {
	return s.db.FailOrderAtomic(orderID, detail)
}
