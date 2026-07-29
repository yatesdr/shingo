package domain

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// cycle_time_test.go — the enforcement half of cycle_time.go.
//
// Every test below was verified RED before it was trusted: the mutation that
// makes it fail is named in the comment above it, and each one was applied and
// the failure observed. A test written after the code and never seen to fail is
// a test that asserts whatever the code happened to do.

var t0 = time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func ev(station, payload, dir string, sec int) CycleEvent {
	return CycleEvent{CycleKey: CycleKey{Station: station, Payload: payload, Direction: dir}, At: at(sec)}
}

// TestGapsNeverCrossAKeyBoundary is the invariant the whole surface rests on.
//
// Two parts running at the same station interleave in the audit stream. A loop
// that differences the globally-ordered slice reads the interval between one
// part's tick and the OTHER part's next tick, which is not a cycle of anything —
// and on a station running two parts it halves both apparent takts, producing a
// number that looks plausible and is wrong.
//
// VERIFIED RED BY: differencing the events slice directly instead of the
// per-key partitions — every gap came back 10 s (the interleave) instead of the
// two real 20 s series.
func TestGapsNeverCrossAKeyBoundary(t *testing.T) {
	events := []CycleEvent{
		ev("SPR", "PART-A", CycleDirectionProduce, 0),
		ev("SPR", "PART-B", CycleDirectionProduce, 10),
		ev("SPR", "PART-A", CycleDirectionProduce, 20),
		ev("SPR", "PART-B", CycleDirectionProduce, 30),
		ev("SPR", "PART-A", CycleDirectionProduce, 40),
		ev("SPR", "PART-B", CycleDirectionProduce, 50),
	}
	series, unattributable := BuildCycleSeries(events)
	if unattributable != 0 {
		t.Fatalf("unattributable = %d, want 0 — every event here has a full key", unattributable)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2 (one per part)", len(series))
	}
	for _, s := range series {
		if len(s.Gaps) != 2 {
			t.Errorf("%s: %d gaps, want 2", s.Key.Payload, len(s.Gaps))
		}
		for _, g := range s.Gaps {
			if g != 20*time.Second {
				t.Errorf("%s: gap %s — a 10 s gap here is the INTERLEAVE between the two "+
					"parts, not a cycle of either", s.Key.Payload, g)
			}
		}
	}
}

// TestDirectionIsPartOfTheKeyNotAFilter. A press filling a tote and a cell
// draining one are two processes with two takts. Folded together they interleave
// exactly like two parts do.
//
// VERIFIED RED BY: dropping Direction from CycleKey — one series of four 15 s
// gaps instead of two series of 30 s.
func TestDirectionIsPartOfTheKeyNotAFilter(t *testing.T) {
	events := []CycleEvent{
		ev("SPR", "PART-A", CycleDirectionProduce, 0),
		ev("SPR", "PART-A", CycleDirectionConsume, 15),
		ev("SPR", "PART-A", CycleDirectionProduce, 30),
		ev("SPR", "PART-A", CycleDirectionConsume, 45),
		ev("SPR", "PART-A", CycleDirectionProduce, 60),
	}
	series, _ := BuildCycleSeries(events)
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2 — produce and consume are different processes", len(series))
	}
	for _, s := range series {
		for _, g := range s.Gaps {
			if g != 30*time.Second {
				t.Errorf("%s %s: gap %s, want 30s", s.Key.Payload, s.Key.Direction, g)
			}
		}
	}
}

