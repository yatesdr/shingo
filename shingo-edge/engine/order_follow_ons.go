package engine

import (
	"shingo/protocol"
)

// order_follow_ons.go — the durable follow-ons of an order whose status is
// Core's versioned word, on the messages that carry their causes.
//
// NOTHING DURABLE HANGS ON THE STATUS EVENT for such an order. Its status is
// applied once, newer only (orders/core_word.go), but a status is a fact about
// where the order is, and the follow-ons are about what just happened to it: a
// delivery makes the held pulling leg's order, a cancel at a station wait sends
// the carrier it left there back. Each hangs on Core's message for that event,
// which arrives once per event. An older Core's order keeps them on the status
// event (wiring.go), and each subscriber there declines a versioned order.

// onDeliveredMessage is Core's order.delivered for a versioned order: the
// partner's delivery that a held sequential pulling leg waits on.
func (e *Engine) onDeliveredMessage(orderID int64, processNodeID *int64) {
	e.heldLegAfterDelivery(orderID, processNodeID, "delivered")
}

// onCancelledMessage is Core's order.cancelled for a versioned order. previous
// is the status Core held when it cancelled (protocol.OrderCancelled.
// PreviousStatus): a leg cancelled while staged at the station wait after its
// drop at staging left its carrier there, and it goes back. The cancelled
// reply to this station's own cancel request carries none, and the
// cancellation notice that follows it does, so one cancel sends one carrier.
func (e *Engine) onCancelledMessage(orderID int64, previous protocol.Status) {
	if previous != protocol.StatusStaged {
		return
	}
	if p, ok := e.carrierLeftWaitingBy(orderID, true); ok {
		e.loops.spawn(func() { e.sendCarrierBack(p) })
	}
}

// statusIsCoresWord reports whether an order's status is Core's versioned word
// (orders.core_version > 0): its follow-ons hang on Core's messages, not on the
// status event. One read, asked only after the event's own status filter. An
// unreadable order is not one, which keeps the older behaviour.
func (e *Engine) statusIsCoresWord(orderID int64) bool {
	o, err := e.db.GetOrder(orderID)
	return err == nil && o != nil && o.CoreVersion > 0
}
