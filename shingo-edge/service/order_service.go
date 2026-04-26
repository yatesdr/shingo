package service

import (
	"shingoedge/store"
	"shingoedge/store/orders"
)

// OrderService exposes the order-query surface used by handlers. The
// full order lifecycle (create, dispatch, complete, abort) lives on
// orders.Manager (`shingoedge/orders`); this service intentionally
// covers ONLY the read paths and the one operator-driven mutation
// (final count confirmation) that handlers reach through the engine.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
type OrderService struct {
	db *store.DB
}

// NewOrderService constructs an OrderService wrapping the shared
// *store.DB.
func NewOrderService(db *store.DB) *OrderService {
	return &OrderService{db: db}
}

// Get returns one order by id.
func (s *OrderService) Get(id int64) (*orders.Order, error) {
	return s.db.GetOrder(id)
}

// ListActive returns every order in a non-terminal state across all
// processes.
func (s *OrderService) ListActive() ([]orders.Order, error) {
	return s.db.ListActiveOrders()
}

// ListActiveByProcess returns active orders scoped to one process.
func (s *OrderService) ListActiveByProcess(processID int64) ([]orders.Order, error) {
	return s.db.ListActiveOrdersByProcess(processID)
}

// UpdateFinalCount writes the final_count + count_confirmed fields
// on an order. Used at operator final-count confirmation time after
// material delivery.
func (s *OrderService) UpdateFinalCount(id int64, finalCount int64, confirmed bool) error {
	return s.db.UpdateOrderFinalCount(id, finalCount, confirmed)
}
