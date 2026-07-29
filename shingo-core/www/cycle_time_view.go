package www

import (
	"fmt"
	"math"
	"sort"
	"time"

	"shingocore/config"
	"shingocore/domain"
)

// cycle_time_view.go — the cycle-time surface's view model (Stage 5.10).
//
// Pure functions of (stats, constants), same discipline as
// demand_episodes_view.go, and it reuses that file's Cell / Value / NoData / NA
// vocabulary rather than inventing a second one. The three absence states are
// the same three absence states everywhere on this branch or they are not a
// doctrine.
//
// WHAT THE STYLE GUIDE ASSIGNS THIS SURFACE, verbatim: "Distribution per (node,
// payload); median annotated ON it, never alone. The tail is the
// material-downtime signal." Three obligations, and each one is a thing this
// file does rather than a thing it aspires to:
//
//   - DISTRIBUTION, not a single number. Every row carries its own histogram.
//   - MEDIAN ANNOTATED ON IT. The band holding the median is marked in the
//     picture; the printed p50 beside it is a second reading, not the only one.
//   - THE TAIL IS THE SIGNAL. It gets its own column, and it reports NOT
//     COMPUTABLE rather than zero whenever the cut could not be derived.
//
// AND NODE IS NOT AVAILABLE. See audit.ListCycleEvents: the truth-path INSERT
// populates neither node_id nor the station column, so the grain is
// (station, payload, direction). That is stated on the page, not papered over.

// ── Precision ────────────────────────────────────────────────────────────────

// FormatCycleDuration renders a cycle time at the precision the source supports.
//
// ROUNDS, rather than truncating to the second the way FormatDuration does on
// its own. That matters here specifically: a 24.996 s median — a real measured
// Springfield value — truncates to "24 s" and reads as a different mode from the
// 25 s one beside it, which is exactly the false discrimination the style guide's
// precision rule exists to prevent. Rounding first puts both in the same place.
//
// Nothing finer than a second is ever printed. The interval is a difference
// between two service clocks, quantised by Edge's five-second accumulator flush;
// "24.996 s" asserts a millisecond-accurate measurement from a source whose
// resolution is five seconds, and an engineer who reads it will chase a
// fractional regression that is entirely quantisation noise.
func FormatCycleDuration(d time.Duration) string {
	return FormatDuration(d.Round(time.Second))
}

// FormatPercent renders a share at whole percent — the style guide's precision
// for a percentage. Rounded, not truncated: 0.6% must not print as 0% on a page
// whose whole subject is a small number climbing.
func FormatPercent(pct float64) string {
	return fmt.Sprintf("%d%%", int(math.Round(pct)))
}

// ── The row ──────────────────────────────────────────────────────────────────

// CycleBandCell is one histogram band, rendered.
type CycleBandCell struct {
	// Label is the band's range in multiples of the key's own median, e.g.
	// "0.9–1.1×". The top band reads "2.1×+".
	Label string

	Count     int
	CountText string

	// BarPct is the band's height as a percentage of the tallest band, for the
	// inline --ct-bar custom property. A share of the TALLEST band, not of n:
	// on a distribution with 57% of its mass in one bucket, scaling to n leaves
	// every other band invisible and the shape unreadable.
	BarPct string

	// HoldsMedian marks the band the median falls in. This is the annotation the
	// style guide requires — on the distribution, not beside it.
	HoldsMedian bool

	// IsTail marks the open-ended top band. Never colour alone: the template
	// prints the band's name too.
	IsTail bool

	Title string
}

// CycleRow is one (station, payload, direction) line, fully rendered.
type CycleRow struct {
	Station   string
	Payload   string
	Direction string
	DirLabel  string

	Samples     int
	SamplesText string

	// Span is how long the observed window actually was for this key. Printed
	// because 40 samples over ten minutes and 40 over a week are the same n and
	// nothing like the same claim.
	SpanText string

	// Median is the takt. Muted — not absent — when the sample count is below
	// the floor: the value is real, it is just not load-bearing.
	Median Cell

	// P90 and P99 are no-data below the sample floor, because below it the
	// nearest-rank p90 IS the maximum and printing it would be labelling the
	// largest gap as a ninetieth percentile.
	P90 Cell
	P99 Cell

	// Tail is the material-downtime signal (5.10's stated purpose). NO DATA when
	// the cut could not be derived — never zero. "No long cycles" and "we had no
	// basis to look" are different claims and the second is the interesting one.
	TailCount Cell
	TailShare Cell
	TailCut   Cell

	// TailShareSort ranks the page. NaN for rows the tail cannot rank.
	TailShareSort float64

	Bands     []CycleBandCell
	HaveBands bool

	// FlushBound is a key whose median is at or below Edge's accumulator flush
	// interval — the row is measuring the transport, not the cell. Carried as a
	// PRINTED NOTE as well as a class, because a reader must not have to know
	// what a grey row means.
	FlushBound bool
	FlushNote  string

	// Underpowered is the small-n case. Same rule: printed, not only styled.
	Underpowered bool
	SampleNote   string

	// SortGroup floats the rows the tail cannot rank above the ranked body.
	SortGroup int
}

