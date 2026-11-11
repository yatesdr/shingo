package dispatch

import (
	"encoding/json"
	"errors"

	"shingo/protocol"
	"shingocore/store/orders"
)

// ErrPickupNotInLane is a DEFINITE answer: the bin this pickup is for exists and
// is not in this lane. It is a sentinel so callers can tell it from an unreadable
// one — the split read_vs_missing.go draws everywhere else, applied to a reader
// that never had it.
//
// The distinction is the difference between a wait and a wedge. "Not here" is a
// fact a gate can refuse on, with a cause and a releaser; "I could not tell" is
// Core declining to answer and must fail closed. Both used to return a bare
// error, so the gate treated a knowable no as an unknown and parked a robot
// forever rather than waiting on something.
var ErrPickupNotInLane = errors.New("pickup bin is not in this lane")

// binForStep answers "WHICH BIN DOES THIS STEP MOVE" — the one spelling.
//
// ── WHY THE QUESTION HAS TO BE PER-STEP ───────────────────────────────────

// binForStep answers "WHICH CART DOES THIS STEP MOVE" — the one spelling: the
// order's row (order_bins) whose pick step or drop step is that step.
//
// ── WHY THE QUESTION HAS TO BE PER-STEP ───────────────────────────────────
//
// A complex order touches several carts across its plan, so there is no single
// cart it "is owed". Asking the order instead of the step wedged the
// lane-stress rig for ten hours: a swap parked at a lane's mark to fetch a FRESH
// cart from storage was checked against the cart at the machine, found to be
// somewhere else — correctly, that is where it belongs — and refused entry
// forever.
//
// Every lift is a row, a relay's re-lift included (domain.Lifts writes it), so
// the rows are the whole answer: a step no row names moves no cart this order
// holds. stepIndex is the index into the order's FULL plan as persisted, the
// waits spliceCoreWaits inserts included; splice_reindex.go moves the rows with
// the plan.
//
// It does not say where the cart IS, only which cart the step is about.
// Locating it is pickupSlotNow's job: one is a fact about the PLAN, the other a
// fact about the PLANT.
type stepBin struct {
	// binID is the cart this step moves.
	binID int64
	// known is false when no row of the order names this step.
	known bool
}

// binForStep is the cart plan step stepIndex of the order moves, read off the
// order's own rows (Order.Carts, loaded with the order).
func binForStep(order *orders.Order, stepIndex int) stepBin {
	if order == nil {
		return stepBin{}
	}
	for _, c := range order.Carts {
		if (c.PickStep != nil && *c.PickStep == stepIndex) || (c.DropStep != nil && *c.DropStep == stepIndex) {
			return stepBin{binID: c.BinID, known: true}
		}
	}
	return stepBin{}
}

// wantedBin is the cart an order's CURRENT lane entry is for: the cart of the
// step entryStepIndex names. Unknown when the entry step moves no cart of this
// order's; the caller then asks the step's node.
//
// AN ORDER WITH NO STORED PLAN asks about step 0, the lift of the plan its
// endpoints imply (domain.OrderPlan): a plain retrieve has one cart and no step
// to disagree with. An unparseable plan reads the same way — a malformed row
// will not resolve on a retry, and holding on it would park the order forever.
func wantedBin(order *orders.Order) stepBin {
	if order == nil {
		return stepBin{}
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil || len(steps) == 0 {
		return binForStep(order, 0)
	}
	idx, ok := entryStepIndex(order, steps)
	if !ok {
		return stepBin{}
	}
	return binForStep(order, idx)
}

// entryStepIndex is which step of an order's plan its NEXT lane entry is for.
//
// ── wait_index 0 MEANS TWO THINGS AND THE GATE BELIEVED THE WRONG ONE ─────
//
// It is the index of the wait an order is parked at, and it is also the zero
// value of a column every order is born with. Those are not the same statement.
// An order that has never been dispatched is not parked at wait 0 — it is not
// parked at anything, and its next lane entry is the FIRST step of its plan.
//
// laneEntryAfterWait, asked with a bare 0, answers for wait 0 either way: it
// finds the first wait in the plan and returns the work AFTER it. On a two-robot
// swap that is the far side of the exchange, so a demand waiting pre-dispatch
// waiting to fetch a bin OUT OF STORAGE was asked about the bin at the MACHINE.
// That bin is at a station, no station is inside a lane, and the answer is
// ErrPickupNotInLane — CauseGatePickupElsewhere, re-asked and re-refused on every
// pass, with no event in the world that can change it.
//
// Measured on the lane-stress rig 2026-08-13: FIVE demands, every one of them
// stuck this way from the first minute of the window to the last, each being
// re-driven and re-refused several times a second. Order 32's plan begins
// `pickup LSC_023` and its junction says step 0 is bin 42, which is standing at
// LSC_023 inside the very lane it was being refused from. The right answer was
// in the junction the whole time; the wrong step index is what asked for it.
//
// THIS IS F-25's FAMILY, ENTERED THROUGH THE OTHER DOOR. binForStep fixed "which
// bin does this step want"; this fixes "which step is this order's entry", and
// the failure they produce is the same sentence — a swap refused entry forever
// because it was checked against the machine-side bin.
//
// IsPreDispatch IS THE PREDICATE, not a status list written out again: pending,
// sourcing and queued are exactly the states in which no robot has been sent, so
// no wait has been reached, so wait_index cannot mean what it says. Everything
// else keeps laneEntryAfterWait, which is correct for an order that really is
// standing at a mark.
func entryStepIndex(order *orders.Order, steps []resolvedStep) (int, bool) {
	if protocol.IsPreDispatch(order.Status) {
		for i, s := range steps {
			switch s.Action {
			case protocol.ActionPickup, protocol.ActionDropoff:
				return i, true
			}
		}
		return -1, false
	}
	_, idx, _, ok := laneEntryAfterWait(steps, order.WaitIndex)
	return idx, ok
}
