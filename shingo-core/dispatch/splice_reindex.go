package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/orders"
)

// ── THE SPLICE MOVES THE PLAN OUT FROM UNDER order_bins ───────────────────
//
// An order_bins row's pick_step and drop_step are POSITIONS IN THE ORDER'S
// PLAN. The allocator writes them during confirmComplexPlan, against the plan as
// it exists at claim time. spliceCoreWaits then INSERTS a wait step ahead of
// every gated lane entry, so every recorded position at or after an insertion
// is off by the number of waits inserted ahead of it — and the plan that gets
// persisted is the spliced one.
//
// Nothing detected the divergence because both halves stayed internally
// consistent: the junction described a plan that was correct when written, and
// steps_json described the plan that actually ran. Only a reader crossing
// between them saw it, and the first such reader was the lane gate.
//
// WHAT IT COST. binForStep looked up the entry step's index, found no row (the
// row was filed under the pre-splice index), fell through to order.BinID — "the
// bin claimed at the process node" — and reported a bin sitting at the MACHINE.
// The gate correctly observed that bin was not in the lane and refused entry
// with gate-pickup-elsewhere. The answer never changes, so the order waited at
// the mark holding a robot until someone killed it: 44 minutes on the rig for
// orders 7 and 10, which were the only two multi-bin complex orders in the run
// and therefore the only two with junction rows at all.
//
// The reader was right, the writer was right, and the transform between them
// silently invalidated the reference. So the transform repairs it: a mutation
// that shifts positions owns every stored position it shifts. Reconciling at
// read time was the alternative and it is the wrong one — it would leave the
// junction permanently describing a plan that no longer exists, and every future
// reader would have to know to compensate.

// spliceStepShift maps each PRE-splice step index to its POST-splice index.
//
// It reads the answer off the spliced plan itself rather than taking it from the
// splice as a side-channel: the steps the splice inserted are exactly the
// WaitKindLane and WaitKindTurn waits (spliceCoreWaits; the plan it is given
// carries neither, withoutInsertedWaits), and every other step is the next
// original step in order.
// Same list, same walk, so the map cannot describe a different plan than the one
// being persisted.
//
// Only entries that actually MOVE are returned — an unshifted prefix needs no
// write, and an empty map means the splice inserted nothing before any recorded
// step.
func spliceStepShift(spliced []resolvedStep) map[int]int {
	shift := make(map[int]int)
	pre := 0
	for post, s := range spliced {
		if s.Action == protocol.ActionWait && (s.WaitKind == WaitKindLane || s.WaitKind == WaitKindTurn) {
			continue // inserted by the splice; it has no pre-splice position
		}
		if post != pre {
			shift[pre] = post
		}
		pre++
	}
	return shift
}

// reindexCartsForSplice moves an order's rows onto the spliced plan's
// positions (moveCarts), then checks them against it.
//
// A FAILURE HERE IS NOT ADVISORY. Leaving the rows on stale positions is what
// produced the 44-minute wedge, and it is invisible from every other vantage
// point, so the error is returned rather than logged past.
func (d *Dispatcher) reindexCartsForSplice(order *orders.Order, spliced []resolvedStep) error {
	n, err := d.moveCarts(order, spliceStepShift(spliced))
	if err != nil {
		return fmt.Errorf("reindex the carts of order %d: %w", order.ID, err)
	}
	if n == 0 {
		return nil
	}
	d.dbg("splice: order %d's carts re-indexed onto the spliced plan (%d position(s) moved)", order.ID, n)

	// ── AND THEN IT CHECKS ITSELF, LIVE ──────────────────────────────────
	//
	// cartsPlanMismatch is the invariant that would have caught the drift on the
	// day it landed; its analog assertEachWaitGatesItsEntry runs inside the
	// splice, and this runs beside it.
	//
	// LOUD RATHER THAN FATAL, and that is a deliberate difference from the analog.
	// assertEachWaitGatesItsEntry REFUSES the splice, because a wait that gates
	// nothing sends a robot into a gated lane unannounced — an unsafe plan, where
	// not shipping is the safe direction. A drifted row is not unsafe in that way:
	// it makes the lane gate resolve a cart the entry is not for, which lands as
	// a REFUSAL carrying a cause, not a wedge. Failing the order here would kill
	// demand over a defect in Core's own bookkeeping, and a retry cannot help: a
	// mismatch is a code fault, not a plant condition. What it needs is a human,
	// and what a human needs is to be told.
	if vErr := cartsPlanMismatch(spliced, order.Carts); vErr != nil {
		log.Printf("INVARIANT: order %d's carts do not describe the plan it will run: %v "+
			"— the re-index ran and did not land the rows where they belong. The lane gate resolves "+
			"WHICH CART an entry wants from these rows, so this order's entry will be answered with "+
			"the wrong cart. It refuses rather than wedges, but the answer never changes. This is a "+
			"defect in the shift, not a plant condition, and it will not clear on a retry",
			order.ID, vErr)
	}
	return nil
}