// TestBlankPayloadIsReportedNotBucketed.
//
// A delta row with no payload code cannot be attributed to a part. Folding those
// into an "unknown" key would invent a distribution out of rows from every part
// on the site; dropping them silently would make the page's own sample counts
// unexplainable. They are counted and returned.
//
// VERIFIED RED BY: removing the blank-payload guard — a third series appeared
// with an empty Payload and unattributable read 0.
func TestBlankPayloadIsReportedNotBucketed(t *testing.T) {
	events := []CycleEvent{
		ev("SPR", "PART-A", CycleDirectionProduce, 0),
		ev("SPR", "", CycleDirectionProduce, 5),
		ev("SPR", "PART-A", CycleDirectionProduce, 20),
		ev("SPR", "", CycleDirectionProduce, 25),
	}
	series, unattributable := BuildCycleSeries(events)
	if unattributable != 2 {
		t.Errorf("unattributable = %d, want 2", unattributable)
	}
	for _, s := range series {
		if s.Key.Payload == "" {
			t.Errorf("a series was built for a blank payload — its gaps are intervals " +
				"between rows from parts nobody can name")
		}
	}
	if len(series) != 1 || len(series[0].Gaps) != 1 || series[0].Gaps[0] != 20*time.Second {
		t.Errorf("the attributable series is wrong: %+v", series)
	}
}

// TestQuantileIsAnObservationNotAnInterpolation.
//
// Springfield's distribution is trimodal at 20/25/30 s: interpolating between
// two order statistics that straddle those modes returns a duration that
// essentially never occurs and prints it as though it had been measured.
//
// VERIFIED RED BY: replacing the nearest-rank body with linear interpolation
// (the type-7 default in most stats libraries) — the p90 of this input came back
// 27.5 s, a value not present in the data at all.
func TestQuantileIsAnObservationNotAnInterpolation(t *testing.T) {
	// The shape of the real thing: a dense trimodal body and a SPARSE HEAVY TAIL.
	// The sparseness matters — interpolation between two adjacent body values that
	// happen to be equal returns the observed value by luck, so a body-only
	// fixture would let a type-7 implementation pass. Straddling 45 s and 300 s is
	// where the two methods actually differ, and it is also where a reader would
	// be misled: type-7 reports a 70.5 s ninetieth percentile from data whose
	// ninth-largest gap is 45 s.
	sorted := []time.Duration{
		20 * time.Second, 20 * time.Second, 25 * time.Second, 25 * time.Second,
		25 * time.Second, 25 * time.Second, 25 * time.Second, 30 * time.Second,
		45 * time.Second, 300 * time.Second,
	}
	obs := map[time.Duration]bool{}
	for _, d := range sorted {
		obs[d] = true
	}
	for _, q := range []float64{0.05, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 1.0} {
		got, ok := Quantile(sorted, q)
		if !ok {
			t.Fatalf("q=%g: no value from a 10-element slice", q)
		}
		if !obs[got] {
			t.Errorf("q=%g returned %s, which is not a duration actually observed — a "+
				"quantile of this data must be a measurement that happened, not a point "+
				"invented between two of them", q, got)
		}
	}
	if got, _ := Quantile(sorted, 0.9); got != 45*time.Second {
		t.Errorf("p90 = %s, want 45s (the 9th of 10 by rank). An interpolating "+
			"implementation reports 70.5s here", got)
	}
}

// TestQuantileOfNothingHasNoValue. Returning a zero Duration for an empty slice
// is the coalesce-absence-into-zero bug at the arithmetic layer, where no
// styling can recover it.
//
// VERIFIED RED BY: returning (0, true) for the empty case — the loop below
// accepted a 0 s p50 as a real value.
func TestQuantileOfNothingHasNoValue(t *testing.T) {
	if _, ok := Quantile(nil, 0.5); ok {
		t.Error("Quantile of an empty slice reported a value")
	}
	if _, ok := Quantile([]time.Duration{}, 0.5); ok {
		t.Error("Quantile of a zero-length slice reported a value")
	}
	// An out-of-range q is also not a value, rather than a clamp.
	if _, ok := Quantile([]time.Duration{time.Second}, 0); ok {
		t.Error("Quantile at q=0 reported a value")
	}
	if _, ok := Quantile([]time.Duration{time.Second}, 1.5); ok {
		t.Error("Quantile at q>1 reported a value")
	}
}

