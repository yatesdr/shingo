package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// ── THE SPLICE MOVES THE PLAN OUT FROM UNDER order_bins ───────────────────
//
// order_bins.step_index is a POSITION IN THE ORDER'S PLAN. The allocator writes
// it during confirmComplexPlan, against the plan as it exists at claim time.
// spliceLaneWait then INSERTS a wait step ahead of every gated lane entry, so
// every recorded position at or after an insertion is off by the number of waits
// inserted ahead of it — and the plan that gets persisted is the spliced one.
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
// WaitKindLane waits, and every other step is the next original step in order.
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
		if s.Action == protocol.ActionWait && s.WaitKind == WaitKindLane {
			continue // inserted by the splice; it has no pre-splice position
		}
		if post != pre {
			shift[pre] = post
		}
		pre++
	}
	return shift
}

// reindexOrderBinsForSplice moves an order's junction rows onto the spliced
// plan's positions.
//
// Orders with no junction rows are the overwhelming majority — the allocator
// writes them only for multi-bin complex orders — so this is a no-op for almost
// every order and must stay cheap and quiet for them.
//
// A FAILURE HERE IS NOT ADVISORY. Leaving the rows on stale indices is what
// produced the 44-minute wedge, and it is invisible from every other vantage
// point, so the error is returned rather than logged past.
func (d *Dispatcher) reindexOrderBinsForSplice(orderID int64, spliced []resolvedStep) error {
	shift := spliceStepShift(spliced)
	if len(shift) == 0 {
		return nil
	}
	rows, err := d.db.ListOrderBins(orderID)
	if err != nil {
		return fmt.Errorf("reindex order_bins for order %d: %w", orderID, err)
	}
	if len(rows) == 0 {
		return nil
	}
	// Restrict the shift to indices this order actually recorded, so the write
	// touches only rows that exist.
	applicable := make(map[int]int, len(rows))
	for _, r := range rows {
		if to, ok := shift[r.StepIndex]; ok {
			applicable[r.StepIndex] = to
		}
	}
	if len(applicable) == 0 {
		return nil
	}
	if err := d.db.ShiftOrderBinSteps(orderID, applicable); err != nil {
		return fmt.Errorf("reindex order_bins for order %d: %w", orderID, err)
	}
	d.dbg("splice: order %d junction re-indexed onto the spliced plan (%d row(s) moved)",
		orderID, len(applicable))

	// ── AND THEN IT CHECKS ITSELF, LIVE ──────────────────────────────────
	//
	// assertJunctionMatchesPlan was written as "the invariant that would have
	// caught this on the day it landed" and then called only from tests — so the
	// next position-shifting transform would get no runtime seam check, which is
	// precisely the gap that let this one live. Its analog
	// assertEachWaitGatesItsEntry runs inside the splice; this now runs beside it.
	//
	// RE-READ RATHER THAN CHECK `rows`: the shift has just moved the indices, and
	// the rows in memory still carry the old ones. Asserting against them would
	// pass while proving nothing, which is the failure mode of an invariant that
	// checks its own input.
	after, err := d.db.ListOrderBins(orderID)
	if err != nil {
		return fmt.Errorf("reindex order_bins for order %d: re-read for the invariant: %w", orderID, err)
	}
	// LOUD RATHER THAN FATAL, and that is a deliberate difference from the analog.
	//
	// assertEachWaitGatesItsEntry REFUSES the splice, because a wait that gates
	// nothing sends a robot into a gated lane unannounced — an unsafe plan, where
	// not shipping is the safe direction. A drifted junction row is not unsafe in
	// that way: it makes the lane gate resolve a bin the entry is not for, which
	// under binForStep's fail-closed contract lands as a REFUSAL carrying a cause,
	// a releaser and a heal-dig proposal — not the wedge it once was.
	//
	// The dispositions differ because the consequences do. Failing the order here
	// would kill demand over a defect in Core's own bookkeeping, which is exactly
	// what the arm above stopped doing; and a retry cannot help, because a
	// mismatch is a code fault, not a plant condition. What it needs is a human,
	// and what a human needs is to be told.
	if vErr := junctionPlanMismatch(spliced, after); vErr != nil {
		log.Printf("INVARIANT: order %d's junction does not describe the plan it will run: %v "+
			"— the re-index ran and did not land the rows where they belong. The lane gate resolves "+
			"WHICH BIN an entry wants from these rows, so this order's entry will be answered with "+
			"the wrong bin. It refuses rather than wedges, but the answer never changes. This is a "+
			"defect in the shift, not a plant condition, and it will not clear on a retry",
			orderID, vErr)
	}
	return nil
}

// junctionPlanMismatch adapts the store's rows to the invariant and returns the
// breach, or nil.
//
// It is the seam the dispatch path calls and a test can call, WITHOUT capturing
// the global logger — which every test in this package would race on, since they
// all run parallel, and which carries its own landmine (a restored nil writer
// poisons `log` process-wide).
func junctionPlanMismatch(spliced []resolvedStep, rows []*orders.OrderBin) error {
	jr := make([]junctionRow, 0, len(rows))
	for _, r := range rows {
		jr = append(jr, junctionRow{BinID: r.BinID, StepIndex: r.StepIndex, NodeName: r.NodeName})
	}
	return assertJunctionMatchesPlan(spliced, jr)
}

// assertJunctionMatchesPlan checks that every junction row names the step it is
// filed under — the invariant the splice broke.
//
// It is the order_bins analog of assertEachWaitGatesItsEntry, and it exists for
// the same reason: a positional reference that drifts still READS as valid, so
// the only way to catch it is to ask the plan whether the position means what
// the row says it means. Comparing the node is what makes it a real check; an
// index alone is always "present".
//
// The pure form, so it is callable from a fixture. The dispatch path reaches it
// through junctionPlanMismatch at the end of reindexOrderBinsForSplice — see
// there for why a breach is loud rather than fatal, and its analog the reverse.
func assertJunctionMatchesPlan(steps []resolvedStep, rows []junctionRow) error {
	for _, r := range rows {
		if r.StepIndex < 0 || r.StepIndex >= len(steps) {
			return fmt.Errorf(
				"order_bins row for bin %d names step %d, but the plan has %d steps - "+
					"the row points outside the plan it describes",
				r.BinID, r.StepIndex, len(steps))
		}
		s := steps[r.StepIndex]
		if r.NodeName != "" && s.Node != r.NodeName {
			return fmt.Errorf(
				"order_bins row for bin %d names step %d as a pickup at %q, but step %d of the "+
					"plan is %q at %q - the junction is describing a different plan than the one "+
					"that will run, which is how a lane entry resolves to a bin it is not for",
				r.BinID, r.StepIndex, r.NodeName, r.StepIndex, s.Action, s.Node)
		}
	}
	return nil
}

// junctionRow is the subset of a junction row the invariant needs, so the check
// is callable from both the store type and a test fixture.
type junctionRow struct {
	BinID     int64
	StepIndex int
	NodeName  string
}
