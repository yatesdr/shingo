package www

import (
	"testing"
	"time"

	"shingocore/domain"
)

// orphan_trend_view_test.go — the guards on Stage 5.7.
//
// No build tag: pure functions, run on every push without Docker.

func trendWindow() (time.Time, time.Time) {
	start := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	return start, start.Add(4 * time.Hour)
}

// ── THE ZERO DENOMINATOR ─────────────────────────────────────────────────────

// TestEmptyBucketIsAnUnmeasuredRateNotZeroPercent is 5.7's version of the rule
// the whole surface exists for.
//
// An hour in which the plant created NO ORDERS has no orphan rate. 0% is the
// most reassuring number this trend can print, and printing it for an hour
// nobody measured is the same defect as a tile printing 0 where the truth is
// "we never heard" — worse here, because a run of reassuring buckets across a
// shutdown makes the line look healthy for exactly the period it knows nothing
// about.
func TestEmptyBucketIsAnUnmeasuredRateNotZeroPercent(t *testing.T) {
	since, until := trendWindow()
	c := disp()

	// Only the first and last hours had any orders. The two in between are gaps.
	points := BuildOrphanTrend([]domain.OrphanBucket{
		{Start: since, Orphans: 2, Orders: 100},
		{Start: since.Add(3 * time.Hour), Orphans: 0, Orders: 80},
	}, since, until, c)

	if len(points) != 4 {
		t.Fatalf("got %d buckets, want 4 — the series must be GENERATED, not taken from "+
			"what the query returned. A trend drawn only from non-empty buckets closes "+
			"its own gaps and draws a line across a period nobody measured.", len(points))
	}

	for _, i := range []int{1, 2} {
		if points[i].Rate.Kind != CellNoData {
			t.Errorf("bucket %d has no orders and rendered its rate as %q (%q). "+
				"No orders means there is nothing for orphans to be a share OF.",
				i, points[i].Rate.Kind, points[i].Rate.Text)
		}
		if !points[i].Empty {
			t.Errorf("bucket %d has no orders but is not flagged Empty — the template "+
				"cannot draw the gap as a gap", i)
		}
		if points[i].Rate.Title == "" {
			t.Errorf("bucket %d no-data rate has no title saying WHICH absence it is", i)
		}
	}

	// AND THE MIRROR: a bucket that had orders and no orphans is a MEASURED
	// zero. Dashing that out hides the healthy reading and is the same error
	// from the other side.
	last := points[3]
	if last.Rate.Kind != CellValue {
		t.Errorf("a bucket with 80 orders and 0 orphans rendered as %q — that is a "+
			"measured zero and the healthiest reading this page has", last.Rate.Kind)
	}
	if last.Rate.Text != "0%" {
		t.Errorf("clean bucket rate = %q, want %q", last.Rate.Text, "0%")
	}
	if last.Empty {
		t.Error("a bucket with 80 orders is flagged Empty")
	}
	if points[0].Rate.Text != "2%" {
		t.Errorf("2 of 100 = %q, want 2%%", points[0].Rate.Text)
	}
}

// TestEmptyBucketAndThinBucketDoNotCollapse holds the two absence-adjacent
// states apart.
//
// "No orders at all" and "too few orders to trust the rate" are different
// findings with different actions — one says look at the calendar, the other
// says look at the raw count — and both are quiet, greyish things at the same
// end of the page. That is exactly the pair that collapses.
func TestEmptyBucketAndThinBucketDoNotCollapse(t *testing.T) {
	since, until := trendWindow()
	c := disp()

	points := BuildOrphanTrend([]domain.OrphanBucket{
		// Thin: real orders, but under the floor.
		{Start: since.Add(time.Hour), Orphans: 1, Orders: 3},
	}, since, until, c)

	empty, thin := points[0], points[1]

	if empty.Rate.Kind == thin.Rate.Kind {
		t.Fatalf("an empty bucket and a thin bucket both rendered as %q.\n"+
			"One has no denominator and one has a weak denominator; a page that "+
			"renders them identically cannot tell a shutdown from a noisy hour.",
			empty.Rate.Kind)
	}
	if thin.Rate.Kind != CellValue || !thin.Rate.Muted {
		t.Errorf("thin bucket rendered kind=%q muted=%v, want a MUTED VALUE — the "+
			"number exists and is shown, it is just not load-bearing",
			thin.Rate.Kind, thin.Rate.Muted)
	}
	if thin.Rate.Text == empty.Rate.Text {
		t.Errorf("both buckets print %q", thin.Rate.Text)
	}
	if thin.Rate.Title == "" {
		t.Error("a muted rate with no title never tells the reader why it is grey")
	}

	// The floor must come from CONFIG, not from a literal. Retuning it has to
	// move the greying, or the constant is decorative and its provenance record
	// describes a number nothing reads.
	loose := c
	loose.MinBucketOrders = 2
	again := BuildOrphanTrend([]domain.OrphanBucket{
		{Start: since.Add(time.Hour), Orphans: 1, Orders: 3},
	}, since, until, loose)
	if again[1].Rate.Muted {
		t.Errorf("3 orders is still muted with MinBucketOrders=2 — the floor is not " +
			"being read from config")
	}
}

