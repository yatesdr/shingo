package www

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"shingocore/config"
	"shingocore/domain"
)

// cycle_time_view_test.go — the enforcement half of cycle_time_view.go.
//
// Every test names the mutation that makes it fail. Each was applied and the
// failure observed before the test was trusted.

func cycleConfig() config.DisplayConfig { return config.DisplayDefaults() }

func statsFor(gaps []time.Duration, c config.DisplayConfig) domain.CycleStats {
	return domain.SummarizeCycles(
		domain.CycleSeries{
			Key:   domain.CycleKey{Station: "SPR", Payload: "PART-A", Direction: domain.CycleDirectionProduce},
			Gaps:  gaps,
			First: time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC),
			Last:  time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC),
		},
		c.CycleMinSamples, c.CycleSpreadMultiple, c.CycleBandWidth, c.CycleFlushInterval)
}

// steady builds n gaps around 25 s with a real spread and no long ones.
func steady(n int) []time.Duration {
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, time.Duration(20+i%11)*time.Second)
	}
	return out
}

// ── Rule 4: no data, zero and not applicable must look different ─────────────

// TestThreeAbsenceStatesRenderDifferentlyOnACycleRow is the load-bearing test on
// this file — the style guide calls rule 4 the load-bearing rule in its section
// and this surface has all three states live.
//
// It fails if any TWO of them collapse, which is the actual failure mode: a
// no-data rendered as "0" is the reassuring reading on the row where nothing is
// known, and a real zero rendered as an em dash hides the good news.
//
// VERIFIED RED BY: (a) making the single-tick median NoData instead of NA — the
// kind-distinctness assertion fired, because n/a and no-data then rendered
// identically; (b) rendering the not-computable tail as Value(FormatCount(0)) —
// the text-distinctness assertion fired against the real measured zero.
func TestThreeAbsenceStatesRenderDifferentlyOnACycleRow(t *testing.T) {
	c := cycleConfig()

	// A key with exactly one tick: no interval exists yet. NOT APPLICABLE.
	single := BuildCycleRow(statsFor(nil, c), c)
	// A key with plenty of cycles and none past its own cut: a measured ZERO.
	healthy := BuildCycleRow(statsFor(steady(200), c), c)
	// A key with too few cycles for a p90: NO DATA.
	sparse := BuildCycleRow(statsFor(steady(4), c), c)

	states := map[string]Cell{
		"not applicable": single.Median,
		"no data":        sparse.P90,
		"zero":           healthy.TailCount,
	}

	if got := states["not applicable"].Kind; got != CellNA {
		t.Errorf("a single tick rendered as %s, want %s — nothing is missing and nothing "+
			"failed, an interval simply needs two events", got, CellNA)
	}
	if got := states["no data"].Kind; got != CellNoData {
		t.Errorf("a p90 withheld for want of samples rendered as %s, want %s", got, CellNoData)
	}
	if got := states["zero"].Kind; got != CellValue || states["zero"].Text != "0" {
		t.Errorf("a measured zero rendered as %s/%q, want a plain 0 — dashing out a true zero "+
			"hides the finding from the other direction", got, states["zero"].Text)
	}

	// And now the part that fails if any two collapse.
	for a, ca := range states {
		for b, cb := range states {
			if a >= b {
				continue
			}
			if ca.Kind == cb.Kind {
				t.Errorf("%q and %q share the kind %s", a, b, ca.Kind)
			}
			if ca.Text == cb.Text {
				t.Errorf("%q and %q both render as %q — a reader cannot tell them apart",
					a, b, ca.Text)
			}
		}
	}

	// Every absence must say WHICH absence it is. The style guide's "with a title
	// saying which of those it is" is not optional.
	for name, cell := range states {
		if cell.Kind != CellValue && strings.TrimSpace(cell.Title) == "" {
			t.Errorf("%q renders with no title — an absence with no stated reason tells the "+
				"reader nothing about whether to act", name)
		}
	}
}

