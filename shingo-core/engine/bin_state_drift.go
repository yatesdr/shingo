package engine

import (
	"sync"

	"shingocore/dispatch"
	"shingocore/store/orders"
)

// ── THE FIRST BIN-STATE TRIPWIRE ──────────────────────────────────────────
//
// One bin-state fact — where a bin is DUE — is recorded in two places: the
// order's plan, and order_bins.dest_node. The plan is authoritative (it is what
// the robot is driven from); the junction row is a projection of it, written by
// the allocator and, since the re-bind learned to, kept in step.
//
// This counts the times they disagree at the moment the disagreement would
// actually cost something: settle time, when the junction row is what places the
// bin. It is a SHOULD-BE-ZERO. Non-zero means a writer moved the plan and left
// the projection behind — which is D2, and which is precisely the assertion that
// was FALSE IN PRODUCTION when this batch started. Its turning true is the cure's
// proof, and until then the count is the size of the population A inherits.
//
// LABEL AND COUNT ONLY. It does not choose what gets written — the arrived check
// and the settle assert do that. An instrument that also repairs is an instrument
// nobody can use to measure whether the repair was needed.
//
// ── THE INSTRUMENT RULES, WHICH THIS FILE IS THE FIRST TO GET RIGHT UP FRONT ──
//
//   - It names the MECHANISM, not the symptom. "dest_node disagrees" is a
//     symptom; "the allocation-time destination went stale against the plan, and
//     the re-bind should have updated it" tells the reader where to go.
//   - The COUNTER is the number, not a grep. Every should-be-zero on this branch
//     that was read by grepping was read wrong at least once.
//   - The tally line does NOT contain its own search pattern. That is the R.9
//     artifact — a tally that quotes its grep string is counted by that grep, so
//     it reads non-zero forever and the reader learns to ignore it.
//   - The instrument emits its own read instruction, so nobody has to remember
//     one.

// DestNodeDriftMarker is the per-event line's search string, named once so the
// emitter, the periodic tally and the guard test share one definition — and so
// the tally can be checked for NOT containing it.
const DestNodeDriftMarker = "bin-state drift at"

// The settle sites, named once so the tally's keys cannot drift from the callers.
const (
	driftSiteDelivery  = "multi-bin delivery settle"
	driftSiteCompleted = "multi-bin completion settle"
)

// destNodeDriftTally counts, per site, the junction rows whose recorded
// destination disagreed with the order's own plan at settle time.
//
// In-process and reset-on-restart, like the arrival-refusal tally: it is a
// tripwire reading, not a fact anything recovers from. The durable evidence is
// the per-order line, which carries the order, the bin, and both nodes.
var destNodeDriftTally = struct {
	mu     sync.Mutex
	bySite map[string]int
}{bySite: map[string]int{}}

// DestNodeDriftTally returns drift counts so far, by site. Expected to be EMPTY.
func DestNodeDriftTally() map[string]int {
	destNodeDriftTally.mu.Lock()
	defer destNodeDriftTally.mu.Unlock()
	out := make(map[string]int, len(destNodeDriftTally.bySite))
	for k, v := range destNodeDriftTally.bySite {
		out[k] = v
	}
	return out
}

// resetDestNodeDriftTally exists for tests, which must not inherit a count from
// whichever test ran before them.
func resetDestNodeDriftTally() {
	destNodeDriftTally.mu.Lock()
	defer destNodeDriftTally.mu.Unlock()
	destNodeDriftTally.bySite = map[string]int{}
}

// noteDestNodeDrift compares every junction row against the order's CURRENT plan
// and counts the ones that disagree.
//
// The comparison runs through dispatch.PlannedBinDestinations — the same single
// derivation the allocator and the re-bind write from — so this measures a RECORD
// against a PLAN, not one derivation against another.
//
// Silent and free on the ordinary path: no junction rows means a single-bin
// order, and a plan that will not parse is not a drift finding (the settle logs
// its own failures). It is called for its side effect and returns nothing,
// because nothing may branch on it.
func (e *Engine) noteDestNodeDrift(order *orders.Order, orderBins []*orders.OrderBin, site string) {
	if order == nil || len(orderBins) == 0 || order.StepsJSON == "" {
		return
	}
	claimed := make(map[string]int64, len(orderBins))
	for _, ob := range orderBins {
		claimed[ob.NodeName] = ob.BinID
	}
	planned, err := dispatch.PlannedBinDestinations(order.StepsJSON, claimed)
	if err != nil {
		return
	}
	for _, ob := range orderBins {
		want, ok := planned[ob.BinID]
		// An unplanned bin (the plan no longer moves it) and a bin the plan leaves
		// where it is are both outside this question. Only a row that names a
		// DIFFERENT node than the plan does is drift.
		if !ok || want == "" || ob.DestNode == "" || want == ob.DestNode {
			continue
		}
		destNodeDriftTally.mu.Lock()
		destNodeDriftTally.bySite[site]++
		destNodeDriftTally.mu.Unlock()

		e.logFn("WARN: "+DestNodeDriftMarker+" %s — order %d bin %d is recorded for %s but its own plan "+
			"sends it to %s. The allocation-time destination went stale against the plan; the "+
			"re-bind is what should have updated it (rebindGatedDropoff → refreshOrderBinDestinations). "+
			"Expected count is ZERO: the bin is about to be placed from the RECORD, so the robot and "+
			"the ledger are about to disagree about where this bin is.",
			site, order.ID, ob.BinID, ob.DestNode, want)
	}
}
