package www

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/domain"
)

// demand_causes_view_test.go — the guards on Stage 5.2's cause column.
//
// No build tag: pure functions, so they run on every push without Docker. That
// is the whole reason the classification lives in Go rather than in a SQL CASE.

// ── THE PROXY ────────────────────────────────────────────────────────────────

// TestReArmProxySeparatesPreFromPostDispatchCancels is the test the re-arm
// category exists or dies by.
//
// The category the plan asked for — "cancelled PRE-DISPATCH" — has no direct
// field. There is no dispatched_at, no cancelled_at and no cancel_reason on
// `orders`, and a cancelled order's current status is `cancelled`, so
// protocol.IsPreDispatch cannot answer it either. The only discriminator is
// whether the fleet vendor ever acknowledged the order.
//
// SO THIS TEST ASSERTS THE PROPERTY THAT MAKES THE PROXY WORTH HAVING, not the
// weaker one it is easy to write by accident. The weak test — "a cancelled
// order classifies as cancelled" — passes against a classifier that keys on
// status ALONE, which is precisely the classifier that cannot tell re-arm churn
// from a robot-side cancel and would report every cancelled order as churn.
//
// The property: TWO ORDERS AT THE SAME STATUS, differing only in whether the
// vendor acknowledged them, MUST LAND IN DIFFERENT BUCKETS.
func TestReArmProxySeparatesPreFromPostDispatchCancels(t *testing.T) {
	early := ClassifyChild(string(protocol.StatusCancelled), "")
	late := ClassifyChild(string(protocol.StatusCancelled), "RDS-ORDER-771")

	if early == late {
		t.Fatalf("a cancel the vendor never saw and a cancel the vendor accepted both "+
			"classified as %q.\nThe proxy's ONLY job is to separate them; a classifier "+
			"that keys on status alone reports every cancelled order as re-arm churn, "+
			"which is the number this column would then be lying about.", early)
	}
	if early != OutcomeCancelledEarly {
		t.Errorf("cancelled with no vendor id = %q, want %q", early, OutcomeCancelledEarly)
	}
	if late != OutcomeCancelledLate {
		t.Errorf("cancelled with a vendor id = %q, want %q", late, OutcomeCancelledLate)
	}

	// Whitespace is not a vendor id. A row carrying " " would otherwise be read
	// as acknowledged and quietly leave the churn bucket.
	if got := ClassifyChild(string(protocol.StatusCancelled), "   "); got != OutcomeCancelledEarly {
		t.Errorf("cancelled with a whitespace-only vendor id = %q, want %q — "+
			"blank is not an acknowledgement", got, OutcomeCancelledEarly)
	}

	// THE VENDOR ID MUST NOT MOVE ANY OTHER STATUS. It is a cancel-time
	// discriminator only; letting it leak into the other classes would make
	// "consumed" depend on a field that has nothing to do with consumption.
	for _, s := range []protocol.Status{
		protocol.StatusConfirmed, protocol.StatusDelivered,
		protocol.StatusSkipped, protocol.StatusFailed, protocol.StatusInTransit,
	} {
		withID := ClassifyChild(string(s), "RDS-ORDER-771")
		withoutID := ClassifyChild(string(s), "")
		if withID != withoutID {
			t.Errorf("status %q classified as %q with a vendor id and %q without — "+
				"the vendor id discriminates cancels and nothing else", s, withID, withoutID)
		}
	}
}

// TestProxyLabelDoesNotClaimMoreThanItMeasures holds the naming honest.
//
// The proxy catches every re-arm cancel AND every order that failed to reach
// the vendor for an unrelated reason. A label reading "Re-armed" would assert
// the first set when the data only supports the union, and the next person to
// read this page would take a churn number that is an upper bound as a
// measurement.
func TestProxyLabelDoesNotClaimMoreThanItMeasures(t *testing.T) {
	label := strings.ToLower(OutcomeCancelledEarly.Label())
	for _, forbidden := range []string{"re-arm", "rearm", "re arm", "churn"} {
		if strings.Contains(label, forbidden) {
			t.Errorf("OutcomeCancelledEarly.Label() = %q contains %q.\n"+
				"This bucket is 'cancelled before the vendor acknowledged it', which is a "+
				"SUPERSET of re-arm churn — it also holds every order that failed to reach "+
				"the vendor. Naming it after the hypothesis makes an upper bound read as a "+
				"measurement.", OutcomeCancelledEarly.Label(), forbidden)
		}
	}
	// And it must still say which side of dispatch it is on, or it says nothing.
	if !strings.Contains(label, "early") && !strings.Contains(label, "before") {
		t.Errorf("OutcomeCancelledEarly.Label() = %q says nothing about WHEN the cancel "+
			"happened, which is the only thing the proxy measures", OutcomeCancelledEarly.Label())
	}
}

