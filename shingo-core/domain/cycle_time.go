package domain

import (
	"math"
	"sort"
	"time"

	"shingo/protocol"
)

// cycle_time.go — cycle time from the BinUOPDelta truth path (Stage 5.10).
//
// The queries live in store/audit/cycle_time.go. Everything here is a PURE
// FUNCTION of (events, constants): no database, no clock, no template. The
// rules below are the ones most likely to be got wrong and least likely to be
// noticed, so they have to be testable without Postgres.
//
// ── WHAT A CYCLE IS HERE ─────────────────────────────────────────────────────
//
// One interval between consecutive produce (or consume) ticks at the same
// place, for the same part. The source is bin_uop_audit rows written by
// uop.(*InventoryDeltaService).ApplyBinUOPDelta — the path that actually moved
// bins.uop_remaining. cell_part_events is NOT used, and the usual reason given
// (at-most-once delivery) is not the disqualifier: its payload_code is EMPTY ON
// EVERY ROW, so it cannot attribute a cycle to a part number at all.
//
// ── THE PARTITION IS THE MEASUREMENT ─────────────────────────────────────────
//
// A naive median over the whole audit stream reads 4.99 s at Springfield. That
// is not a cycle — it is the Edge accumulator's five-second flush cadence
// (shingo-edge/uop/accumulator.go, defaultInventoryDeltaInterval) seen through a
// stream that interleaves every bin on the site. Partitioning by
// (station, payload, direction) BEFORE differencing is what turns the stream
// into a takt; it is not an aggregation convenience. Everything below assumes
// the partition has already happened, which is why BuildCycleSeries is the only
// way to get a gap.
//
// ── NO DEDUP PASS, DELIBERATELY ──────────────────────────────────────────────
//
// The plan specifies one ("~1,779/day stale-epoch drops + ~1,779/day replays
// produce phantom gaps"). It must not be built, for two independent reasons:
//
//  1. The two figures are ONE POPULATION COUNTED TWICE. The stale-epoch branch
//     logs "BinUOPDelta stale epoch DROPPED" and then returns
//     ErrInventoryDeltaSkipped, whose caller logs "replay — already applied".
//     Every drop emits both lines, so the counts are necessarily equal, and
//     adding them invents a replay population that does not exist.
//
//  2. A GENUINE REPLAY WRITES NO AUDIT ROW AT ALL. The applier returns before
//     the INSERT when the dedup sequence has already been consumed. Replays are
//     therefore unobservable in bin_uop_audit BY CONSTRUCTION, and a dedup pass
//     over rows that are all first applications would discard real events and
//     manufacture the very gaps it was meant to remove.
//
// Dropped deltas DO leave a hole in the series — that is real, and it is part of
// what the tail measures. It is not fixable by deduplication.

// ── The grain ────────────────────────────────────────────────────────────────

// Cycle directions, DERIVED FROM THE WIRE VOCABULARY rather than restated. A
// second spelling of "produce_tick" in this package would be a string that
// drifts silently the day the protocol renames one, and the drift would show up
// as an empty page rather than as a compile error.
//
// ONLY THESE TWO ARE CYCLES. capture_reduction is an operator pulling parts,
// operator_correction is an admin fixing a count, and ab_fallthrough is a
// carrier artefact — none of them is a part crossing a station on its own
// cadence, and mixing any of them in would put a human decision inside a
// machine's takt.
const (
	CycleDirectionProduce = string(protocol.ReasonProduceTick)
	CycleDirectionConsume = string(protocol.ReasonConsumeTick)
)

// CycleKey is what a distribution is drawn per.
//
// STATION, NOT NODE — and this is a gap worth knowing about. The style guide
// assigns 5.10 a distribution per (node, payload). The truth-path INSERT in
// uop.applier writes neither node_id nor the station column: it names
// (bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata)
// and puts the station string in ACTOR. So node is not recoverable from this
// stream for delta rows, historically or going forward, and the honest grain is
// the one below.
type CycleKey struct {
	Station string
	Payload string

	// Direction is produce_tick or consume_tick. It is part of the KEY, not a
	// filter: a press filling a tote and a cell draining one are two different
	// processes with two different takts, and interleaving them halves the
	// apparent cycle time of both.
	Direction string
}

// CycleEvent is one truth-path delta row reduced to what a cycle needs.
type CycleEvent struct {
	CycleKey
	At time.Time
}

// CycleSeries is one key's gaps, in the order they happened.
type CycleSeries struct {
	Key  CycleKey
	Gaps []time.Duration

	// First and Last bound the observed span. Carried so a reader can tell a key
	// with 40 samples over ten minutes from one with 40 over a week — the same
	// n, and nothing like the same claim.
	First, Last time.Time
}

