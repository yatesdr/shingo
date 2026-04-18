package service

import (
	"fmt"

	"shingocore/fleet"
	"shingocore/store"
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
func (s *OrderService) Create(o *store.Order) error {
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
func (s *OrderService) SetPriority(orderID int64, priority int) (*store.Order, error) {
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