// ── UNRECOGNISED VALUES ──────────────────────────────────────────────────────

// TestUnknownStatusIsNotSilentlyLive is rule 3 applied to this vocabulary.
//
// The tempting classifier is "terminal → its bucket, everything else → live",
// which puts a status string this build has never seen into "still running" —
// a reassuring, wrong answer that would make a data defect invisible. The
// ClosedBySummary.Other precedent is the same decision.
func TestUnknownStatusIsNotSilentlyLive(t *testing.T) {
	for _, unknown := range []string{
		"quarantined", "awaiting_operator", "", "PENDING", "weird-new-status",
	} {
		if got := ClassifyChild(unknown, ""); got != OutcomeOther {
			t.Errorf("ClassifyChild(%q) = %q, want %q — a status this build does not "+
				"recognise is a FINDING, not a live order", unknown, got, OutcomeOther)
		}
	}

	// Every status the protocol DOES declare must land somewhere that is not
	// "other", or the bucket stops meaning "unrecognised" and starts meaning
	// "this file went stale".
	for _, s := range protocol.AllStatuses() {
		if got := ClassifyChild(string(s), ""); got == OutcomeOther {
			t.Errorf("protocol status %q classified as %q — every declared status must "+
				"have a home, or the unrecognised bucket is just drift", s, OutcomeOther)
		}
	}
}

// TestEveryOutcomeCountsIntoItsOwnBucket guards the fold.
//
// A missing case in Add or Count silently moves a whole category into another
// one and every total still adds up, which is exactly the kind of wrong number
// nobody finds by looking.
func TestEveryOutcomeCountsIntoItsOwnBucket(t *testing.T) {
	for _, class := range causePrecedence {
		var m ChildOutcomes
		m.Add(class, 3)
		if m.Total != 3 {
			t.Errorf("%s: Total = %d, want 3", class, m.Total)
		}
		if got := m.Count(class); got != 3 {
			t.Errorf("%s: Count = %d, want 3 — Add and Count disagree about where this "+
				"class lives", class, got)
		}
		for _, other := range causePrecedence {
			if other == class {
				continue
			}
			if n := m.Count(other); n != 0 {
				t.Errorf("adding 3 to %s put %d into %s", class, n, other)
			}
		}
	}
}

// ── THE ZERO-ORDER EPISODE ───────────────────────────────────────────────────

// TestZeroOrderEpisodeIsAMeasuredFindingNotAnAbsence is the number doctrine's
// load-bearing rule, in this column.
//
// An episode with no children is the plan's WORST case — a real demand that
// produced nothing. Rendering it through the absence styling would file the
// page's most important finding under "we did not hear", which is the same
// mistake as printing 0 for an unmeasured value, mirrored.
func TestZeroOrderEpisodeIsAMeasuredFindingNotAnAbsence(t *testing.T) {
	zero, class := CauseCell(ChildOutcomes{}, true)
	if zero.Kind != CellValue {
		t.Errorf("a zero-order episode rendered as %q. It is a MEASURED result: the "+
			"query ran and found no children. Absence styling here hides the worst "+
			"thing this page can show.", zero.Kind)
	}
	if class != "" {
		t.Errorf("a zero-order episode claimed dominant class %q — there is nothing to "+
			"be dominant over", class)
	}

	// The failed read is the absence, and it must look different.
	failed, _ := CauseCell(ChildOutcomes{}, false)
	if failed.Kind != CellNoData {
		t.Errorf("an unreadable mix rendered as %q, want %q", failed.Kind, CellNoData)
	}
	if failed.Text == zero.Text {
		t.Fatalf("a zero-order episode and an unreadable mix both render as %q.\n"+
			"These are opposite findings — one says the floor produced nothing, the "+
			"other says we cannot see the floor at all.", zero.Text)
	}
	if failed.Title == "" {
		t.Error("the no-data cell has no title — the style guide requires it to say " +
			"WHICH absence this is")
	}
}