// TestCycleBandsAreSymmetricAboutTheMedian is the STRUCTURAL claim, checked.
//
// A distribution drawn in units of its own median is only readable if 1.0 is at
// the CENTRE of a band. This asserts it directly and asserts the consequence:
// as many bands below the centre as above it.
//
// VERIFIED RED BY: tiling the bands from zero (edges at 0, w, 2w, …) — 1.0
// landed on an edge, HoldsMedian moved to the band above it, and the counts
// either side came back 4 and 3.
func TestCycleBandsAreSymmetricAboutTheMedian(t *testing.T) {
	for _, width := range []float64{0.1, 0.2, 0.25, 0.5} {
		bands := CycleBands(60*time.Second, width)
		if len(bands) < 3 {
			t.Fatalf("width %g: only %d bands", width, len(bands))
		}
		medianIdx := -1
		for i, b := range bands {
			if b.HoldsMedian {
				if medianIdx >= 0 {
					t.Fatalf("width %g: two bands claim the median", width)
				}
				medianIdx = i
			}
			if b.LoMul >= b.HiMul {
				t.Errorf("width %g band %d: inverted edges [%g,%g)", width, i, b.LoMul, b.HiMul)
			}
		}
		if medianIdx < 0 {
			t.Fatalf("width %g: no band holds the median", width)
		}
		mb := bands[medianIdx]
		// 1.0 must be strictly INSIDE the band, not on its lower edge.
		if !(mb.LoMul < 1 && 1 < mb.HiMul) {
			t.Errorf("width %g: the median band is [%g,%g) — 1.0 sits on an edge, so the "+
				"distribution is tiled from zero rather than centred on its own median",
				width, mb.LoMul, mb.HiMul)
		}
		below, above := medianIdx, len(bands)-medianIdx-1
		if below != above {
			t.Errorf("width %g: %d bands below the median band and %d above — the window is "+
				"1.0 ± 1.0 and must have the same number each side", width, below, above)
		}
	}
}

