package domain

import (
	"time"

	"shingo/protocol"
)

// OrderHistory is one row in the order_history table — a status-change
// audit trail for a single Order. Every transition through the
// lifecycle (pending → queued → dispatched → …) appends a row here so
// callers can render a timeline.
//
// Stage 2A.2 lifted this struct into domain/ so handlers and services
// can return order-with-history shapes without importing
// shingo-core/store/orders. The store/orders package re-exports it
// via `type History = domain.OrderHistory`.
type OrderHistory struct {
	ID        int64     `json:"id"`
	OrderID   int64           `json:"order_id"`
	Status    protocol.Status `json:"status"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}
