package www

import (
	"fmt"
	"sort"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
)

// demand_causes_view.go — Stage 5.2: WHY an episode's cost ratio reads the way
// it does, derived from what happened to its child orders.
//
// Pure functions of (status, vendor id, counts). No database, no template — the
// same reason demand_episodes_view.go is pure: the rules here are the ones most
// likely to be got wrong and least likely to be noticed.
//
// ── THE PLAN ROW SAYS "CAUSE CHIPS". THERE ARE NO CHIPS HERE. ────────────────
//
// The .chip-* vocabulary fails both contrast floors STRUCTURALLY, not by a
// tuning miss: a chip's fill is a color-mix of its own label colour over the
// surface, so making the text readable makes the pill invisible and vice versa.
// Fifteen of twenty-eight (theme × chip × surface) combinations are below AA on
// text, worst .chip-ok at 2.89:1. So the cause takes the same treatment the
// duration band already takes on this page — A PRINTED NAME plus a ring — which
// satisfies "never colour alone" and consumes none of the broken tokens.
// See the report's row-renaming recommendation.
//
// ── WHAT THE FOUR PLAN CATEGORIES ACTUALLY ARE ───────────────────────────────
//
// The plan names four: re-arm churn / orders dying / legitimate consumption /
// corrupt denominator. THREE of them are properties of the CHILDREN and one is a
// property of the EPISODE:
//
//   - legitimate consumption → confirmed, delivered           (children)
//   - orders dying           → skipped, failed                (children)
//   - re-arm churn           → cancelled pre-dispatch         (children, PROXY)
//   - corrupt denominator    → expected_orders NULL or <= 0   (EPISODE)
//
// THE CORRUPT DENOMINATOR IS DELIBERATELY NOT REPORTED IN THIS COLUMN, and that
// is a considered divergence from a literal reading of the plan row. It is
// already rendered twice on every affected row — the Expected cell is NoData
// with a reason, and the Ratio cell is NoData with a second reason — and those
// rows are already floated to the top of the page by SortRows. Spending the one
// cell that can carry information found nowhere else on a fact the row states
// twice would be a net loss of information per pixel. The cause column reports
// the child mix, which nothing else on the page reports at all.
//
// ── THE RE-ARM PROXY, STATED HONESTLY ────────────────────────────────────────
//
// "Cancelled PRE-DISPATCH" is not recordable from the ORDERS ROW. It carries no
// dispatched_at, no cancelled_at and no cancel_reason, and a cancelled order's
// CURRENT status is `cancelled`, so protocol.IsPreDispatch (which classifies
// pending/sourcing/queued) cannot answer it either: by the time we look, the
// order has left every state that predicate names.
//
// AN EARLIER VERSION OF THIS PARAGRAPH ALSO SAID THERE IS NO STATUS-HISTORY
// TABLE, "CONFIRMED against the schema snapshot". That was wrong, and it is the
// first thing a reader of this file would have believed. order_history exists
// (store/schema/postgres_ddl.go), carries (status, detail, code, actor, ref,
// created_at), is written by transition() on every status change, and is
// indexed by order_id. orders.EverReachedStatus asks it this EXACT question for
// one order — "did this order ever record `dispatched`" — and its own header
// explains why it has to be asked of history rather than of the current status.
//
// So the proxy is a COST choice, not an impossibility, and saying so is the
// difference between a reader trusting this file and a reader re-deriving the
// schema. CountChildrenByStatus is one GROUP BY over `orders` returning at most
// (statuses x 2) rows per episode, for every episode on the page. The history
// answer is per-ORDER: an EXISTS per child, or a join and a second aggregate
// over a table that holds every transition of every order ever. That is the
// shape the aggregate exists to avoid, and the page's cardinality argument
// above is the whole reason it is shaped this way.
//
// What has changed since, and what it is worth: a cancel now writes
// order_history.code and .actor (protocol.TermOperatorCancelled and the
// person's name on the operator doors). That does not replace the proxy — it
// is still per-order — but it means the discriminator this column approximates
// is now RECORDED rather than inferred, so a future version of this aggregate
// can group on it instead of on a vendor id.
//
// The only discriminator on the row is whether the order ever got a vendor
// order id. UpdateOrderVendor is called exactly once per order, immediately
// after the fleet vendor accepts the create (dispatcher.go and
// complex_dispatch.go), and vendor_order_id is NOT NULL DEFAULT '' — so an
// empty value means THE VENDOR NEVER ACKNOWLEDGED THIS ORDER.
//
// WHAT THE PROXY ACTUALLY SEPARATES, which is narrower than what the plan asked
// for and is the reason this is two categories rather than one:
//
//	cancelled + no vendor id  → the vendor never had it. Re-arm churn lives
//	                            here, and SO DOES every order that failed to
//	                            reach the vendor for an unrelated reason.
//	cancelled + vendor id     → the vendor had it and it was cancelled anyway.
//	                            This is NOT re-arm churn, and it is the half the
//	                            proxy can exclude with certainty.
//
// So the proxy is SOUND IN ONE DIRECTION ONLY: every re-arm cancel is in the
// first bucket, but not everything in the first bucket is a re-arm. The label
// says "Cancelled early", not "Re-armed", because naming it after the
// hypothesis rather than the measurement is how a proxy stops being read as one.
// TestReArmProxySeparatesPreFromPostDispatchCancels holds the property that
// matters: the two cancel classes must not collapse into each other.