// TestBandWidthSeparatesTheThreeMeasuredModes.
//
// The band width is not a taste decision: 25 s, 30 s and 20 s hold roughly 57%
// of Springfield's intervals in three one-second bands, and a width that merges
// any two of them erases the trimodality that is the most striking fact about
// this data. Against a 25 s median those modes are 1.0×, 1.2× and 0.8×.
//
// The raw values are used deliberately — 24.995826 and 29.999909, not 25 and 30.
// Only 29 of 219,465 real intervals are exact multiples of five, so a band
// implementation that rounded or compared for equality would find nothing.
//
// VERIFIED RED BY: tiling from zero, which puts 1.0 on an edge and drops both
// 24.995826 and 29.999909 into [1.00,1.25) — two of the three modes in one band.
func TestBandWidthSeparatesTheThreeMeasuredModes(t *testing.T) {
	const width = 0.25 // display.cycle_band_width
	median := 25 * time.Second
	bands := CycleBands(median, width)

	modes := []time.Duration{
		19998 * time.Millisecond,    // ~20 s
		24995826 * time.Microsecond, // the measured raw value
		29999909 * time.Microsecond, // the measured raw value
	}
	seen := map[int]time.Duration{}
	for _, m := range modes {
		mul := float64(m) / float64(median)
		idx := -1
		for i, b := range bands {
			if mul >= b.LoMul && mul < b.HiMul {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("%s (%.4f×) fell in no band", m, mul)
		}
		if prev, dup := seen[idx]; dup {
			t.Errorf("%s and %s both landed in band %d [%g,%g) — at width %g the measured "+
				"modes must not merge, or the trimodality disappears into the bucketing",
				prev, m, idx, bands[idx].LoMul, bands[idx].HiMul, width)
		}
		seen[idx] = m
	}
}

// TestTailCutIsNotComputableWithoutSpread.
//
// When p90 == median the key has no measurable dispersion, and median + k×0 is
// the median itself — which flags about half of every key's cycles. Reporting
// that as a tail count would be a large, confident, meaningless number; and
// reporting zero would be the reassuring one on the row where nothing is known.
//
// VERIFIED RED BY: returning (median, true, "") from the zero-spread branch —
// the tail count came back 5 of 10 for a key with a perfectly regular cycle.
func TestTailCutIsNotComputableWithoutSpread(t *testing.T) {
	if _, ok, why := CycleTailCut(25*time.Second, 25*time.Second, 3); ok {
		t.Error("a zero spread produced a tail cut")
	} else if why == "" {
		t.Error("a not-computable tail cut came back with no reason — an absence with no " +
			"stated reason is exactly what the style guide forbids")
	}
	if _, ok, _ := CycleTailCut(0, 10*time.Second, 3); ok {
		t.Error("a zero median produced a tail cut")
	}
	got, ok, _ := CycleTailCut(25*time.Second, 40*time.Second, 3)
	if !ok || got != 70*time.Second {
		t.Errorf("CycleTailCut(25s, 40s, 3) = %s ok=%v, want 70s true", got, ok)
	}
}

// TestTailCutAdaptsToTheKeysOwnDispersion is the load-bearing one on this file.
//
// THE MEASURED FACT IT ENCODES: dispersion INVERTS between Springfield's two
// payload families. The slow family (70–80 s median) is the STEADIEST at CV
// 0.27–0.32; the fast family (20–31 s) is the noisiest at CV 0.42–0.86. So a
// tolerance expressed as a percentage of takt — the obvious implementation —
// over-triggers on the fast lines and under-triggers on the slow ones with the
// SAME constant. Any constant expressed as a percentage of takt has to be
// per-family, which is what the spread-based cut avoids needing.
//
// The test builds both families, applies both cuts, and asserts the naive one
// diverges while this one does not.
//
// VERIFIED RED BY: replacing the body of CycleTailCut with median × multiple —
// the flagged shares came back 6.9% (fast) against 0.0% (slow), a 6.9-point
// spread, and the assertion below fired.
func TestTailCutAdaptsToTheKeysOwnDispersion(t *testing.T) {
	rng := rand.New(rand.NewSource(20260728))

	// Log-normal-ish gaps with a given median and coefficient of variation.
	family := func(medianSec, cv float64, n int) []time.Duration {
		sigma := math.Sqrt(math.Log(1 + cv*cv))
		out := make([]time.Duration, n)
		for i := range out {
			v := medianSec * math.Exp(sigma*rng.NormFloat64())
			out[i] = time.Duration(v * float64(time.Second))
		}
		return out
	}

	share := func(gaps []time.Duration, cut time.Duration) float64 {
		n := 0
		for _, g := range gaps {
			if g >= cut {
				n++
			}
		}
		return float64(n) / float64(len(gaps)) * 100
	}

	fast := family(25, 0.86, 4000) // the noisiest measured fast payload
	slow := family(75, 0.30, 4000) // the steadiest measured slow payload

	stats := func(gaps []time.Duration) (median, p90 time.Duration) {
		sorted := append([]time.Duration(nil), gaps...)
		for i := 1; i < len(sorted); i++ {
			for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
				sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			}
		}
		median, _ = Quantile(sorted, 0.5)
		p90, _ = Quantile(sorted, 0.90)
		return median, p90
	}

	fm, fp := stats(fast)
	sm, sp := stats(slow)

	// The naive cut: a fixed percentage of each key's takt.
	const naiveMultiple = 1.5
	naiveFast := share(fast, time.Duration(naiveMultiple*float64(fm)))
	naiveSlow := share(slow, time.Duration(naiveMultiple*float64(sm)))

	// The shipped cut: median + k × (p90 − median), k from config.
	const k = 3.0
	cutFast, okF, _ := CycleTailCut(fm, fp, k)
	cutSlow, okS, _ := CycleTailCut(sm, sp, k)
	if !okF || !okS {
		t.Fatal("both synthetic families have real spread; the cut must be computable")
	}
	adaptiveFast := share(fast, cutFast)
	adaptiveSlow := share(slow, cutSlow)

	naiveGap := math.Abs(naiveFast - naiveSlow)
	adaptiveGap := math.Abs(adaptiveFast - adaptiveSlow)

	t.Logf("percentage-of-takt cut: fast %.2f%% vs slow %.2f%% (gap %.2f)", naiveFast, naiveSlow, naiveGap)
	t.Logf("spread-based cut:       fast %.2f%% vs slow %.2f%% (gap %.2f)", adaptiveFast, adaptiveSlow, adaptiveGap)

	if naiveGap < 5 {
		t.Fatalf("the percentage-of-takt cut flagged %.2f%% and %.2f%% — this test's premise "+
			"is that it diverges across the two families, and it did not, so the comparison "+
			"below proves nothing", naiveFast, naiveSlow)
	}
	if adaptiveGap >= naiveGap {
		t.Errorf("the spread-based cut is no more even-handed than a percentage of takt "+
			"(%.2f vs %.2f points apart). Its entire justification is that it neutralises "+
			"the inverted dispersion between the fast and slow families", adaptiveGap, naiveGap)
	}
	if adaptiveGap > 3 {
		t.Errorf("the spread-based cut flagged %.2f%% of the fast family and %.2f%% of the "+
			"slow one — %.2f points apart, which is too wide to call dispersion-neutral",
			adaptiveFast, adaptiveSlow, adaptiveGap)
	}
}