// moveCarts moves the order's rows with its plan after a transform moved plan
// positions: moved[old] = new. Only the positions a row names are written, in
// one write (ShiftOrderBinSteps), none when no row moves; order.Carts follows.
// Returns how many positions moved.
func (d *Dispatcher) moveCarts(order *orders.Order, moved map[int]int) (int, error) {
	applicable := map[int]int{}
	for _, c := range order.Carts {
		for _, at := range []*int{c.PickStep, c.DropStep} {
			if at == nil {
				continue
			}
			if to, ok := moved[*at]; ok {
				applicable[*at] = to
			}
		}
	}
	if len(applicable) == 0 {
		return 0, nil
	}
	if err := d.db.ShiftOrderBinSteps(order.ID, applicable); err != nil {
		return 0, err
	}
	for i := range order.Carts {
		c := &order.Carts[i]
		c.PickStep, c.DropStep = shiftedStep(c.PickStep, applicable), shiftedStep(c.DropStep, applicable)
	}
	return len(applicable), nil
}

// shiftedStep is at after the shift, a fresh pointer when it moved.
func shiftedStep(at *int, shift map[int]int) *int {
	if at == nil {
		return nil
	}
	if to, ok := shift[*at]; ok {
		return &to
	}
	return at
}

// cartsPlanMismatch checks that every row names steps of the plan that do what
// the row says: its pick step a pickup (or domain.InHand, a cart in hand when
// the plan starts), its drop step a dropoff after it. Returns the breach, or
// nil.
//
// It is the order_bins analog of assertEachWaitGatesItsEntry, and it exists for
// the same reason: a position that drifts still READS as valid, so the only way
// to catch it is to ask the plan whether the position means what the row says
// it means. After a splice, a row left on its old position names a WAIT or the
// wrong kind of step; an index alone is always "present".
func cartsPlanMismatch(steps []resolvedStep, carts []domain.Cart) error {
	for _, c := range carts {
		if c.PickStep == nil {
			continue // carried over from before the rows (v173): no steps to check
		}
		pick := *c.PickStep
		if pick != domain.InHand {
			if pick < 0 || pick >= len(steps) {
				return fmt.Errorf("the row for cart %d names pick step %d, but the plan has %d steps", c.BinID, pick, len(steps))
			}
			if steps[pick].Action != protocol.ActionPickup {
				return fmt.Errorf("the row for cart %d names step %d as its pickup, but step %d of the plan is %q at %q — "+
					"the rows describe a different plan than the one that will run", c.BinID, pick, pick, steps[pick].Action, steps[pick].Node)
			}
		}
		if c.DropStep == nil {
			continue
		}
		drop := *c.DropStep
		if drop <= pick || drop >= len(steps) {
			return fmt.Errorf("the row for cart %d names drop step %d after pick step %d, outside a plan of %d steps", c.BinID, drop, pick, len(steps))
		}
		if steps[drop].Action != protocol.ActionDropoff {
			return fmt.Errorf("the row for cart %d names step %d as its set-down, but step %d of the plan is %q at %q — "+
				"the rows describe a different plan than the one that will run", c.BinID, drop, drop, steps[drop].Action, steps[drop].Node)
		}
	}
	return nil
}