// OutcomeClass is what became of one child order.
type OutcomeClass string

const (
	// OutcomeConsumed — the order did its job. This is the healthy case and the
	// one that explains nothing about a bad ratio.
	OutcomeConsumed OutcomeClass = "consumed"

	// OutcomeSkipped — protocol's "the work was never needed" terminal. On a
	// demand episode this is the sourcing story: the finder had no bin, so the
	// order was never needed by the time anyone looked. queue_cause carries
	// which finder tier gave up (finder-group-empty, finder-pool-empty,
	// finder-node-empty, finder-plant-empty).
	OutcomeSkipped OutcomeClass = "skipped"

	// OutcomeFailed — attempted and ended badly. Kept apart from skipped
	// because "never needed" and "tried and broke" are opposite findings that
	// the plan's single phrase "orders dying" would have merged.
	OutcomeFailed OutcomeClass = "failed"

	// OutcomeCancelledEarly — cancelled with no vendor order id. THE PROXY.
	// Named for what it measures, not for the hypothesis it supports.
	OutcomeCancelledEarly OutcomeClass = "cancelled_early"

	// OutcomeCancelledLate — cancelled after the vendor acknowledged it. The
	// half the proxy excludes with certainty.
	OutcomeCancelledLate OutcomeClass = "cancelled_late"

	// OutcomeLive — any non-terminal status. Derived from protocol.IsTerminal
	// rather than enumerated, so a status added to the protocol lands here
	// automatically instead of falling through to Other and reading as a data
	// defect.
	OutcomeLive OutcomeClass = "live"

	// OutcomeOther — a status string this build does not recognise at all.
	//
	// The same third state ClosedBySummary.Other keeps, for the same reason: a
	// summary that silently dropped unknown values would under-report the total
	// and make every share wrong. An unrecognised value is not an absent one.
	OutcomeOther OutcomeClass = "other"
)

// Label is the outcome's PRINTED name — the channel that is not colour.
func (o OutcomeClass) Label() string {
	switch o {
	case OutcomeConsumed:
		return "Consumed"
	case OutcomeSkipped:
		return "Never sourced"
	case OutcomeFailed:
		return "Failed"
	case OutcomeCancelledEarly:
		return "Cancelled early"
	case OutcomeCancelledLate:
		return "Cancelled in flight"
	case OutcomeLive:
		return "Still running"
	case OutcomeOther:
		return "Unrecognised"
	default:
		// Rule 3 applied to this vocabulary too.
		return humanizeUnknown(string(o))
	}
}

// ClassifyChild places one child order in exactly one outcome.
//
// TAKES THE RAW vendor_order_id STRING, not a pre-computed bool, so the "never
// populated" rule has exactly one home and that home is covered by a test. A
// signature taking a bool would put the load-bearing half of the proxy in
// whichever caller computed it.
func ClassifyChild(status, vendorOrderID string) OutcomeClass {
	s := protocol.Status(status)

	switch s {
	case protocol.StatusConfirmed, protocol.StatusDelivered:
		return OutcomeConsumed
	case protocol.StatusSkipped:
		return OutcomeSkipped
	case protocol.StatusFailed:
		return OutcomeFailed
	case protocol.StatusCancelled:
		// THE PROXY. Empty vendor id means the vendor never acknowledged this
		// order, so it cannot have been cancelled after dispatch.
		if strings.TrimSpace(vendorOrderID) == "" {
			return OutcomeCancelledEarly
		}
		return OutcomeCancelledLate
	}

	// Known to the protocol and not terminal → still running. Asking
	// IsTerminal rather than listing the live statuses means a new protocol
	// status is classified by its own transition table instead of by this
	// file going stale.
	if isKnownStatus(s) && !protocol.IsTerminal(s) {
		return OutcomeLive
	}
	return OutcomeOther
}

// isKnownStatus reports whether the protocol declares this status at all.
//
// Derived from protocol.AllStatuses() rather than from a list here, because a
// list here is the drift this whole file is trying not to have.
func isKnownStatus(s protocol.Status) bool {
	for _, known := range protocol.AllStatuses() {
		if known == s {
			return true
		}
	}
	return false
}