// ── THE DOMINANT CAUSE ───────────────────────────────────────────────────────

// TestDominantCauseNeverLetsConsumedHideTheChurn pins the tie-break.
//
// Consumed is the healthy class and explains nothing. If it won ties, an
// episode split evenly between consumed and cancelled-early would report
// "Consumed" and bury the one thing this column exists to surface.
func TestDominantCauseNeverLetsConsumedHideTheChurn(t *testing.T) {
	tied := ChildOutcomes{Consumed: 4, CancelledEarly: 4, Total: 8}
	got, ok := DominantOutcome(tied)
	if !ok {
		t.Fatal("a mix of 8 orders reported no dominant class")
	}
	if got == OutcomeConsumed {
		t.Errorf("4 consumed vs 4 cancelled-early reported %q. On a tie the explanatory "+
			"class must win — a healthy-looking label over an even split with churn is "+
			"the failure this column exists to prevent.", got)
	}
	if got != OutcomeCancelledEarly {
		t.Errorf("tie broke to %q, want %q", got, OutcomeCancelledEarly)
	}

	// A genuine plurality still wins outright — the tie-break must not become a
	// thumb on the scale that reports churn wherever any churn exists.
	clear := ChildOutcomes{Consumed: 9, CancelledEarly: 1, Total: 10}
	if got, _ := DominantOutcome(clear); got != OutcomeConsumed {
		t.Errorf("9 consumed vs 1 cancelled-early reported %q, want %q — precedence "+
			"breaks TIES, it does not overrule a plurality", got, OutcomeConsumed)
	}

	if _, ok := DominantOutcome(ChildOutcomes{}); ok {
		t.Error("an empty mix reported a dominant class")
	}
}

// TestCauseTitleCarriesTheWholeMix guards against the single printed name being
// the only thing a reader can get.
//
// One cell can print one word. The counts behind it are the difference between
// "mostly churn" and "one order of churn out of forty", and a column that
// prints the former for the latter is worse than no column.
func TestCauseTitleCarriesTheWholeMix(t *testing.T) {
	mix := ChildOutcomes{Consumed: 2, Skipped: 5, CancelledEarly: 1, Total: 8}
	cell, class := CauseCell(mix, true)
	if class != OutcomeSkipped {
		t.Fatalf("dominant class = %q, want %q", class, OutcomeSkipped)
	}
	for _, want := range []string{"5", "2", "1"} {
		if !strings.Contains(cell.Title, want) {
			t.Errorf("cell title %q is missing the count %q — the printed name alone "+
				"cannot distinguish 'mostly this' from 'one of forty'", cell.Title, want)
		}
	}
	// Zero classes stay out. A title listing five zeroes buries the three
	// numbers that are not zero.
	if strings.Contains(strings.ToLower(cell.Title), "failed") {
		t.Errorf("cell title %q lists a class with a zero count", cell.Title)
	}
}

// ── THE FOLD ─────────────────────────────────────────────────────────────────

