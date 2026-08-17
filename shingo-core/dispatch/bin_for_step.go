package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"

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
//
// `orders.bin_id` is one column with two meanings, and which one you get depends
// on the order's shape:
//
//	plain retrieve (fulfillment/scanner.go) → the SOURCE bin it will pick up
//	complex        (allocator.go)           → "the bin claimed at the process
//	                                          node, else the first" — the bin at
//	                                          the CELL
//
// Both are right for their writer. Neither is "the bin this step moves", and a
// complex order touches several bins across its plan, so there is no single bin
// it "is owed".
//
// Reading bin_id as if it were per-step wedged the lane-stress rig for ten
// hours: a swap parked at a lane's mark to fetch a FRESH bin from storage was
// checked against the bin at the machine, found to be somewhere else — correctly,
// that is where it belongs — and refused entry forever. The gate's question was
// right; it was asking the order instead of asking the step.
//
// ── THREE SOURCES, IN THIS ORDER, AND ALL THREE ARE REAL ──────────────────
//
//  1. THE JUNCTION. order_bins carries (bin, step_index, node, dest) for every
//     bin an order CLAIMED, written by the allocator from the mapping it already
//     computes. This is the authoritative answer whenever it exists.
//
//  2. A RELAY PICKUP. Not every pickup claims a bin, and that is by design, not
//     an omission: a step that re-picks a bin THIS ORDER DROPPED THERE ITSELF
//     needs no claim (complex_plan.go's `potentialRelay`, and the planner records
//     such steps in Skips). The bin is whatever the order left at that node, so
//     the node IS the answer — there is nothing to look up and nothing that can
//     be stale.
//
//     An assertion that "every pickup step has a claim" would fail exactly these,
//     which is why this returns a node rather than demanding a row.
//
//  3. THE ORDER'S OWN BIN. Single-bin orders — plain retrieves, compound
//     children, and complex orders with exactly one claim — have no junction row
//     (the allocator writes it only for multi-bin orders) and do not need one:
//     with one claim, "the bin at the process node, else the first" IS that bin,
//     so both meanings coincide and bin_id is correct.
//
// ── WHAT IT DELIBERATELY DOES NOT DO ──────────────────────────────────────
//
// It does not say where the bin IS, only which bin the step is about. Locating it
// is pickupSlotNow's job and the two are kept apart on purpose: one is a fact
// about the PLAN, the other a fact about the PLANT, and conflating them is how
// the original defect read a correct plan as a broken one.
type stepBin struct {
	// binID is the bin this step moves. Zero when the answer is positional
	// (a relay) rather than a specific claimed bin.
	binID int64
	// atNode is set for a RELAY: the bin is whatever this order left at this
	// node, so the node answers the question by itself.
	atNode string
	// known is false when the plan has no answer for this step — a legitimate
	// "no claim here", which the caller resolves from the order's own columns.
	//
	// IT IS NOT THE FAIL-CLOSED SIGNAL. "The plan does not name a bin" and "I
	// could not read the plan" are different answers with different dispositions,
	// and this bool cannot hold both — that is why binForStep returns an error
	// alongside it. See the note there.
	known bool
}