// TestOrphanRateNeverCarriesMorePrecisionThanItHas pins the number doctrine's
// precision rule on this column.
func TestOrphanRateNeverCarriesMorePrecisionThanItHas(t *testing.T) {
	since, until := trendWindow()
	for _, tc := range []struct {
		orphans, orders int
		want            string
	}{
		{1, 3, "33%"},   // 33.33 → whole percent
		{2, 3, "67%"},   // 66.67 → ROUNDED, not truncated
		{1, 1000, "0%"}, // 0.1% → a real, tiny, measured rate
		{50, 100, "50%"},
		{100, 100, "100%"},
	} {
		points := BuildOrphanTrend([]domain.OrphanBucket{
			{Start: since, Orphans: tc.orphans, Orders: tc.orders},
		}, since, until, disp())
		if got := points[0].Rate.Text; got != tc.want {
			t.Errorf("%d/%d rendered %q, want %q", tc.orphans, tc.orders, got, tc.want)
		}
	}
}

// TestTrendKeepsEveryBucketInTheWindow guards the generated series' edges.
func TestTrendKeepsEveryBucketInTheWindow(t *testing.T) {
	since, until := trendWindow()
	points := BuildOrphanTrend(nil, since, until, disp())
	if len(points) != 4 {
		t.Fatalf("an empty result set produced %d buckets, want 4 — a failed or empty "+
			"query must still render the window as unmeasured, not as an absent chart",
			len(points))
	}
	for i, p := range points {
		want := since.Add(time.Duration(i) * time.Hour)
		if !p.Start.Equal(want) {
			t.Errorf("bucket %d starts %s, want %s", i, p.Start, want)
		}
		if p.Rate.Kind != CellNoData {
			t.Errorf("bucket %d of an empty result rendered as %q", i, p.Rate.Kind)
		}
	}

	// A zero or negative bucket width has no rendering; it must not loop
	// forever building one.
	bad := disp()
	bad.OrphanBucket = 0
	if got := BuildOrphanTrend(nil, since, until, bad); got != nil {
		t.Errorf("a zero bucket width produced %d points", len(got))
	}
}

// TestBarsCarryTheCountNotTheRate records the form decision.
//
// The magnitude marks are drawn from the ORPHAN COUNT, which every bucket has,
// and not from the rate, which some buckets genuinely do not. Were the bars
// driven by the rate, an unmeasured bucket would have to draw at some height,
// and every available height is a lie: zero reads as clean, anything else
// invents a number.
func TestBarsCarryTheCountNotTheRate(t *testing.T) {
	since, until := trendWindow()
	points := BuildOrphanTrend([]domain.OrphanBucket{
		// Rate 50% on a count of 1; rate 10% on a count of 10. If the bars
		// tracked the rate, the FIRST bucket would be the tall one.
		{Start: since, Orphans: 1, Orders: 2},
		{Start: since.Add(time.Hour), Orphans: 10, Orders: 100},
	}, since, until, disp())

	if points[0].BarStep >= points[1].BarStep {
		t.Errorf("bucket with 1 orphan drew at step %d and bucket with 10 drew at step "+
			"%d — the bars must carry the COUNT, which every bucket has, not the rate, "+
			"which some buckets do not", points[0].BarStep, points[1].BarStep)
	}
	if points[1].BarStep != disp().BarSteps {
		t.Errorf("the tallest bucket drew at step %d, want %d",
			points[1].BarStep, disp().BarSteps)
	}
	// An empty bucket draws nothing, which is correct: nothing is what happened.
	if points[2].BarStep != 0 {
		t.Errorf("an empty bucket drew at step %d", points[2].BarStep)
	}

	// A NON-ZERO COUNT NEVER ROUNDS AWAY TO NOTHING. One orphan beside a
	// bucket of two hundred must still draw, or the chart says nothing
	// happened in a bucket where something did.
	tiny := BuildOrphanTrend([]domain.OrphanBucket{
		{Start: since, Orphans: 1, Orders: 500},
		{Start: since.Add(time.Hour), Orphans: 200, Orders: 500},
	}, since, until, disp())
	if tiny[0].BarStep < 1 {
		t.Errorf("1 orphan against a maximum of 200 drew at step %d. Any non-zero "+
			"count must reach step 1: 'nothing happened here' is a claim the data "+
			"does not make.", tiny[0].BarStep)
	}
}

// ── THE LANE ─────────────────────────────────────────────────────────────────