// TestMissingOriginIsTheZeroMixNotAnUnknown is the distinction the whole page
// is about, at the seam where it is easiest to get backwards.
//
// An episode absent from a GROUP BY result is absent for exactly one reason: it
// has no child orders. Reading that as "unknown" would dash out every
// zero-order episode — the page's worst finding — while reading a FAILED QUERY
// as "no children" would report that worst finding on every row of a healthy
// plant. The two errors are mirror images and both are reachable from here.
func TestMissingOriginIsTheZeroMixNotAnUnknown(t *testing.T) {
	rows := []EpisodeRow{{OriginID: "has-children"}, {OriginID: "has-none"}}
	byOrigin := FoldChildCounts([]domain.ChildStatusCount{
		{OriginID: "has-children", Status: string(protocol.StatusConfirmed), ReachedVendor: true, Count: 3},
	})

	AttachCauses(rows, byOrigin, true)

	if rows[1].Cause.Kind != CellValue {
		t.Errorf("an episode absent from the mix query rendered as %q. Absent from a "+
			"GROUP BY means NO CHILDREN, which is measured — not unknown.",
			rows[1].Cause.Kind)
	}
	if rows[1].Outcomes.Total != 0 {
		t.Errorf("absent origin got Total = %d, want 0", rows[1].Outcomes.Total)
	}
	if rows[0].Outcomes.Consumed != 3 || rows[0].Cause.Kind != CellValue {
		t.Errorf("origin with 3 confirmed children: Consumed = %d, cell kind = %q",
			rows[0].Outcomes.Consumed, rows[0].Cause.Kind)
	}

	// The failed read moves EVERY row, including the one that has children —
	// the query either ran or it did not, and there is no per-row version of
	// that failure.
	AttachCauses(rows, byOrigin, false)
	for i, r := range rows {
		if r.Cause.Kind != CellNoData {
			t.Errorf("row %d rendered as %q after a failed mix read, want %q",
				i, r.Cause.Kind, CellNoData)
		}
	}
}

// TestFoldKeepsTheVendorSplit checks the bool survives the store→view boundary.
//
// The store returns a bool and the classifier takes a string, so the fold has
// to reconstitute one from the other. Getting that wrong collapses the two
// cancel classes back together AFTER the classifier has correctly separated
// them — the proxy would be right and the page still wrong.
func TestFoldKeepsTheVendorSplit(t *testing.T) {
	got := FoldChildCounts([]domain.ChildStatusCount{
		{OriginID: "e1", Status: string(protocol.StatusCancelled), ReachedVendor: false, Count: 4},
		{OriginID: "e1", Status: string(protocol.StatusCancelled), ReachedVendor: true, Count: 2},
	})
	mix := got["e1"]
	if mix.CancelledEarly != 4 || mix.CancelledLate != 2 {
		t.Errorf("fold gave early=%d late=%d, want 4 and 2 — the vendor split did not "+
			"survive the store→view boundary, so the proxy is correct and the page is "+
			"still wrong", mix.CancelledEarly, mix.CancelledLate)
	}
	if mix.Total != 6 {
		t.Errorf("Total = %d, want 6", mix.Total)
	}
}

// TestCauseTotalsEmitEveryClassIncludingZero guards the page-level strip.
//
// The style guide assigns 5.2's trend to SMALL MULTIPLES, not a stack: "four
// causes on one stack hide the one that is growing". A strip that drops
// zero-count classes reads as "not applicable here" for a class that means
// "measured, none" — and the class that just went from 5 to 0 is exactly the
// one worth seeing.
func TestCauseTotalsEmitEveryClassIncludingZero(t *testing.T) {
	totals := SummarizeCauses(map[string]ChildOutcomes{
		"e1": {Consumed: 10, Total: 10},
	})
	if len(totals.Classes) != len(causePrecedence) {
		t.Fatalf("strip emitted %d classes, want all %d — a class that dropped to zero "+
			"must still be visible at zero", len(totals.Classes), len(causePrecedence))
	}
	if totals.Total != 10 {
		t.Errorf("Total = %d, want 10", totals.Total)
	}
	if totals.Classes[0].Class != OutcomeConsumed || totals.Classes[0].Count != 10 {
		t.Errorf("strip head = %s/%d, want consumed/10 (sorted by count desc)",
			totals.Classes[0].Class, totals.Classes[0].Count)
	}
	seen := map[OutcomeClass]bool{}
	for _, c := range totals.Classes {
		if seen[c.Class] {
			t.Errorf("class %s emitted twice", c.Class)
		}
		seen[c.Class] = true
		if c.Label == "" {
			t.Errorf("class %s has no printed label — the cause is carried by a NAME, "+
				"never by colour alone", c.Class)
		}
	}
}