// ChildOutcomes is one episode's child mix.
//
// Every field is a REAL MEASURED COUNT including zero. There is no absence
// state inside this struct: absence is the struct not existing for an origin,
// which the caller renders as NoData.
type ChildOutcomes struct {
	Consumed       int
	Skipped        int
	Failed         int
	CancelledEarly int
	CancelledLate  int
	Live           int
	Other          int
	Total          int
}

// Add folds one classified order into the mix.
func (m *ChildOutcomes) Add(class OutcomeClass, n int) {
	m.Total += n
	switch class {
	case OutcomeConsumed:
		m.Consumed += n
	case OutcomeSkipped:
		m.Skipped += n
	case OutcomeFailed:
		m.Failed += n
	case OutcomeCancelledEarly:
		m.CancelledEarly += n
	case OutcomeCancelledLate:
		m.CancelledLate += n
	case OutcomeLive:
		m.Live += n
	default:
		m.Other += n
	}
}

// Count returns one class's tally.
func (m ChildOutcomes) Count(class OutcomeClass) int {
	switch class {
	case OutcomeConsumed:
		return m.Consumed
	case OutcomeSkipped:
		return m.Skipped
	case OutcomeFailed:
		return m.Failed
	case OutcomeCancelledEarly:
		return m.CancelledEarly
	case OutcomeCancelledLate:
		return m.CancelledLate
	case OutcomeLive:
		return m.Live
	case OutcomeOther:
		return m.Other
	}
	return 0
}

// causePrecedence orders the classes for tie-breaking, most explanatory first.
//
// A dominant cause is a PLURALITY, and pluralities tie. The order is by how
// much the class explains a ratio that reads wrong: churn and dead orders
// explain a lot, "still running" explains that it is too early to say, and
// "consumed" explains nothing at all — a row whose orders were all consumed and
// whose ratio still looks bad has its answer somewhere other than this column.
//
// Consumed is LAST deliberately. Were it first, an episode split evenly between
// consumed and cancelled-early would report "Consumed" and hide the churn,
// which is the one thing 5.2 exists to surface.
var causePrecedence = []OutcomeClass{
	OutcomeCancelledEarly,
	OutcomeSkipped,
	OutcomeFailed,
	OutcomeCancelledLate,
	OutcomeOther,
	OutcomeLive,
	OutcomeConsumed,
}

// DominantOutcome returns the class holding the plurality, ties broken by
// causePrecedence. Returns ("", false) for an empty mix — the caller decides how
// to render "no orders", because that is the page's WORST case and not a
// formatting detail this function should quietly pick a word for.
func DominantOutcome(m ChildOutcomes) (OutcomeClass, bool) {
	if m.Total <= 0 {
		return "", false
	}
	best, bestN := OutcomeClass(""), -1
	for _, class := range causePrecedence {
		if n := m.Count(class); n > bestN {
			best, bestN = class, n
		}
	}
	if bestN <= 0 {
		return "", false
	}
	return best, true
}

// MixSummary renders the whole mix as prose for the cell's title, so the one
// printed name never has to be the only thing the reader can get.
//
// Only non-zero classes appear. A title listing six zeroes buries the one
// number that is not zero, and the counts are already exact in the struct for
// anything that wants them all.
func MixSummary(m ChildOutcomes) string {
	if m.Total <= 0 {
		return "no child orders"
	}
	parts := make([]string, 0, 7)
	for _, class := range causePrecedence {
		if n := m.Count(class); n > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", FormatCount(n), strings.ToLower(class.Label())))
		}
	}
	return strings.Join(parts, ", ")
}

// CauseCell renders the cause column for one episode.
//
// THE THREE STATES, and which is which here:
//
//	value  → a real mix, including the zero-order case. "No orders" is a
//	         MEASURED finding and the worst thing this page can say; rendering
//	         it as an absence would hide the plan's own worst case behind the
//	         styling reserved for "we did not hear".
//	nodata → the child-outcome read FAILED. Passed as ok=false. An episode
//	         whose mix could not be read must not render as a blank cell that
//	         reads like "nothing happened".
//	na     → does not occur. Every episode can be asked what became of its
//	         orders; there is no row for which the question does not apply.
func CauseCell(m ChildOutcomes, ok bool) (Cell, OutcomeClass) {
	if !ok {
		return NoData("the child-order outcome mix could not be read for this episode"), ""
	}
	if m.Total <= 0 {
		// NOT NoData. The query ran and found no children. Zero orders against a
		// real demand is what this surface exists to show.
		return Cell{
			Kind:  CellValue,
			Text:  "No orders",
			Title: "this episode has no child orders at all — a measured zero, not a missing read",
		}, ""
	}
	class, found := DominantOutcome(m)
	if !found {
		return NoData("the outcome mix totals " + FormatCount(m.Total) +
			" but no class holds any of them — this is a bug, not a floor condition"), ""
	}
	return Cell{
		Kind:  CellValue,
		Text:  class.Label(),
		Title: MixSummary(m),
	}, class
}