// BuildCycleSeries partitions events by key and differences each partition.
//
// Returns the series plus the number of events it could not attribute. An event
// with a blank payload code is NOT silently dropped into an "unknown" bucket and
// NOT folded into a neighbouring key: it is counted and reported, because the
// number of rows the page could not use is a finding about the ingest path and
// the page has to be able to say it. Blank payloads are real — the first-delta
// identity bind fires on a produce bin at exactly zero count.
//
// A GAP NEVER CROSSES A KEY BOUNDARY. That is the one invariant here that a
// SQL-side LAG() would also give and that a careless Go loop over a globally
// sorted slice would silently break, which is exactly why the differencing is in
// a tested function rather than inline in a handler.
func BuildCycleSeries(events []CycleEvent) (series []CycleSeries, unattributable int) {
	byKey := map[CycleKey][]time.Time{}
	for _, e := range events {
		if e.Payload == "" || e.Station == "" || e.Direction == "" {
			unattributable++
			continue
		}
		byKey[e.CycleKey] = append(byKey[e.CycleKey], e.At)
	}

	series = make([]CycleSeries, 0, len(byKey))
	for k, times := range byKey {
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		s := CycleSeries{Key: k, First: times[0], Last: times[len(times)-1]}
		for i := 1; i < len(times); i++ {
			d := times[i].Sub(times[i-1])
			if d < 0 {
				// Two service clocks disagreeing. Clamp rather than carry a
				// negative into a quantile, where it would sort below every real
				// value and drag the low end of the distribution with it.
				d = 0
			}
			s.Gaps = append(s.Gaps, d)
		}
		series = append(series, s)
	}
	// Deterministic order in, deterministic order out — the caller sorts for
	// display, but a map iteration must never be the thing that decides it.
	sort.Slice(series, func(i, j int) bool {
		a, b := series[i].Key, series[j].Key
		if a.Station != b.Station {
			return a.Station < b.Station
		}
		if a.Payload != b.Payload {
			return a.Payload < b.Payload
		}
		return a.Direction < b.Direction
	})
	return series, unattributable
}

// ── Quantiles ────────────────────────────────────────────────────────────────