// TestNotComputableTailIsNotZero. This is the one the plan's own worst case maps
// onto: "no cycles past the cut" and "there is no cut" are different claims, and
// zero is the reassuring one.
//
// VERIFIED RED BY: replacing the !HaveTail branch with Value(FormatCount(0)) —
// a key with no measurable spread reported a confident 0 / 0%.
func TestNotComputableTailIsNotZero(t *testing.T) {
	c := cycleConfig()

	// Perfectly regular: p90 == median, so there is no spread to derive a cut from.
	flat := make([]time.Duration, 60)
	for i := range flat {
		flat[i] = 25 * time.Second
	}
	row := BuildCycleRow(statsFor(flat, c), c)

	for name, cell := range map[string]Cell{"count": row.TailCount, "share": row.TailShare, "cut": row.TailCut} {
		if cell.Kind != CellNoData {
			t.Errorf("tail %s on a zero-spread key rendered as %s/%q — a cut that could not be "+
				"derived must not read as 'nothing is wrong'", name, cell.Kind, cell.Text)
		}
		if cell.Title == "" {
			t.Errorf("tail %s: no reason given", name)
		}
	}
	if row.SortGroup == cycleGroupRanked {
		t.Error("a row with no derivable tail was left in the ranked group, where its " +
			"position asserts a comparison the data cannot make")
	}
}

// ── The picture ──────────────────────────────────────────────────────────────

// TestBandWithOneCycleIsVisiblyNonEmpty is the absence-as-zero bug IN THE
// HISTOGRAM.
//
// On a distribution with 30% of its mass in one band — which is Springfield's
// actual shape — a band holding a single cycle scales to well under half a
// percent and rounds to zero. It then draws at exactly the height of an empty
// band, and the page tells the reader the tail is empty on the row where one
// forty-minute stop is the entire finding.
//
// VERIFIED RED BY: removing the barMinPct floor from barPercent — the
// single-cycle band came back "0%", identical to the empty ones.
func TestBandWithOneCycleIsVisiblyNonEmpty(t *testing.T) {
	if got := barPercent(1, 3000); got == "0%" {
		t.Errorf("a band holding one cycle out of 3,000 renders at %q — the same height as a "+
			"band holding none, so a reader is told the tail is empty when it is not", got)
	}
	if got := barPercent(0, 3000); got != "0%" {
		t.Errorf("an EMPTY band renders at %q, want 0%% — the floor must lift non-empty "+
			"bands only, or empty and non-empty collapse the other way", got)
	}
	if barPercent(1, 3000) == barPercent(0, 3000) {
		t.Error("one cycle and no cycles render at the same height")
	}
	if got := barPercent(3000, 3000); got != "100%" {
		t.Errorf("the tallest band renders at %q, want 100%%", got)
	}
}

// TestMedianIsAnnotatedOnTheDistribution — the style guide's words for 5.10 are
// "median annotated ON it, never alone".
//
// VERIFIED RED BY: dropping HoldsMedian from the CycleBandCell construction —
// the histogram rendered as an unmarked strip and the median existed only in the
// neighbouring column, which is exactly "alone".
func TestMedianIsAnnotatedOnTheDistribution(t *testing.T) {
	c := cycleConfig()
	row := BuildCycleRow(statsFor(steady(200), c), c)
	if !row.HaveBands {
		t.Fatal("no distribution was built for a 200-cycle key")
	}
	marked := 0
	for _, b := range row.Bands {
		if b.HoldsMedian {
			marked++
			if b.Count == 0 {
				t.Error("the band marked as holding the median is empty")
			}
		}
	}
	if marked != 1 {
		t.Errorf("%d bands marked as holding the median, want exactly 1", marked)
	}
}

// ── Sorting ──────────────────────────────────────────────────────────────────

// TestUnrankableCycleRowsFloatAboveTheRanked.
//
// A descending sort on tail share puts every row the tail cannot rank at the
// bottom, below every healthy cell — and the unrankable rows here are the two
// most interesting states the page has: a key whose median is the flush cadence
// (the feed, not the cell) and a key with too few cycles to say anything about.
// Sorting them last asserts they are calmer than everything above them.
//
// VERIFIED RED BY: removing the SortGroup comparison and sorting on
// TailShareSort alone — the order came back [tail13 tail4 sparse flush], with
// both findings under the ranked body (and NaN comparisons making the ordering
// of those two arbitrary as well).
func TestUnrankableCycleRowsFloatAboveTheRanked(t *testing.T) {
	c := cycleConfig()

	long := append(steady(180), // a real tail
		400*time.Second, 500*time.Second, 600*time.Second, 700*time.Second,
		800*time.Second, 900*time.Second, 1000*time.Second, 1100*time.Second,
		1200*time.Second, 1300*time.Second, 1400*time.Second, 1500*time.Second)
	mild := append(steady(190), 300*time.Second, 320*time.Second)

	flushGaps := make([]time.Duration, 60)
	for i := range flushGaps {
		flushGaps[i] = 4 * time.Second
	}

	rows := []CycleRow{
		BuildCycleRow(statsFor(mild, c), c),
		BuildCycleRow(statsFor(long, c), c),
		BuildCycleRow(statsFor(steady(4), c), c),
		BuildCycleRow(statsFor(flushGaps, c), c),
	}
	SortCycleRows(rows)

	if !rows[0].FlushBound {
		t.Errorf("the flush-bound row is at position %d, not the top — a finding about the "+
			"FEED buried under ranked cells is a finding nobody reads",
			indexOfFlushBound(rows))
	}
	if rows[1].TailShare.Kind != CellNoData {
		t.Errorf("the row whose tail could not be derived is not second; order is %v",
			describeRows(rows))
	}
	// The two ranked rows keep their ratio order among themselves.
	if rows[2].TailShareSort < rows[3].TailShareSort {
		t.Errorf("the ranked rows are not in descending tail-share order: %v", describeRows(rows))
	}
}

