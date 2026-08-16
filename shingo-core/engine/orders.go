package engine

import (
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch"
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

// TerminateOrder cancels an order, unclaims any payloads, and emits a cancellation event.
func (e *Engine) TerminateOrder(orderID int64, actor string) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}

	// Reject terminal AND post-delivery statuses. Once the bin is at the
	// destination (Delivered/Confirmed) or terminal, terminate is a no-op.
	if dispatch.IsPostDelivery(order.Status) || protocol.IsTerminal(order.Status) {
		return fmt.Errorf("cannot terminate order in status %q", order.Status)
	}

	// Route through lifecycle.CancelOrder for atomic transition + emit.
	// CancelOrder also cancels the vendor leg if active (no need to call
	// e.fleet.CancelOrder separately).
	e.dispatcher.Lifecycle().CancelOrder(order, order.StationID, "cancelled by "+actor)
	return nil
}

// failOrderAndEmit fails an order in the DB AND emits EventOrderFailed so the
// standard handler chain (audit, return order, edge notification) fires.
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
	if err := e.dispatcher.Lifecycle().Fail(order, order.StationID, errorCode, detail); err != nil {
		e.logFn("engine: fail order %d (%s): %v", orderID, errorCode, err)
	}
}