// AttachCauses fills the cause column on every row from the outcome mix.
//
// `byOrigin` is keyed by origin_id. A row with NO ENTRY is a row the mix query
// did not return, which happens for exactly one reason — the episode has no
// child orders — so a missing key is the ZERO MIX, not a failed read. `ok`
// carries the failed read for ALL rows at once, because the query either ran or
// it did not; there is no per-row version of that failure.
//
// Getting this backwards is the whole class of bug this page is about: treating
// "absent from a GROUP BY result" as "unknown" would dash out every zero-order
// episode, and treating a failed query as "no children" would report the
// page's worst finding on every row of a plant that is running perfectly.
func AttachCauses(rows []EpisodeRow, byOrigin map[string]ChildOutcomes, ok bool) {
	for i := range rows {
		mix := byOrigin[rows[i].OriginID]
		cell, class := CauseCell(mix, ok)
		rows[i].Outcomes = mix
		rows[i].Cause = cell
		rows[i].CauseClass = string(class)
	}
}

// FoldChildCounts turns the store's raw (origin, status, reached-vendor) tally
// into one mix per episode.
//
// THE CLASSIFICATION HAPPENS HERE, ONCE, on the way out of the database and
// before anything renders — which is what keeps ClassifyChild the only place
// the rules exist. An episode with no children simply has no key, and
// AttachCauses reads that as the zero mix rather than as an unknown; see the
// note there on why those two must not be confused.
func FoldChildCounts(counts []domain.ChildStatusCount) map[string]ChildOutcomes {
	out := make(map[string]ChildOutcomes, len(counts))
	for _, c := range counts {
		// ReachedVendor is a bool on the wire but the classifier takes the raw
		// string, so that the "empty means never acknowledged" rule has exactly
		// one home. Reconstituting a sentinel here rather than adding a second
		// signature keeps that true.
		vendorID := ""
		if c.ReachedVendor {
			vendorID = "acknowledged"
		}
		mix := out[c.OriginID]
		mix.Add(ClassifyChild(c.Status, vendorID), c.Count)
		out[c.OriginID] = mix
	}
	return out
}

// CauseTotals is the page-level roll-up of the same mix — the small-multiples
// counterpart the style guide asks for instead of a stacked bar.
//
// "Four causes on one stack hide the one that is growing" (style guide, Applied
// to Phase 6). So this is counts per class, rendered as separate figures, never
// stacked and never shown as shares of one bar.
type CauseTotals struct {
	Classes []CauseTotal
	Total   int
}

// CauseTotal is one class's page-level count.
type CauseTotal struct {
	Class OutcomeClass
	Label string
	Count int
}

// SummarizeCauses rolls every episode's mix into per-class totals.
//
// EVERY CLASS IS EMITTED, including the ones at zero, and that is deliberate:
// this is a fixed set of named buckets, so a class missing from the strip would
// read as "not applicable here" when it means "measured, none". The absence
// case is the whole strip being unavailable, which the handler renders instead.
func SummarizeCauses(byOrigin map[string]ChildOutcomes) CauseTotals {
	var agg ChildOutcomes
	for _, m := range byOrigin {
		agg.Consumed += m.Consumed
		agg.Skipped += m.Skipped
		agg.Failed += m.Failed
		agg.CancelledEarly += m.CancelledEarly
		agg.CancelledLate += m.CancelledLate
		agg.Live += m.Live
		agg.Other += m.Other
		agg.Total += m.Total
	}

	out := CauseTotals{Total: agg.Total}
	for _, class := range causePrecedence {
		out.Classes = append(out.Classes, CauseTotal{
			Class: class,
			Label: class.Label(),
			Count: agg.Count(class),
		})
	}
	// Stable order for the strip: by count descending, then by precedence, so
	// the strip reads worst-first like the table does but never reshuffles for
	// two equal counts.
	rank := map[OutcomeClass]int{}
	for i, class := range causePrecedence {
		rank[class] = i
	}
	sort.SliceStable(out.Classes, func(i, j int) bool {
		if out.Classes[i].Count != out.Classes[j].Count {
			return out.Classes[i].Count > out.Classes[j].Count
		}
		return rank[out.Classes[i].Class] < rank[out.Classes[j].Class]
	})
	return out
}