// binForStep resolves the bin for one step of an order's plan.
//
// stepIndex is the index into the order's FULL step list — the plan as
// persisted, including the waits spliceLaneWait inserts.
//
// THE ALLOCATOR DOES NOT WRITE THAT INDEX; IT WRITES THE PRE-SPLICE ONE, and
// spliceLaneWait re-indexes the junction onto the persisted plan afterwards (see
// splice_reindex.go). This comment used to claim the allocator's index was
// already the full-list index. It was not, and believing it cost two orders 44
// minutes at a mark: the lookup missed, the fallback below returned the bin at
// the process node, and the gate refused a lane entry for a bin it was never
// coming for.
// THE ERROR IS THE FAIL-CLOSED HALF, AND IT USED TO BE UNREPRESENTABLE. This
// returned a bare stepBin, so an unreadable junction and a plan that names no
// bin both came back as `known: false` — and the only caller resolved that by
// falling through to order.BinID, which is exactly the wrong source F-25 is
// about. The doc here said "fail closed" while the code one function over
// failed open, on the population (multi-bin complex orders) where the two
// answers differ. An error the caller must handle is the only shape that makes
// the promise checkable.
func (d *Dispatcher) binForStep(order *orders.Order, steps []resolvedStep, stepIndex int) (stepBin, error) {
	if order == nil || stepIndex < 0 || stepIndex >= len(steps) {
		return stepBin{}, nil
	}
	step := steps[stepIndex]

	// 1. The junction, when the allocator claimed a bin for this step.
	//
	// A read failure is NOT "no junction row": it is no answer at all, and
	// falling through to bin_id on an unreadable database would substitute a
	// possibly-wrong bin for an unknown one. Fail closed — as an ERROR, so the
	// caller cannot mistake it for the absence of a claim.
	rows, err := d.db.ListOrderBins(order.ID)
	if err != nil {
		return stepBin{}, fmt.Errorf("bin for step %d of order %d: read the junction: %w",
			stepIndex, order.ID, err)
	}
	for _, r := range rows {
		if r.StepIndex == stepIndex {
			return stepBin{binID: r.BinID, known: true}, nil
		}
	}

	// 2. A RELAY: this order dropped a bin at this node earlier in its own plan,
	// so the bin it is re-picking is the one sitting there. Positional by nature
	// — the plan is the whole proof, and no row is expected.
	if step.Node != "" && orderDropsAtBefore(steps, step.Node, stepIndex) {
		return stepBin{atNode: step.Node, known: true}, nil
	}

	// 3. Single-bin orders, where the column and the step agree.
	if order.BinID != nil {
		return stepBin{binID: *order.BinID, known: true}, nil
	}
	return stepBin{}, nil
}

// wantedBin resolves the bin an order's CURRENT lane entry is for.
//
// It derives the entry step from the order itself — the wait it is parked at,
// then the first actionable step after it — so no caller has to thread a step
// index through the classifier, admission, and the rebind. "Which bin does this
// order's lane entry want" is answerable from the order alone, and keeping it
// that way is what let this fix land without changing four signatures.
//
// AN ORDER WITH NO PLAN FALLS BACK TO ITS OWN BIN, which is exactly right: a
// plain retrieve and a compound leg each have one bin, their bin_id IS the bin
// their pickup wants, and there is no step to disagree with.
//
// ── THE FALLBACK IS FOR ABSENCE, NEVER FOR AN UNREADABLE DATABASE ─────────
//
// Those are the same shape and opposite dispositions. "This plan names no bin
// for the entry step" is an ANSWER, and order.BinID is the right one for the
// single-bin orders that produce it. "I could not read the junction" is NOT an
// answer, and substituting order.BinID for it re-creates F-25 exactly: the gate
// checks a swap's lane entry against the bin at the MACHINE, finds it elsewhere,
// and refuses — definitively, under a cause that says the plant moved something,
// when in fact Core failed a query. So the error propagates and the caller holds
// the candidate as undetermined.
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
// swap that is the far side of the exchange, so a demand sitting in `sourcing`
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

func (d *Dispatcher) wantedBin(order *orders.Order) (stepBin, error) {
	if order == nil {
		return stepBin{}, nil
	}
	fromOrder := stepBin{}
	if order.BinID != nil {
		fromOrder = stepBin{binID: *order.BinID, known: true}
	}
	if order.StepsJSON == "" {
		return fromOrder, nil
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		// An unparseable plan is not a licence to guess. Fall back to the order's
		// own bin, which is what every reader did before this existed.
		//
		// NOT the same case as an unreadable junction, deliberately: the plan is
		// a column on the order that was parsed to get here at all, so a failure
		// is a malformed row rather than a database that is not answering, and it
		// will not resolve on a retry. Holding on it would park the order forever.
		return fromOrder, nil
	}
	idx, ok := entryStepIndex(order, steps)
	if !ok {
		return fromOrder, nil
	}
	b, err := d.binForStep(order, steps, idx)
	if err != nil {
		return stepBin{}, err
	}
	if b.known {
		return b, nil
	}
	return fromOrder, nil
}

// orderDropsAtBefore reports whether the plan drops a bin at node before
// stepIndex — the test for a relay pickup.
//
// It mirrors how complex_plan.go decides `potentialRelay` (a pickup whose node
// this order has already dropped into) and how resolvePerBinDestinations
// simulates the sequence. Same question, read off the same list, so a relay
// cannot be one thing to the planner and another to the gate.
func orderDropsAtBefore(steps []resolvedStep, node string, stepIndex int) bool {
	for i := 0; i < stepIndex && i < len(steps); i++ {
		if steps[i].Action == protocol.ActionDropoff && steps[i].Node == node {
			return true
		}
	}
	return false
}