// TestNoLiveFindingsIsNotApplicableNotUnknown separates the third state on the
// lane's one nullable column.
//
// A station with nothing live has no oldest-live age — the question has no
// subject. A station whose timestamps went missing WOULD be an absence. A
// cleaned-up station must not read like a broken one, or the lane's healthiest
// row and its most suspicious row look the same.
func TestNoLiveFindingsIsNotApplicableNotUnknown(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	old := now.Add(-90 * time.Minute)

	lane := BuildOrphanLane([]domain.OrphanSite{
		{StationID: "PLANT.LINE1", Live: 2, Aged: 5, Total: 7, OldestLive: &old},
		{StationID: "PLANT.LINE2", Live: 0, Aged: 9, Total: 9, OldestLive: nil},
	}, now)

	clean := lane.Sites[1]
	if clean.OldestLive.Kind != CellNA {
		t.Errorf("a station with no live findings rendered its oldest-live as %q, want "+
			"%q. There is no age because there is nothing to be old — that is not the "+
			"same claim as 'we do not know'.", clean.OldestLive.Kind, CellNA)
	}
	if clean.LiveText != "0" {
		t.Errorf("live count rendered %q — a station with zero live findings has a "+
			"MEASURED zero and it is the healthy reading", clean.LiveText)
	}

	live := lane.Sites[0]
	if live.OldestLive.Kind != CellValue || live.OldestLive.Text != "1h 30m" {
		t.Errorf("oldest live = %q/%q, want a value of 1h 30m",
			live.OldestLive.Kind, live.OldestLive.Text)
	}
	if live.OldestLive.Text == clean.OldestLive.Text {
		t.Error("a live age and a not-applicable both render the same text")
	}

	if lane.LiveTotal != 2 || lane.AgedTotal != 14 || lane.Total != 16 {
		t.Errorf("totals live=%d aged=%d total=%d, want 2/14/16",
			lane.LiveTotal, lane.AgedTotal, lane.Total)
	}
}

// TestLaneAgeUsesTheInjectedClock — the age of a live finding depends on `now`,
// so `now` has to be a parameter. A function reaching for time.Now() cannot be
// tested at a boundary and this repo has already established injection for it.
func TestLaneAgeUsesTheInjectedClock(t *testing.T) {
	pinned := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	created := pinned.Add(-3 * time.Hour)
	lane := BuildOrphanLane([]domain.OrphanSite{
		{StationID: "S", Live: 1, Total: 1, OldestLive: &created},
	}, pinned)

	if got := lane.Sites[0].OldestLive.Text; got != "3h 00m" {
		t.Errorf("oldest live age = %q, want 3h 00m against the pinned clock. A "+
			"duration read from time.Now() would be ~%s off and would change on "+
			"every run.", got, time.Since(pinned).Truncate(time.Hour))
	}
}

// TestBucketsAlignToTheSameEpochPostgresUsed guards a silent, total failure.
//
// The store floors bucket starts against the UNIX EPOCH. The view floors them
// again to build the series and matches the two by map key. Go's time.Truncate
// LOOKS like the same operation and is not — it floors against the zero time,
// 1 January of year 1, which is 62,135,596,800 seconds earlier. The two agree
// only for widths that divide that gap evenly.
//
// An hour does divide it, and so does a day, which is exactly why this is worth
// a test rather than a comment: every width anyone would reach for first works,
// so the bug ships and waits. When it fires, EVERY KEY MISSES — the page reports
// "no orders were created" for every bucket in the window, which reads as a
// plant that has stopped, while the real counts sit unread in the result set.
func TestBucketsAlignToTheSameEpochPostgresUsed(t *testing.T) {
	// Fifty minutes: 62,135,596,800 / 3000 is not an integer, so this is a width
	// where Truncate and the epoch floor disagree.
	const width = 50 * time.Minute

	c := disp()
	c.OrphanBucket = width

	// A bucket start exactly as Postgres would emit it: an integer multiple of
	// the width since the epoch.
	epochAligned := time.Unix((time.Now().Unix()/int64(width/time.Second)-3)*int64(width/time.Second), 0).UTC()

	points := BuildOrphanTrend([]domain.OrphanBucket{
		{Start: epochAligned, Orphans: 4, Orders: 200},
	}, epochAligned, epochAligned.Add(3*width), c)

	if len(points) != 3 {
		t.Fatalf("got %d buckets, want 3", len(points))
	}
	if points[0].Orders != 200 || points[0].Orphans != 4 {
		t.Errorf("the store's row did not land in the generated bucket it belongs to: "+
			"orders=%d orphans=%d, want 200 and 4.\nThe view and the store are flooring "+
			"against different epochs, so every key misses and the whole window renders "+
			"as unmeasured while the counts sit right there in the result set.",
			points[0].Orders, points[0].Orphans)
	}
	if points[0].Rate.Kind != CellValue {
		t.Errorf("bucket 0 rendered as %q despite carrying 200 orders", points[0].Rate.Kind)
	}

	// And the demonstration that the two flooring rules really do differ at this
	// width — so the test above is testing something rather than passing by luck.
	if bucketStart(epochAligned, width).Equal(epochAligned.Truncate(width)) {
		t.Fatal("bucketStart and time.Truncate agree at a 50-minute width, so this test " +
			"cannot detect the bug it exists for — pick a width where they differ")
	}
}