// Sort groups. Lower sorts first.
const (
	// cycleGroupUnreadable — the row's median is at or below the flush cadence,
	// so nothing on it is a cycle time. Floated to the top because it is a
	// FINDING ABOUT THE FEED, and a finding about the feed buried under sixty
	// ranked rows is a finding nobody reads. This is the same argument 5.1 makes
	// for zero-order episodes, applied to a different unrankable class.
	cycleGroupUnreadable = 0

	// cycleGroupNoTail — the tail could not be derived (too few samples, or no
	// measurable spread). Sorting these below the ranked body would assert they
	// are calmer than every row above them, which is not something the data says.
	cycleGroupNoTail = 1

	// cycleGroupRanked — everything the tail share can actually order.
	cycleGroupRanked = 2
)

// DirectionLabel names a cycle direction for display.
//
// THE DEFAULT RENDERS THE VALUE AS ITSELF, for the same reason every other
// vocabulary switch on this branch does: protocol.BinUOPDeltaReason has grown
// (ab_fallthrough, capture_reduction, operator_correction were all added after
// the first two), and a default returning "Other" would turn the next addition
// into silent data loss on the one page whose job is to notice things.
func DirectionLabel(dir string) string {
	switch dir {
	case domain.CycleDirectionProduce:
		return "Produce"
	case domain.CycleDirectionConsume:
		return "Consume"
	case "":
		return ""
	default:
		return humanizeUnknown(dir)
	}
}

// BuildCycleRow renders one key's statistics into its display form.
func BuildCycleRow(s domain.CycleStats, c config.DisplayConfig) CycleRow {
	row := CycleRow{
		Station:       s.Key.Station,
		Payload:       s.Key.Payload,
		Direction:     s.Key.Direction,
		DirLabel:      DirectionLabel(s.Key.Direction),
		Samples:       s.Samples,
		SamplesText:   FormatCount(s.Samples),
		FlushBound:    s.FlushBound,
		Underpowered:  s.Underpowered,
		TailShareSort: math.NaN(),
		SortGroup:     cycleGroupRanked,
	}

	if !s.First.IsZero() && !s.Last.IsZero() {
		row.SpanText = FormatDuration(s.Last.Sub(s.First))
	}

	// ── Median ───────────────────────────────────────────────────────────────
	if !s.HaveMedian {
		// NOT NO-DATA, and the difference is the whole point of having three
		// states. A key reaches here with exactly one tick observed — a series is
		// only created for a key that had at least one event, and one event yields
		// zero intervals. Nothing is missing and nothing failed: an interval needs
		// two events, so the question has not been asked yet. Same shape as an open
		// episode having no close reason.
		row.Median = NA("only one tick observed for this key — an interval needs two")
	} else {
		row.Median = Value(FormatCycleDuration(s.Median))
		if s.Underpowered {
			// Muted, not absent: the median of six gaps is a real order statistic.
			// It is simply not a takt, and the sample count beside it says so.
			row.Median.Muted = true
			row.Median.Title = fmt.Sprintf(
				"%s cycles — below the minimum of %s, so this median is a reading and not a takt.",
				FormatCount(s.Samples), FormatCount(c.CycleMinSamples))
		}
	}

	if s.FlushBound {
		row.SortGroup = cycleGroupUnreadable
		row.FlushNote = fmt.Sprintf(
			"median at or below the %s Edge delta flush — this row is measuring the transport, "+
				"not the cell", FormatDuration(c.CycleFlushInterval))
	}
	if s.Underpowered {
		row.SampleNote = fmt.Sprintf("fewer than %s cycles", FormatCount(c.CycleMinSamples))
	}

	// ── p90 / p99 ────────────────────────────────────────────────────────────
	if !s.HaveTailQuantiles {
		why := fmt.Sprintf(
			"%s cycles is below the minimum of %s — at that sample size the nearest-rank p90 "+
				"is simply the largest gap observed, so reporting it would label a maximum as a "+
				"ninetieth percentile",
			FormatCount(s.Samples), FormatCount(c.CycleMinSamples))
		row.P90 = NoData(why)
		row.P99 = NoData(why)
	} else {
		row.P90 = Value(FormatCycleDuration(s.P90))
		row.P99 = Value(FormatCycleDuration(s.P99))
	}

	// ── The tail: the material-downtime signal ───────────────────────────────
	if !s.HaveTail {
		why := s.TailReason
		if why == "" {
			why = "the tail cut could not be derived for this key"
		}
		// NOT ZERO. "No cycles past the cut" and "there is no cut" are different
		// claims, and a zero here would read as the reassuring one on exactly the
		// rows where nothing is known.
		row.TailCount = NoData(why)
		row.TailShare = NoData(why)
		row.TailCut = NoData(why)
		if row.SortGroup == cycleGroupRanked {
			row.SortGroup = cycleGroupNoTail
		}
	} else {
		row.TailCut = Value(FormatCycleDuration(s.TailCut))
		row.TailCut.Title = fmt.Sprintf(
			"median %s + %g × (p90 %s − median) — the spread is taken from this key's own "+
				"history, so the cut widens on a noisy line and tightens on a steady one",
			FormatCycleDuration(s.Median), c.CycleSpreadMultiple, FormatCycleDuration(s.P90))
		// A measured zero IS a value here — this key ran the whole window with
		// nothing past its own cut, which is the good news and must be printable.
		row.TailCount = Value(FormatCount(s.TailCount))
		row.TailShare = Value(FormatPercent(s.TailShare))
		row.TailShareSort = s.TailShare
	}

	row.Bands, row.HaveBands = buildCycleBands(s)
	return row
}

