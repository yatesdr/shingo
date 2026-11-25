package engine

import (
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/store/orders"
)

// ErrDestinationOccupied is returned when a move is refused because something
// is already at the destination.
//
// It is a sentinel rather than a plain error because the caller has to tell it
// apart: this is a conflict with the plant's current state, which the operator
// can resolve by clearing the spot or picking another one, and it deserves a
// 409 rather than the 500 every other failure on this path gets. Wrapped so the
// rendered sentence travels with it.
var ErrDestinationOccupied = errors.New("destination occupied")

// ErrBinTaken is returned when another order acquired the bin in the moment
// between choosing it and reserving it.
//
// A sentinel for the same reason as the one above: the caller has to tell it
// apart from a real fault. This one used to be tagged in the error TEXT — the
// words "transient reservation conflict, retry" were spliced into the message —
// which the comment beside it described as tagging it "so the caller can retry
// rather than surface a hard 500". Nothing could read a phrase in a string, so
// the caller never did, and it surfaced as a hard 500 with an instruction buried
// inside it. Now it is a value.
var ErrBinTaken = errors.New("bin taken by another order")

// ErrNodeNotFound is returned when the request names a node that does not
// exist.
//
// A sentinel for the same reason as the two above: without one, the caller
// turns every unrecognised error into a 500, so an engineer who mistypes a node
// is told the server is broken. The operator's door has always answered 400 for
// exactly this — the two doors disagreed about what kind of failure a typo is.
var ErrNodeNotFound = errors.New("node not found")

// HardReleaseOrder advances a dwelling order past its wait regardless of who
// owns that wait — the Core operator's escape hatch (W3).
//
// It is the engine's thin door onto Dispatcher.HardReleaseStagedOrder, which is
// where the reasoning and the audit live. Same shape and same protected route
// group as TerminateOrder: an engineer has decided, and the row records who.
func (e *Engine) HardReleaseOrder(orderID int64, actor string) error {
	return e.dispatcher.HardReleaseStagedOrder(orderID, actor)
}

// OrderFinishedError is TerminateOrder's refusal of an order with nothing left
// to stop: it has ended (Status is terminal), or its cart is set down where
// the order takes it (SetDown: delivered, or landed and still in_transit,
// store.DB.OrderTrips). It is a refusal, not a fault, so a door answers 409
// with its sentence (www apiTerminateOrder; pinned by
// TestPinOrders_TerminateFailureAnswers and
// TestPinOrders_TerminateRefusesAnOrderWhoseCartIsSetDown).
type OrderFinishedError struct {
	Status  protocol.Status
	SetDown bool
}

func (e *OrderFinishedError) Error() string {
	if e.SetDown {
		return "cannot cancel this order: its cart is already set down"
	}
	return fmt.Sprintf("cannot cancel this order: it has already ended (%s)", e.Status)
}

// TerminateOrder cancels an order, unclaims any payloads, and emits a cancellation event.
func (e *Engine) TerminateOrder(orderID int64, actor string) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}

	// Refuse an ended order, and one whose cart is set down: there is nothing
	// left to stop, and nothing is written. Set down is asked of where the cart
	// is, not the status: the landing puts the cart down and releases its claim
	// before the delivered write, so a landed order still reads in_transit. A
	// cart a robot is carrying is still cancelled. One trip read, only for a
	// live order that is not delivered and carries a cart.
	if protocol.IsTerminal(order.Status) {
		return &OrderFinishedError{Status: order.Status}
	}
	trips, err := e.db.OrderTrips([]*orders.Order{order})
	if err != nil {
		return fmt.Errorf("order %d: %w", orderID, err)
	}
	if trips[order.ID] == domain.TripSetDown {
		return &OrderFinishedError{Status: order.Status, SetDown: true}
	}

	// ── THE UI DOOR CASCADES NOW, LIKE THE WIRE DOOR ─────────────────────────
	//
	// This was a bare Lifecycle().CancelOrder. It transitions the order and emits,
	// and that is ALL it does — so cancelling a compound parent from the
	// operations page left its children RUNNING: live vendor orders driving
	// robots, bins still claimed, and a lane still held by a parent that no
	// longer exists. The wire door (HandleOrderCancel) has cascaded
	// unconditionally since a Queued parent was found orphaning its legs; the UI
	// door was never given the same treatment, and the two are indistinguishable
	// from the operator's side — the same button, in their mind, on a different
	// page.
	//
	// CancelOrderWithCascade is the shared door: lane snapshot first, parent
	// cancel, then the children, then the lane release and the wake. It is
	// idempotent for a plain order — ListChildOrders returns nothing and the loop
	// no-ops — so routing every cancel through it costs one SELECT and removes a
	// whole class of "why is this robot still moving".
	//
	// THE ACTOR IS RECORDED AS THE ACTOR, not baked into the prose and dropped.
	// It was being formatted into "cancelled by X" and then overwritten with
	// "system:<station>" one frame down, so order_history said the station
	// cancelled every order a person cancelled — on this door and the
	// diagnostics CancelStuckOrder door above it. The prose is kept as it was:
	// classifyCancelByDetail reads it for every row written before now.
	e.dispatcher.CancelOrderWithCascade(order, order.Requester(), "cancelled by "+actor,
		dispatch.CancelCause{Code: protocol.TermOperatorCancelled, Actor: actor})
	return nil
}

// failOrderAndEmit fails an order in the DB AND emits EventOrderFailed so the
// standard handler chain fires (wiring.go's failed subscribers: the Edge is
// told, a compound leg's or a swap peer's unwind runs; no return order is made).
//
// Use this from any caller that previously did a bare db.FailOrderAtomic and
// would otherwise leave the order silently failed in the DB. The fulfillment
// scanner's structural-error path uses this to ensure scanner-driven failures
// reach Edge via the same notification pipeline as fleet-driven failures.
//
// Looks up StationID and EdgeUUID from the order so the EventOrderFailed
// payload is complete — without these fields populated, the wiring.go
// handler's notification gate skips the edge push.
func (e *Engine) failOrderAndEmit(orderID int64, errorCode, detail string) {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		e.logFn("engine: load order %d for fail: %v", orderID, err)
		return
	}
	// Route through lifecycle.Fail for atomic transition + emit.
	if err := e.dispatcher.Lifecycle().Fail(order, order.Requester(), errorCode, detail); err != nil {
		e.logFn("engine: fail order %d (%s): %v", orderID, errorCode, err)
	}
}