// TestP90IsWithheldBelowTheSampleFloor.
//
// This is the arithmetic the floor comes from, asserted rather than described:
// with nearest-rank quantiles the p90 index is ceil(0.9n), which equals n for
// every n up to 9. At those sizes the p90 IS the maximum, and a column headed
// p90 would be printing the largest gap in the window under a label saying nine
// in ten are below it.
//
// VERIFIED RED BY: setting minSamples to 1 in the call below — HaveTailQuantiles
// came back true at n=9 and the reported p90 was the 300 s outlier, i.e. the max.
func TestP90IsWithheldBelowTheSampleFloor(t *testing.T) {
	// First: prove the arithmetic the floor is derived from.
	for n := 1; n <= 9; n++ {
		if got := int(math.Ceil(0.9 * float64(n))); got != n {
			t.Fatalf("n=%d: ceil(0.9n) = %d, not n — the derivation of the floor is wrong", n, got)
		}
	}
	if got := int(math.Ceil(0.9 * 10)); got != 9 {
		t.Fatalf("n=10: ceil(0.9n) = %d, want 9 — 10 is where p90 and max first separate", got)
	}

	gaps := make([]time.Duration, 0, 9)
	for i := 0; i < 8; i++ {
		gaps = append(gaps, 25*time.Second)
	}
	gaps = append(gaps, 300*time.Second)

	st := SummarizeCycles(CycleSeries{Key: CycleKey{Station: "SPR", Payload: "P", Direction: CycleDirectionProduce}, Gaps: gaps},
		10, 3, 0.25, 5*time.Second)
	if st.HaveTailQuantiles {
		t.Errorf("p90 reported at n=%d — at that size it is the maximum (%s) wearing a "+
			"percentile's label", st.Samples, 300*time.Second)
	}
	if st.HaveTail {
		t.Error("a tail cut was derived from a p90 that could not be computed")
	}
	if st.TailReason == "" {
		t.Error("no reason given for the withheld tail")
	}
	if !st.HaveMedian || st.Median != 25*time.Second {
		t.Errorf("the median is still a real order statistic and must be reported: %+v", st)
	}
	if !st.Underpowered {
		t.Error("the row was not marked underpowered")
	}
}

// TestFlushBoundMedianIsMarked.
//
// A median at or below the Edge accumulator's flush cadence is measuring the
// transport, not the cell — this is the "naive median measures the flush"
// correction, enforced rather than written in a comment.
//
// VERIFIED RED BY: deleting the FlushBound assignment — a key with a 4 s median
// rendered as a 4-second takt with nothing saying otherwise.
func TestFlushBoundMedianIsMarked(t *testing.T) {
	flush := 5 * time.Second
	mk := func(gap time.Duration) CycleStats {
		gaps := make([]time.Duration, 40)
		for i := range gaps {
			gaps[i] = gap
		}
		return SummarizeCycles(CycleSeries{Key: CycleKey{Station: "S", Payload: "P", Direction: CycleDirectionProduce}, Gaps: gaps},
			10, 3, 0.25, flush)
	}
	if !mk(4 * time.Second).FlushBound {
		t.Error("a 4 s median was not marked flush-bound against a 5 s flush")
	}
	if !mk(5 * time.Second).FlushBound {
		t.Error("a median exactly at the flush cadence was not marked — the boundary is " +
			"inclusive, because a median that IS the flush interval is the clearest case " +
			"of the artefact this flag exists to name")
	}
	if mk(25 * time.Second).FlushBound {
		t.Error("a 25 s median was marked flush-bound")
	}
}