// barMinPct is the floor on a NON-EMPTY band's rendered height.
//
// THIS IS THE ABSENCE-AS-ZERO BUG IN THE PICTURE, and it is easy to ship. On a
// distribution where one band holds 30% of 3,000 cycles, a band holding one
// cycle scales to 0.1% and rounds to 0 — so it draws at exactly the same height
// as a band holding nothing, and the reader is told the tail is empty on the row
// where a single 40-minute stop is the whole finding. A non-empty band must be
// visibly non-empty; below this floor the bar stops being proportional and says
// only "some", which is the true statement at that resolution.
const barMinPct = 3

func barPercent(count, max int) string {
	if count <= 0 || max <= 0 {
		return "0%"
	}
	pct := int(math.Round(float64(count) / float64(max) * 100))
	if pct < barMinPct {
		pct = barMinPct
	}
	return fmt.Sprintf("%d%%", pct)
}

// buildCycleBands renders the histogram, scaled to its own tallest band.
func buildCycleBands(s domain.CycleStats) ([]CycleBandCell, bool) {
	if !s.HaveBands || len(s.Bands) == 0 {
		return nil, false
	}
	max := 0
	for _, b := range s.Bands {
		if b.Count > max {
			max = b.Count
		}
	}
	if max == 0 {
		// Every band empty with bands defined at all should not happen, but a
		// division by zero here would render as a blank strip that looks like a
		// measured flat distribution.
		return nil, false
	}

	out := make([]CycleBandCell, 0, len(s.Bands))
	for _, b := range s.Bands {
		cell := CycleBandCell{
			Count:       b.Count,
			CountText:   FormatCount(b.Count),
			BarPct:      barPercent(b.Count, max),
			HoldsMedian: b.HoldsMedian,
			IsTail:      b.OpenHigh,
		}
		switch {
		case b.OpenLow:
			cell.Label = fmt.Sprintf("<%.2g×", b.HiMul)
			cell.Title = fmt.Sprintf("under %s — %s cycles",
				FormatCycleDuration(b.Hi), cell.CountText)
		case b.OpenHigh:
			cell.Label = fmt.Sprintf("%.2g×+", b.LoMul)
			cell.Title = fmt.Sprintf("%s and over — %s cycles",
				FormatCycleDuration(b.Lo), cell.CountText)
		default:
			cell.Label = fmt.Sprintf("%.2g–%.2g×", b.LoMul, b.HiMul)
			cell.Title = fmt.Sprintf("%s to %s — %s cycles",
				FormatCycleDuration(b.Lo), FormatCycleDuration(b.Hi), cell.CountText)
		}
		if b.HoldsMedian {
			cell.Title += " · holds the median"
		}
		out = append(out, cell)
	}
	return out, true
}

// SortCycleRows orders the page by tail share, worst first, with the rows the
// tail cannot rank floated ABOVE the ranked body rather than buried below it.
//
// A plain ORDER BY tail_share DESC would put every unrankable row at the bottom,
// below every healthy cell on the floor — and the unrankable rows here are the
// two most interesting states this page has: a key whose median is the flush
// cadence (the feed, not the cell) and a key with too few cycles to say anything
// about (a line that has stopped producing, or a payload that just started).
// Sorting them last asserts they are calmer than everything above them, which is
// not something the data says. Same argument 5.1 makes for zero-order episodes.
//
// Ties break on sample count, largest first, so two keys the tail cannot
// separate are separated by how much evidence each carries.
func SortCycleRows(rows []CycleRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.SortGroup != b.SortGroup {
			return a.SortGroup < b.SortGroup
		}
		if a.SortGroup == cycleGroupRanked && a.TailShareSort != b.TailShareSort {
			return a.TailShareSort > b.TailShareSort
		}
		return a.Samples > b.Samples
	})
}
