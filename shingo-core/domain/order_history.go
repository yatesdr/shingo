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
	ID      int64           `json:"id"`
	OrderID int64           `json:"order_id"`
	Status  protocol.Status `json:"status"`
	Detail  string          `json:"detail"`

	// Code / Actor / Ref are the TYPED reason (migration 55). Empty on every
	// row written before it and on every uncoded transition, which is most of
	// them — uncoded is the honest value, not a gap to be filled in by
	// substring-matching Detail.
	//
	// Code is a protocol.TermCode on a terminal row and a protocol.QueueCode
	// on a queued one. Ref says what the reason concerns; at Springfield the
	// terminal codes are two values, so the reference is the actionable part.
	Code  string            `json:"code,omitempty"`
	Actor string            `json:"actor,omitempty"`
	Ref   *protocol.TermRef `json:"ref,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}