// Quantile returns the nearest-rank quantile of an ascending-sorted slice.
//
// NEAREST RANK, NOT INTERPOLATION, and the reason is in this data specifically.
// Springfield's cycle times are strongly trimodal: 25 s, 30 s and 20 s account
// for roughly 57% of all intervals in three one-second bands. Linear
// interpolation between two order statistics straddling those modes returns a
// value like 27.3 s — a duration that essentially never occurs — and prints it
// as though it had been observed. An order statistic is always a measurement
// that actually happened, which is the only kind of number this page should
// contain.
//
// The bool is not decoration: a quantile of an empty slice has no value, and
// returning a zero Duration for it would be the coalesce-absence-into-zero bug
// at the arithmetic layer instead of the display layer.
func Quantile(sorted []time.Duration, q float64) (time.Duration, bool) {
	n := len(sorted)
	if n == 0 || q <= 0 || q > 1 {
		return 0, false
	}
	rank := int(math.Ceil(q * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1], true
}

// ── The distribution ─────────────────────────────────────────────────────────

// CycleBand is one bucket of the distribution, in MULTIPLES OF THE KEY'S OWN
// MEDIAN rather than in absolute seconds.
//
// WHY RELATIVE. There is no single Springfield takt; there are at least two
// families. Eighteen payloads run a 20–31 s median and three run 70–80 s. One
// absolute bucket set renders the fast family inside two buckets and smears the
// slow family across the whole axis, so neither shape is readable and the two
// cannot be compared. Normalising each key by its own median puts every
// distribution on one axis where 1.0 means "this key's normal", which is the
// comparison a reader of this page is actually making.
//
// AND NEVER INTEGER BUCKETING. Only 29 of 219,465 measured intervals are exact
// multiples of five seconds — the modes are 24.995826 and 29.999909, not 25 and
// 30. Anything that rounds, truncates or tests equality finds nothing. Bands are
// half-open float ranges for exactly that reason.
type CycleBand struct {
	// LoMul and HiMul are the band's edges as multiples of the key's median.
	// HiMul is +Inf on the top band. The range is half-open: [LoMul, HiMul).
	LoMul, HiMul float64

	// Lo and Hi are the same edges in wall time, for the tooltip. Hi is zero
	// when HiMul is infinite; OpenHigh says so rather than a sentinel duration.
	Lo, Hi time.Duration

	Count int

	// HoldsMedian marks the band containing 1.0. THE MEDIAN IS ANNOTATED ON THE
	// DISTRIBUTION, which is what the style guide asks for and what a median
	// printed only in a neighbouring column does not do.
	HoldsMedian bool

	OpenLow, OpenHigh bool
}

// CycleStats is one key's fully-computed distribution.
//
// Every "Have" flag is load-bearing. A statistic that could not be computed is
// NOT reported as zero: a p90 of zero and a p90 we had no basis to compute are
// different claims, and the second one is the interesting one on the day the
// feed breaks.
type CycleStats struct {
	Key     CycleKey
	Samples int

	First, Last time.Time

	// Median is the takt. HaveMedian is false only with no samples at all.
	Median     time.Duration
	HaveMedian bool

	// Underpowered marks a median computed from fewer than minSamples gaps. The
	// value is real and is still shown — it is just not load-bearing, which is a
	// different claim from absent.
	Underpowered bool

	// P90 and P99 need minSamples. Below that the nearest-rank p90 IS the
	// maximum (for n <= 9, ceil(0.9n) == n), so reporting it would be printing
	// the largest observed gap under a label saying nine in ten are below it.
	P90, P99          time.Duration
	HaveTailQuantiles bool

	// Spread is P90 - Median: a ROBUST dispersion estimate taken from the key's
	// own history, with no mean anywhere in it. See CycleTailCut.
	Spread     time.Duration
	HaveSpread bool

	// TailCut is where a gap stops being a slow cycle and starts being a stop.
	// TailCount and TailShare are the gaps at or past it.
	TailCut   time.Duration
	TailCount int
	TailShare float64
	HaveTail  bool

	// TailReason says WHY there is no tail figure, when there is none. It is the
	// text a no-data cell needs; an absence with no stated reason is the shape
	// the style guide forbids.
	TailReason string

	// FlushBound marks a key whose median is at or below the Edge accumulator's
	// flush interval. Such a median is measuring the transport, not the cell, and
	// the row says so in words rather than being quietly rendered as a takt.
	FlushBound bool

	Bands     []CycleBand
	HaveBands bool
}

// CycleTailCut is where the tail begins, for one key: Median + k × (P90 − Median).
//
// ── WHY NOT A PERCENTAGE OF TAKT ─────────────────────────────────────────────
//
// Because dispersion INVERTS between Springfield's two families, so a relative
// tolerance mis-fires at both ends. The slow family is the STEADIEST (CV
// 0.27–0.32); the fast family is the noisiest (CV 0.42–0.86). A cut at
// "median × 1.5" sits at roughly median + 0.8σ on a fast payload — flagging
// something like a fifth of all cycles — and at median + 1.7σ on a slow one,
// flagging almost nothing. Same constant, opposite failure at each end.
//
// The (P90 − Median) term is the fix, and it is not a trick: it is a dispersion
// estimate taken FROM THE KEY'S OWN HISTORY, so the cut widens on a noisy line
// and tightens on a steady one without anybody keying a constant per payload.
// This is why the constant can honestly stay a single scalar.
//
// ── AND WHY NOT A QUANTILE ───────────────────────────────────────────────────
//
// "Flag the slowest 5% of this key's cycles" is dispersion-adaptive too, and it
// is useless: it flags exactly 5% of every key by construction, so the count
// carries no information at all. A tail number has to be able to read zero on a
// good line and climb on a bad one.
//
// ── THE DEGENERATE CASE IS NOT ZERO ──────────────────────────────────────────
//
// When P90 == Median the key has no measurable dispersion in this window and the
// cut collapses onto the median, which would flag about half of all cycles. That
// is reported as NOT COMPUTABLE, never as "no long cycles" — a check must know
// whether it had the input to check.
func CycleTailCut(median, p90 time.Duration, multiple float64) (time.Duration, bool, string) {
	if median <= 0 {
		return 0, false, "the median cycle is zero, so there is no scale to measure a tail against"
	}
	if multiple <= 0 {
		return 0, false, "the configured spread multiple is not positive"
	}
	spread := p90 - median
	if spread <= 0 {
		return 0, false, "p90 and the median are the same value in this window, so this key has " +
			"no measurable spread — a tail cut derived from it would flag about half of all cycles"
	}
	return median + time.Duration(multiple*float64(spread)), true, ""
}

// CycleBands builds the band edges for one key.
//
// SYMMETRIC ABOUT THE MEDIAN, and that is structural rather than chosen: a
// distribution drawn in units of its own median is only readable if 1.0 sits at
// the CENTRE of a band rather than on an edge, and durations cannot be negative,
// so the window is 1.0 ± 1.0 and the number of bands each side of centre follows
// from the width alone. TestCycleBandsAreSymmetricAboutTheMedian holds it.
//
// Tiling from zero instead would put 1.0 on an edge, and at width 0.25 that
// alone would merge two of Springfield's three modes (25 s and 30 s both land in
// [1.00,1.25) when the edges are 0, 0.25, 0.50 …) — the trimodality would
// disappear into the bucketing.
//
// Width is the ONE free number. At 0.25 the three modes land in three different
// bands: 20/25 = 0.8 in [0.625,0.875), 25/25 = 1.0 in [0.875,1.125), and
// 30/25 = 1.2 in [1.125,1.375).
func CycleBands(median time.Duration, width float64) []CycleBand {
	if median <= 0 || width <= 0 {
		return nil
	}
	half := width / 2

	mul := func(m float64) time.Duration { return time.Duration(m * float64(median)) }

	// Degenerate: a band so wide its lower edge is already at or below zero. One
	// centre band and one overflow; still symmetric in the only sense available,
	// since there is nothing below zero to be symmetric with.
	if 1-half <= 0 {
		return []CycleBand{
			{LoMul: 0, HiMul: 1 + half, Lo: 0, Hi: mul(1 + half), HoldsMedian: true, OpenLow: true},
			{LoMul: 1 + half, HiMul: math.Inf(1), Lo: mul(1 + half), OpenHigh: true},
		}
	}

	// How many whole bands fit between the centre band's lower edge and zero.
	steps := 0
	for lo := 1 - half; lo-width > 0; lo -= width {
		steps++
	}

	bottom := 1 - half - float64(steps)*width // lower edge of the lowest middle band
	total := 2*steps + 1                      // steps below centre, centre, steps above

	bands := make([]CycleBand, 0, total+2)
	bands = append(bands, CycleBand{
		LoMul: 0, HiMul: bottom, Lo: 0, Hi: mul(bottom), OpenLow: true,
	})
	for i := 0; i < total; i++ {
		lo := bottom + float64(i)*width
		hi := lo + width
		bands = append(bands, CycleBand{
			LoMul: lo, HiMul: hi, Lo: mul(lo), Hi: mul(hi),
			HoldsMedian: lo <= 1 && 1 < hi,
		})
	}
	top := bottom + float64(total)*width
	bands = append(bands, CycleBand{
		LoMul: top, HiMul: math.Inf(1), Lo: mul(top), OpenHigh: true,
	})
	return bands
}

// SummarizeCycles computes one key's whole distribution.
//
// minSamples, spreadMultiple, bandWidth and flushInterval all arrive as
// parameters. THIS PACKAGE HOLDS NO DISPLAY CONSTANT: every one of them is
// config with a recorded provenance note (config/provenance.go), and a default
// baked in here would be a literal at a use site with nowhere to attach that
// record to. Passing them in is also what makes the behavioural anti-hardcoding
// tests possible — retuning a constant has to change the output.
func SummarizeCycles(s CycleSeries, minSamples int, spreadMultiple, bandWidth float64, flushInterval time.Duration) CycleStats {
	st := CycleStats{
		Key:     s.Key,
		Samples: len(s.Gaps),
		First:   s.First,
		Last:    s.Last,
	}
	if st.Samples == 0 {
		st.TailReason = "no cycles observed for this key in the window"
		return st
	}

	sorted := make([]time.Duration, len(s.Gaps))
	copy(sorted, s.Gaps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	st.Median, st.HaveMedian = Quantile(sorted, 0.5)
	st.Underpowered = st.Samples < minSamples
	st.FlushBound = st.HaveMedian && flushInterval > 0 && st.Median <= flushInterval

	if st.Underpowered {
		st.TailReason = "too few cycles in this window for a p90 to mean anything — below the " +
			"minimum, the nearest-rank p90 is simply the largest gap observed"
	} else {
		st.P90, _ = Quantile(sorted, 0.90)
		st.P99, _ = Quantile(sorted, 0.99)
		st.HaveTailQuantiles = true

		st.Spread = st.P90 - st.Median
		st.HaveSpread = st.Spread > 0

		st.TailCut, st.HaveTail, st.TailReason = CycleTailCut(st.Median, st.P90, spreadMultiple)
		if st.HaveTail {
			for _, g := range sorted {
				if g >= st.TailCut {
					st.TailCount++
				}
			}
			st.TailShare = float64(st.TailCount) / float64(st.Samples) * 100
		}
	}

	if st.HaveMedian && st.Median > 0 {
		st.Bands = CycleBands(st.Median, bandWidth)
		st.HaveBands = len(st.Bands) > 0
		for _, g := range sorted {
			m := float64(g) / float64(st.Median)
			for i := range st.Bands {
				if m >= st.Bands[i].LoMul && m < st.Bands[i].HiMul {
					st.Bands[i].Count++
					break
				}
			}
		}
	}

	return st
}