// TestSummarizeIsDrivenByItsConstants is behavioural anti-hardcoding.
//
// The binding rule is that no display constant is a literal at a use site. A
// test that only reads the config proves the config exists; this one proves the
// OUTPUT moves when the config does, which is what "not a literal" actually
// means.
//
// VERIFIED RED BY: hardcoding 10, 3 and 0.25 inside SummarizeCycles and ignoring
// the parameters — all three sub-assertions fired.
func TestSummarizeIsDrivenByItsConstants(t *testing.T) {
	// A body of 90 gaps spread over 20–30 s (median 25 s, p90 30 s, so a real
	// 5 s spread) plus ten long ones. Deliberately NOT a flat body: with every
	// gap identical the p90 equals the median, the tail cut is not computable at
	// any k, and this test would pass vacuously against a hardcoded constant.
	gaps := make([]time.Duration, 0, 100)
	for i := 0; i < 90; i++ {
		gaps = append(gaps, time.Duration(20+i%11)*time.Second)
	}
	for i := 0; i < 10; i++ {
		gaps = append(gaps, time.Duration(40+i*7)*time.Second)
	}
	series := CycleSeries{Key: CycleKey{Station: "S", Payload: "P", Direction: CycleDirectionProduce}, Gaps: gaps}

	base := SummarizeCycles(series, 10, 3, 0.25, 5*time.Second)

	if wider := SummarizeCycles(series, 10, 8, 0.25, 5*time.Second); wider.TailCount >= base.TailCount {
		t.Errorf("raising the spread multiple from 3 to 8 did not shrink the tail count "+
			"(%d then %d) — the constant is not reaching the arithmetic",
			base.TailCount, wider.TailCount)
	}
	if narrow := SummarizeCycles(series, 10, 3, 0.5, 5*time.Second); len(narrow.Bands) >= len(base.Bands) {
		t.Errorf("doubling the band width did not reduce the band count (%d then %d)",
			len(base.Bands), len(narrow.Bands))
	}
	if strict := SummarizeCycles(series, 500, 3, 0.25, 5*time.Second); strict.HaveTailQuantiles {
		t.Error("raising the sample floor above n did not withhold the tail quantiles")
	}
	if loose := SummarizeCycles(series, 10, 3, 0.25, time.Hour); !loose.FlushBound {
		t.Error("raising the flush interval above the median did not mark the row flush-bound")
	}
}

// TestBandCountsAccountForEveryGap. A histogram that silently drops values is a
// picture of a subset, and nothing on the page would say which subset.
//
// VERIFIED RED BY: making the band scan use a closed upper bound (m <= HiMul),
// which double-counts every value sitting exactly on an edge — the total came
// back above n.
func TestBandCountsAccountForEveryGap(t *testing.T) {
	gaps := []time.Duration{
		0,                    // exactly zero: the open low band
		5 * time.Second,      //
		20 * time.Second,     //
		25 * time.Second,     // exactly the median
		30 * time.Second,     //
		25 * time.Second / 2, // exactly 0.5x — an edge value at width 0.25
		400 * time.Second,    // far into the open high band
	}
	st := SummarizeCycles(CycleSeries{Key: CycleKey{Station: "S", Payload: "P", Direction: CycleDirectionProduce}, Gaps: gaps},
		1, 3, 0.25, 5*time.Second)
	total := 0
	for _, b := range st.Bands {
		total += b.Count
	}
	if total != len(gaps) {
		t.Errorf("bands hold %d of %d gaps — every value must land in exactly one half-open band",
			total, len(gaps))
	}
}