func indexOfFlushBound(rows []CycleRow) int {
	for i, r := range rows {
		if r.FlushBound {
			return i
		}
	}
	return -1
}

func describeRows(rows []CycleRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.TailShare.Text)
	}
	return out
}

// ── Precision ────────────────────────────────────────────────────────────────

// TestFormatCycleDurationRoundsRatherThanTruncates.
//
// 24.995826 s is a MEASURED Springfield value. Truncated it prints "24 s" and
// reads as a different mode from the 25 s one beside it — a discrimination the
// data does not support, invented by the formatter.
//
// VERIFIED RED BY: dropping the .Round(time.Second) — the first case returned
// "24 s".
func TestFormatCycleDurationRoundsRatherThanTruncates(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{24995826 * time.Microsecond, "25 s"},
		{29999909 * time.Microsecond, "30 s"},
		{499 * time.Millisecond, "0 s"},
		{4*time.Minute + 6*time.Second + 800*time.Millisecond, "4m 07s"},
		{59*time.Minute + 59*time.Second + 900*time.Millisecond, "1h 00m"},
	} {
		if got := FormatCycleDuration(tc.in); got != tc.want {
			t.Errorf("FormatCycleDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatPercentRoundsRatherThanTruncates. A page whose whole subject is a
// small number climbing must not print 0.6% as 0%.
//
// VERIFIED RED BY: int(pct) instead of math.Round — 0.6 printed as "0%".
func TestFormatPercentRoundsRatherThanTruncates(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{{0, "0%"}, {0.6, "1%"}, {0.4, "0%"}, {99.6, "100%"}, {12.4, "12%"}} {
		if got := FormatPercent(tc.in); got != tc.want {
			t.Errorf("FormatPercent(%g) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDirectionLabelDefaultRendersUnknownAsItself. Rule 3 of the branch's
// doctrine: BinUOPDeltaReason has grown three times, and a default returning a
// constant turns the next addition into silent data loss.
//
// VERIFIED RED BY: `default: return "Other"` — both unknown cases failed, and
// the word-survival assertion fired independently of the exact formatting.
func TestDirectionLabelDefaultRendersUnknownAsItself(t *testing.T) {
	for _, unknown := range []string{"capture_reduction", "ab_fallthrough", "some_future_reason"} {
		got := DirectionLabel(unknown)
		if got == "" {
			t.Errorf("DirectionLabel(%q) returned empty — the row's only information was the "+
				"string it carried", unknown)
			continue
		}
		// The words must survive, whatever the presentation does to them.
		for _, word := range strings.Split(unknown, "_") {
			if !strings.Contains(strings.ToLower(got), word) {
				t.Errorf("DirectionLabel(%q) = %q — %q was lost", unknown, got, word)
			}
		}
	}
	if got := DirectionLabel(domain.CycleDirectionProduce); got != "Produce" {
		t.Errorf("known value rendered as %q", got)
	}
}

// ── Anti-hardcoding at the view layer ────────────────────────────────────────

// TestCycleRowIsDrivenByConfig. Retuning a constant must change the rendered
// output, which is what "no display constant is a literal at a use site"
// actually means.
//
// VERIFIED RED BY: comparing against literal 10 / 5s inside BuildCycleRow rather
// than c.CycleMinSamples / c.CycleFlushInterval — both assertions fired.
func TestCycleRowIsDrivenByConfig(t *testing.T) {
	base := cycleConfig()
	gaps := steady(20)

	if row := BuildCycleRow(statsFor(gaps, base), base); row.Underpowered {
		t.Fatal("20 cycles should clear the shipped floor of 10; the premise is wrong")
	}

	strict := base
	strict.CycleMinSamples = 200
	if row := BuildCycleRow(statsFor(gaps, strict), strict); !row.Underpowered || row.Median.Kind != CellValue || !row.Median.Muted {
		t.Errorf("raising the sample floor to 200 did not grey the median: %+v", row.Median)
	}

	loose := base
	loose.CycleFlushInterval = time.Hour
	if row := BuildCycleRow(statsFor(gaps, loose), loose); !row.FlushBound || row.FlushNote == "" {
		t.Error("raising the flush interval above the median did not mark the row flush-bound " +
			"with a printed note")
	}
}

// ── The markup ───────────────────────────────────────────────────────────────

// TestCycleTimeTemplateKeepsTheThreeStatesDistinct closes the gap between "the
// view model distinguishes them" and "the page shows them differently".
//
// The Cell type can hold three states perfectly and a template can still render
// all three as the same span. This renders the real template against real rows
// and checks the markup.
//
// VERIFIED RED BY: pointing the shared cell partial's nodata and na arms at the
// same class — the distinctness assertion fired. Also verified by deleting the
// title attribute from the nodata arm, which fired the title assertion.
//
// The partial is templates/partials/cells.html. This lane wrote it as
// num-cell.html; two other Phase 6 lanes wrote the same renderer as
// cells.html and demand-cell.html, and the merge kept one — see that file's
// header for which arm of which copy survived and why.
func TestCycleTimeTemplateKeepsTheThreeStatesDistinct(t *testing.T) {
	base := template.Must(template.New("").Funcs(templateFuncs(nil)).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	page := template.Must(template.Must(base.Clone()).ParseFS(templateFS, "templates/cycle-time.html"))

	c := cycleConfig()
	rows := []CycleRow{
		BuildCycleRow(statsFor(steady(200), c), c), // healthy: a measured zero tail
		BuildCycleRow(statsFor(steady(4), c), c),   // sparse: no-data p90
		BuildCycleRow(statsFor(nil, c), c),         // single tick: n/a median
	}
	SortCycleRows(rows)

	var buf bytes.Buffer
	err := page.ExecuteTemplate(&buf, "content", map[string]any{
		"Page": "cycle-time", "WindowHours": 24, "MinSamples": c.CycleMinSamples,
		"SpreadMultiple": c.CycleSpreadMultiple, "BandWidth": c.CycleBandWidth,
		"FlushInterval": FormatDuration(c.CycleFlushInterval),
		"Rows":          rows, "Events": "1,000", "Unattributable": 0,
		"UnattributableText": "0", "Limit": 20000,
	})
	if err != nil {
		t.Fatalf("execute cycle-time.html: %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		`class="de-na"`,     // not applicable
		`class="de-nodata"`, // no data
		`>0<`,               // a real measured zero, unadorned
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered page does not contain %s — one of the three states is not "+
				"reaching the markup, so the distinction the view model makes is invisible", want)
		}
	}

	// The two absence markers must not be the same string, or they render alike.
	if strings.Contains(html, `class="de-na"`) && strings.Contains(html, `class="de-nodata"`) {
		if !strings.Contains(html, "n/a") || !strings.Contains(html, "—") {
			t.Error("the two absence classes are present but their glyphs are not — n/a and " +
				"no-data must differ by more than a class name a reader cannot see")
		}
	}

	// Every no-data span carries a reason.
	for _, frag := range strings.Split(html, `class="de-nodata"`)[1:] {
		if !strings.HasPrefix(strings.TrimSpace(frag), `title="`) {
			t.Errorf("a no-data span has no title: %.80s", frag)
		}
	}

	// And the median is annotated ON the distribution, not only beside it.
	if !strings.Contains(html, "ct-bin--median") {
		t.Error("no band is marked as holding the median — the style guide's requirement " +
			"for this surface is the median annotated ON the distribution")
	}

	// No chips. Stage 4 has 15 of 28 chip combinations below AA and this surface
	// would consume the broken tokens at scale.
	if strings.Contains(html, "chip-") {
		t.Error("the page uses a .chip-* class")
	}
}
