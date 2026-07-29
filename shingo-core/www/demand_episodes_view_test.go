package www

import (
	"strings"
	"testing"
	"time"

	"shingocore/config"
	"shingocore/domain"
)

// demand_episodes_view_test.go — the guards on the demand browser's rules.
//
// No build tag: these are pure functions and they must run on every push,
// without Docker. That is the point of having put the rules in pure functions.

func disp() config.DisplayConfig { return config.DisplayDefaults() }

func at(min int) time.Duration { return time.Duration(min) * time.Minute }

// ep pairs an episode row with its child count — the read model the view takes.
func ep(o domain.DemandOrigin, children int) domain.DemandEpisode {
	return domain.DemandEpisode{DemandOrigin: o, Children: children}
}

// ── 5.5 — an unknown enum value renders AS ITSELF ────────────────────────────

// TestCloseReasonDefaultRendersUnknownAsItself is the 5.5 guard.
//
// It is written to fail when a NEW close_reason renders as anything other than
// itself, which is the failure mode that actually happened twice: the vocabulary
// grew by claim_removed and then by superseded, and a default rendering
// "unknown" would have silently discarded both.
//
// The test uses values that are deliberately NOT in the vocabulary, because a
// test listing only today's values would go vacuous the moment the vocabulary
// grows — which is precisely the event it exists to survive.
func TestCloseReasonDefaultRendersUnknownAsItself(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		// Shapes a future close reason would plausibly take.
		{"binding_vanished", "Binding vanished"},
		{"edge_unreachable", "Edge unreachable"},
		{"operator_cancelled_manually", "Operator cancelled manually"},
		{"weird", "Weird"},
		{"UPPER_CASE_REASON", "UPPER CASE REASON"},
		{"already Spaced", "Already Spaced"},
	} {
		got := CloseReasonLabel(tc.in)
		if got != tc.want {
			t.Errorf("CloseReasonLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}

		// The load-bearing property, stated independently of the exact casing:
		// every word of the original value survives. A default that renders
		// "unknown", "", "Other", or a truncation would pass a naive equality
		// test written against one example and still throw the row's only
		// information away.
		for _, word := range strings.Split(strings.ReplaceAll(tc.in, "_", " "), " ") {
			if !strings.EqualFold("", word) && !strings.Contains(strings.ToLower(got), strings.ToLower(word)) {
				t.Errorf("CloseReasonLabel(%q) = %q — the word %q from the original value "+
					"is gone. An unrecognised reason must render AS ITSELF; the row's only "+
					"information is the string it carries.", tc.in, got, word)
			}
		}
	}

	// Known values still render as prose, so the default is not swallowing the
	// whole switch.
	for in, want := range map[string]string{
		"recovered":     "Recovered",
		"claim_removed": "Claim removed",
		"superseded":    "Superseded",
		"unattributed":  "Unattributed",
	} {
		if got := CloseReasonLabel(in); got != want {
			t.Errorf("CloseReasonLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClosedByDefaultRendersUnknownAsItself — same rule, the other vocabulary.
// closed_by has two values today and every reason to grow the same way.
func TestClosedByDefaultRendersUnknownAsItself(t *testing.T) {
	for in, want := range map[string]string{
		"notification":   "Notification",
		"sweep":          "Sweep",
		"startup_resync": "Startup resync",
		"migration":      "Migration",
	} {
		if got := ClosedByLabel(in); got != want {
			t.Errorf("ClosedByLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKindLabelDefaultRendersUnknownAsItself — the third vocabulary on the row.
func TestKindLabelDefaultRendersUnknownAsItself(t *testing.T) {
	if got := kindLabel("loader_evac"); got != "Loader evac" {
		t.Errorf("kindLabel(loader_evac) = %q, want %q", got, "Loader evac")
	}
	if got := kindLabel("threshold"); got != "Threshold" {
		t.Errorf("kindLabel(threshold) = %q", got)
	}
}

// ── The number doctrine: no data / zero / n-a are three different things ─────

// TestThreeAbsenceStatesRenderDifferently is the doctrine's load-bearing rule.
//
// All three states are produced from real rows rather than constructed by hand,
// so the test fails if the ROW BUILDER collapses them — which is where the
// collapse would actually happen.
func TestThreeAbsenceStatesRenderDifferently(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-at(5))
	closed := now.Add(-at(1))
	exp2 := 2

	// ZERO: measured, and the answer is zero. The worst case this page shows.
	zeroRow := BuildEpisodeRow(ep(domain.DemandOrigin{
		OriginID: "z", Kind: "cell", OpenedAt: opened, ExpectedOrders: &exp2,
	}, 0), now, disp())

	// NO DATA: expected_orders is NULL — the denominator is unknowable.
	noDataRow := BuildEpisodeRow(ep(domain.DemandOrigin{
		OriginID: "n", Kind: "cell", OpenedAt: opened,
		ExpectedUnknownReason: "catalog UOPCapacity was zero",
	}, 3), now, disp())

	// NOT APPLICABLE: the episode is open, so it has no close reason.
	openRow := zeroRow

	// CLOSED with NULL closed_by — no data, and specifically NOT n/a.
	closedRow := BuildEpisodeRow(ep(domain.DemandOrigin{
		OriginID: "c", Kind: "cell", OpenedAt: opened, ClosedAt: &closed,
		CloseReason: "recovered", ClosedBy: "", ExpectedOrders: &exp2,
	}, 2), now, disp())

	if zeroRow.OrdersText != "0" {
		t.Errorf("a measured zero must render as a plain 0, got %q", zeroRow.OrdersText)
	}
	if zeroRow.Ratio.Kind != CellValue || zeroRow.Ratio.Text != "0.0×" {
		t.Errorf("zero orders against a real expectation is a MEASURED ratio of 0.0×, "+
			"not an absence: got kind=%s text=%q", zeroRow.Ratio.Kind, zeroRow.Ratio.Text)
	}

	if noDataRow.Ratio.Kind != CellNoData {
		t.Errorf("a NULL expected_orders must render as no-data, got %s", noDataRow.Ratio.Kind)
	}
	if noDataRow.Ratio.Text == "0" || noDataRow.Ratio.Text == "0.0×" {
		t.Errorf("no-data rendered as a number (%q) — this is COALESCE(x,0) at the "+
			"display layer, the exact bug the doctrine names", noDataRow.Ratio.Text)
	}
	if noDataRow.Expected.Title == "" {
		t.Error("no-data must carry a title saying WHICH absence it is")
	}
	if !strings.Contains(noDataRow.Expected.Title, "UOPCapacity") {
		t.Errorf("the recorded reason was dropped from the title: %q", noDataRow.Expected.Title)
	}

	if openRow.CloseReason.Kind != CellNA {
		t.Errorf("an OPEN episode has no close reason — that is not-applicable, not "+
			"no-data: got %s", openRow.CloseReason.Kind)
	}
	if closedRow.ClosedBy.Kind != CellNoData {
		t.Errorf("a CLOSED episode with NULL closed_by is no-data (the sender did not "+
			"say), not n/a: got %s", closedRow.ClosedBy.Kind)
	}

	// The three must be mutually distinguishable in the rendered output, not
	// merely in the type. Text alone is what a reader sees.
	seen := map[string]string{}
	for label, c := range map[string]Cell{
		"zero":    zeroRow.Ratio,
		"nodata":  noDataRow.Ratio,
		"notappl": openRow.CloseReason,
	} {
		if prev, dup := seen[c.Text]; dup {
			t.Errorf("%q and %q both render as %q — the three states must look "+
				"different from each other", label, prev, c.Text)
		}
		seen[c.Text] = label
	}
}

// ── 5.4 — small denominators are greyed, not hidden ──────────────────────────

func TestSmallDenominatorIsMutedNotAbsent(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c := disp()

	exp1, exp2 := 1, 2
	small := BuildEpisodeRow(ep(domain.DemandOrigin{OpenedAt: now, ExpectedOrders: &exp1}, 3), now, c)
	ok := BuildEpisodeRow(ep(domain.DemandOrigin{OpenedAt: now, ExpectedOrders: &exp2}, 3), now, c)

	if !small.Ratio.Muted {
		t.Errorf("expected=1 must grey the ratio: one extra order reads as 2.0× and two "+
			"as 3.0×, so the column cannot discriminate (min denominator = %d)",
			c.MinExpectedOrders)
	}
	if small.Ratio.Kind != CellValue || small.Ratio.Text != "3.0×" {
		t.Errorf("a greyed ratio is still SHOWN — muted is not absent: got kind=%s text=%q",
			small.Ratio.Kind, small.Ratio.Text)
	}
	if small.Ratio.Title == "" {
		t.Error("a greyed ratio must say why, and point the reader at the order count")
	}
	if ok.Ratio.Muted {
		t.Errorf("expected=%d is at the floor and must NOT be greyed", exp2)
	}

	// The floor is CONFIG, not a literal. Move it and the classification must
	// move with it — this is what catches a hardcoded threshold at the use site.
	c.MinExpectedOrders = 10
	retuned := BuildEpisodeRow(ep(domain.DemandOrigin{OpenedAt: now, ExpectedOrders: &exp2}, 3), now, c)
	if !retuned.Ratio.Muted {
		t.Error("raising display.min_expected_orders did not change the greying — the " +
			"threshold is hardcoded at the use site rather than read from config, which " +
			"is the binding rule for every Phase 6 constant")
	}
}

// TestExpectedZeroIsNotADenominator — the guard on the other route in.
// 0 and 1 are both "lies that render as a real ratio"; NULL is handled above,
// and a stored 0 arrives by a different path.
func TestExpectedZeroIsNotADenominator(t *testing.T) {
	now := time.Now()
	for _, exp := range []int{0, -3} {
		e := exp
		row := BuildEpisodeRow(ep(domain.DemandOrigin{OpenedAt: now, ExpectedOrders: &e}, 5), now, disp())
		if row.Ratio.Kind != CellNoData {
			t.Errorf("expected_orders=%d produced ratio kind %s (%q); it must be no-data, "+
				"never a division", exp, row.Ratio.Kind, row.Ratio.Text)
		}
		if strings.Contains(row.Ratio.Text, "Inf") || strings.Contains(row.Ratio.Text, "NaN") {
			t.Errorf("expected_orders=%d rendered %q", exp, row.Ratio.Text)
		}
	}
}

// ── 5.14 — a smooth scale must never encode a threshold ──────────────────────

// TestRampCarriesMagnitudeOnlyUpToWorry is the design rule 45/60 forces.
//
// If the ramp continued past the worry line, a 61-minute episode and a
// three-hour one would render identically at the top step, losing all
// discrimination exactly past the line where it matters most. So the ramp
// saturates AT worry, and everything past it is carried by the band.
func TestRampCarriesMagnitudeOnlyUpToWorry(t *testing.T) {
	c := disp()

	if got := RampStep(0, c); got != 0 {
		t.Errorf("a zero-length episode has no magnitude to show: got step %d", got)
	}
	if got := RampStep(c.WorryAfter-time.Second, c); got != c.RampSteps {
		t.Errorf("just below the worry line should be at or near the top step: got %d of %d",
			got, c.RampSteps)
	}

	// Saturation: everything at or past worry is the SAME step, so the ramp
	// carries no information there and cannot be read as one.
	sat := RampStep(c.WorryAfter, c)
	for _, d := range []time.Duration{c.WorryAfter, c.ConcernAfter, at(180), at(600)} {
		if got := RampStep(d, c); got != sat {
			t.Errorf("the ramp must saturate at the worry line: RampStep(%s) = %d, want %d",
				d, got, sat)
		}
	}

	// The ramp is monotonic below the line, or it is not a magnitude scale.
	prev := 0
	for m := 0; m <= 45; m++ {
		got := RampStep(at(m), c)
		if got < prev {
			t.Errorf("ramp went backwards at %dm: %d after %d", m, got, prev)
		}
		if got > c.RampSteps {
			t.Errorf("ramp step %d at %dm exceeds the %d-step token set", got, m, c.RampSteps)
		}
		prev = got
	}
}

// TestBandsTakeTheirOwnChannel — crossing a line is announced in TEXT.
func TestBandsTakeTheirOwnChannel(t *testing.T) {
	c := disp()

	for _, tc := range []struct {
		d    time.Duration
		want DurationBand
	}{
		{at(0), BandCalm},
		{at(44), BandCalm},
		{c.WorryAfter - time.Second, BandCalm},
		{c.WorryAfter, BandWorry},     // inclusive: exactly 45 IS worry
		{at(59), BandWorry},           //
		{c.ConcernAfter, BandConcern}, // inclusive: exactly 60 IS concern
		{at(61), BandConcern},
		{at(600), BandConcern},
	} {
		if got := BandFor(tc.d, c); got != tc.want {
			t.Errorf("BandFor(%s) = %s, want %s", tc.d, got, tc.want)
		}
	}

	// The band must be PRINTABLE. Colour alone is barred, so an empty label on
	// an alert band would leave the crossing carried by nothing else.
	if BandFor(c.WorryAfter, c).Label() == "" {
		t.Error("the worry band has no printed name — crossing a line must take its own " +
			"channel, ring plus TEXT, never colour alone")
	}
	if BandFor(c.ConcernAfter, c).Label() == BandFor(c.WorryAfter, c).Label() {
		t.Error("worry and concern print the same name; they are separate channels")
	}
	if BandFor(0, c).Label() != "" {
		t.Error("the calm band must print nothing — a name on every row makes the two " +
			"that matter unfindable")
	}

	// The lines are CONFIG. Retune them and the classification must follow.
	c.WorryAfter, c.ConcernAfter = at(5), at(10)
	if got := BandFor(at(6), c); got != BandWorry {
		t.Errorf("retuning display.worry_after did not move the band: BandFor(6m) = %s "+
			"with worry=5m. The threshold is hardcoded at the use site.", got)
	}
	if got := BandFor(at(40), c); got != BandConcern {
		t.Errorf("retuning display.concern_after did not move the band: BandFor(40m) = %s "+
			"with concern=10m", got)
	}
}

// ── 5.1 — the sort must not bury its own worst case ──────────────────────────

func TestSortFloatsTheUnrankableAboveTheRanked(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	exp2 := 2
	build := func(id string, children int, expected *int, dur time.Duration) EpisodeRow {
		return BuildEpisodeRow(ep(domain.DemandOrigin{
			OriginID: id, Kind: "cell", OpenedAt: now.Add(-dur), ExpectedOrders: expected,
		}, children), now, disp())
	}

	rows := []EpisodeRow{
		build("mild", 2, &exp2, at(3)),  // ratio 1.0
		build("bad", 20, &exp2, at(3)),  // ratio 10.0
		build("noratio", 4, nil, at(3)), // unknowable denominator
		build("zero", 0, &exp2, at(3)),  // ratio 0.0 — the worst case
	}
	SortRows(rows)

	order := make([]string, len(rows))
	for i, r := range rows {
		order[i] = r.OriginID
	}

	if order[0] != "zero" {
		t.Errorf("sort order %v — ZERO ORDERS AGAINST A REAL DEMAND must not be sorted "+
			"to the bottom by a descending ratio sort. Its ratio is 0.0 and the plan is "+
			"explicit that it is worse than a high ratio, not milder than every row on "+
			"the floor.", order)
	}
	if order[1] != "noratio" {
		t.Errorf("sort order %v — a row the ratio cannot rank must not be interleaved "+
			"with ranked rows as though it had been ranked", order)
	}
	if order[2] != "bad" || order[3] != "mild" {
		t.Errorf("sort order %v — ranked rows must be ratio-descending", order)
	}
}

// ── 5.6 — closed_by as a visible number ──────────────────────────────────────

func TestClosedBySummaryKeepsEveryStateSeparate(t *testing.T) {
	s := SummarizeClosedBy([]string{
		"sweep", "sweep", "sweep", "notification", "", "startup_resync",
	})

	if s.Total != 6 {
		t.Errorf("Total = %d, want 6", s.Total)
	}
	if s.Sweep != 3 || s.Notification != 1 || s.Unrecorded != 1 || s.Other != 1 {
		t.Errorf("counts collapsed: %+v", s)
	}
	if s.Notification+s.Sweep+s.Unrecorded+s.Other != s.Total {
		t.Errorf("the four buckets do not sum to the total: %+v", s)
	}
	if s.SweepShare.Text != "50%" {
		t.Errorf("SweepShare = %q, want 50%% (3 of 6)", s.SweepShare.Text)
	}

	// The empty-window case is the one that matters most, because 0% is the most
	// reassuring number this tile can print and the truth is "we have not
	// measured".
	empty := SummarizeClosedBy(nil)
	if empty.SweepShare.Kind != CellNoData {
		t.Errorf("with nothing closed the sweep share is UNMEASURED, not 0%%: got kind=%s "+
			"text=%q", empty.SweepShare.Kind, empty.SweepShare.Text)
	}
	if strings.Contains(empty.SweepShare.Text, "0") {
		t.Errorf("an empty window rendered %q — a zero share and an unmeasured share are "+
			"different findings and this one is the alarm's blind spot",
			empty.SweepShare.Text)
	}
}

// ── The number doctrine's formatting rules ───────────────────────────────────

func TestFormatDurationIsCompoundAtTheRightPrecision(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0 s"},
		{47 * time.Second, "47 s"},
		{4*time.Minute + 7*time.Second, "4m 07s"},
		{9*time.Minute + 59*time.Second, "9m 59s"},
		{12 * time.Minute, "12m"},
		{45 * time.Minute, "45m"},
		{time.Hour + 4*time.Minute, "1h 04m"},
		{4*time.Hour + 12*time.Minute, "4h 12m"},
	} {
		if got := FormatDuration(tc.d); got != tc.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}

	// Never decimal hours — nobody converts 0.78h in their head.
	for _, d := range []time.Duration{at(47), at(90), at(200)} {
		if got := FormatDuration(d); strings.Contains(got, ".") {
			t.Errorf("FormatDuration(%s) = %q — durations are compound, never decimal", d, got)
		}
	}
}

func TestFormatCountNeverAbbreviatesInATable(t *testing.T) {
	for in, want := range map[int]string{
		0: "0", 7: "7", 999: "999", 1779: "1,779", 9481: "9,481",
		12400: "12,400", 1234567: "1,234,567", -443: "-443",
	} {
		if got := FormatCount(in); got != want {
			t.Errorf("FormatCount(%d) = %q, want %q", in, got, want)
		}
	}
	// A table is where someone reads the exact figure and copies it out.
	if got := FormatCount(12400); strings.ContainsAny(got, "kM") {
		t.Errorf("FormatCount abbreviated to %q — tables and detail views never abbreviate", got)
	}
}

func TestFormatRatioIsOneDecimal(t *testing.T) {
	for in, want := range map[float64]string{
		0: "0.0×", 1: "1.0×", 1.5: "1.5×", 3.25: "3.2×", 242: "242.0×",
	} {
		if got := FormatRatio(in); got != want {
			t.Errorf("FormatRatio(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestOpenEpisodeDurationUsesTheInjectedClock — no time.Now() inside.
func TestOpenEpisodeDurationUsesTheInjectedClock(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	row := BuildEpisodeRow(ep(domain.DemandOrigin{
		OriginID: "o", Kind: "cell", OpenedAt: now.Add(-at(90)),
	}, 1), now, disp())

	if row.Duration != at(90) {
		t.Errorf("open duration = %s, want 90m — measured against the passed clock", row.Duration)
	}
	if row.DurationText != "1h 30m" {
		t.Errorf("DurationText = %q, want %q", row.DurationText, "1h 30m")
	}
	if row.Band != BandConcern {
		t.Errorf("a 90-minute open episode is past the concern line: got %s", row.Band)
	}

	// Clock skew must not render as a negative duration.
	skewed := BuildEpisodeRow(ep(domain.DemandOrigin{
		OriginID: "s", Kind: "cell", OpenedAt: now.Add(at(5)),
	}, 0), now, disp())
	if skewed.Duration < 0 || strings.HasPrefix(skewed.DurationText, "-") {
		t.Errorf("clock skew rendered a negative duration: %q", skewed.DurationText)
	}
}
