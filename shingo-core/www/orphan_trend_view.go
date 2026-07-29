package www

import (
	"fmt"
	"math"
	"time"

	"shingocore/config"
	"shingocore/domain"
)

// orphan_trend_view.go — Stage 5.7's view model: the orphan lane and the trend.
//
// Pure functions of (buckets, window, constants). Same discipline as
// demand_episodes_view.go and for the same reason.
//
// ── WHY A TREND AND NOT A LEVEL ──────────────────────────────────────────────
//
// ListOrphanFindings already answers "how many orphans are there" and the plan
// is explicit that the trend is the number that matters. A level on this bucket
// is nearly uninformative: it is non-empty on any plant that has ever dropped a
// notification, it never shrinks (findings are never deleted), and so it only
// ever climbs. A monotonically rising number is not a signal.
//
// ── WHY A RATE AND NOT A COUNT ───────────────────────────────────────────────
//
// A bare count confounds "more orphans" with "more orders". Doubling output
// doubles orphans at an unchanged failure rate and the count climbs while
// nothing has got worse — which is the wrong direction for a reconciliation
// alarm to be wrong in, because it manufactures the alarm rather than
// suppressing it.
//
// ── AND WHY THE COUNT IS STILL DRAWN ─────────────────────────────────────────
//
// The rate's denominator can be ZERO — a quiet hour, a shutdown, a weekend —
// and the style guide files "the window holds no rows" under NO DATA, not under
// zero. So the rate has buckets it genuinely cannot report, and the guide's own
// rule on lines is that one "asserts continuity between its points, which is a
// claim; use one only where the gap between points is genuinely traversed". A
// rate line would have to either bridge those gaps (a claim the data does not
// support) or break into fragments.
//
// The COUNT has no such hole: a bucket with no orders has no orphans, measured,
// and zero is the honest reading. So the magnitude marks are drawn from the
// count — bars, baseline at zero, no continuity claim — and the RATE is carried
// beside them as a Cell, which is the type that can say "unmeasured" out loud.
// Both are the trend; only one of them can be drawn without lying in the gaps.

// OrphanPoint is one bucket of the trend, fully rendered.
type OrphanPoint struct {
	Start time.Time
	Label string

	// Orphans and Orders are REAL MEASURED COUNTS, including zero. An empty
	// bucket has 0 of each and that is the truth about it — what is unmeasured
	// is the RATE, not the counts.
	Orphans     int
	OrphansText string
	Orders      int
	OrdersText  string

	// Rate is orphans ÷ orders as a whole percent.
	//
	//   value        → measured. INCLUDING 0%, which is the healthy reading and
	//                  must not be dashed out.
	//   value+muted  → measured, but over a denominator below MinBucketOrders,
	//                  so one orphan is dominating it. Shown and greyed; the
	//                  raw count beside it carries the bucket.
	//   nodata       → the bucket holds no orders at all. Not a rate of zero:
	//                  there is nothing to take a share OF.
	Rate Cell

	// BarStep is the count's magnitude, quantised to 0..c.BarSteps, selecting an
	// .or-bar-N class.
	//
	// QUANTISED RATHER THAN A PERCENTAGE, for two reasons that happen to agree.
	// The style guide forbids inline style= in new code, and a continuous height
	// has nowhere else to live. And a bar drawn to the pixel from a count of 3
	// against a count of 4 asserts a resolution the eye cannot read off a
	// 24-pixel-wide column anyway — the same "never print more precision than
	// the measurement supports" rule, applied to a mark instead of a figure.
	//
	// Step 0 is reserved for a count of zero and draws nothing. A bucket with
	// one orphan gets step 1 even when the tallest bucket has two hundred, so
	// "something happened here" never rounds away to "nothing did".
	BarStep int

	// Empty marks a bucket with no orders, so the template can draw the gap as
	// a gap rather than as a zero-height bar indistinguishable from a bucket
	// that ran clean.
	Empty bool
}

// BuildOrphanTrend renders the full bucket series over [since, until).
//
// THE STORE RETURNS ONLY NON-EMPTY BUCKETS — a GROUP BY cannot emit a bucket
// with no rows — so this function GENERATES THE WHOLE SERIES and fills it. That
// is not cosmetic. A trend drawn from only the buckets that had orders silently
// closes its own gaps: three quiet hours vanish and the two live hours either
// side become adjacent, which draws a continuous line across a period nobody
// measured. The missing buckets are the ones a reader most needs to see as
// missing.
func BuildOrphanTrend(buckets []domain.OrphanBucket, since, until time.Time, c config.DisplayConfig) []OrphanPoint {
	width := c.OrphanBucket
	if width <= 0 || !until.After(since) {
		return nil
	}

	// Index what came back by its bucket start, floored identically to the
	// series below so a row and its generated slot agree on which bucket they
	// are.
	//
	// NOT time.Truncate. Truncate floors against the ZERO TIME — 1 January of
	// year 1 — and Postgres floored against the UNIX EPOCH. Those agree only
	// when the gap between the two eras divides evenly by the bucket width, and
	// the gap is 62,135,596,800 seconds: divisible by an hour and by a day, so
	// every width anyone is likely to configure would have worked, and a fifty
	// minute width would not have. THE FAILURE IS TOTAL AND SILENT — every key
	// misses, every bucket renders as "no orders were created", and the page
	// reports a plant that has stopped working while the counts sit unread in
	// the result set. bucketStart floors against the same epoch Postgres used.
	byStart := make(map[int64]domain.OrphanBucket, len(buckets))
	for _, b := range buckets {
		byStart[bucketStart(b.Start, width).Unix()] = b
	}

	maxCount := 0
	for _, b := range buckets {
		if b.Orphans > maxCount {
			maxCount = b.Orphans
		}
	}

	var out []OrphanPoint
	for t := bucketStart(since, width); t.Before(until); t = t.Add(width) {
		b, seen := byStart[t.Unix()]
		p := OrphanPoint{
			Start:       t,
			Label:       t.Format("15:04"),
			Orphans:     b.Orphans,
			OrphansText: FormatCount(b.Orphans),
			Orders:      b.Orders,
			OrdersText:  FormatCount(b.Orders),
		}
		if !seen {
			// A bucket the query did not return had no rows at all. Both counts
			// are genuinely zero; it is the rate that is unmeasured.
			p.Orders, p.Orphans = 0, 0
			p.OrdersText, p.OrphansText = FormatCount(0), FormatCount(0)
		}

		switch {
		case p.Orders <= 0:
			p.Empty = true
			p.Rate = NoData("no orders were created in this bucket, so there is nothing " +
				"for orphans to be a share of — this is an unmeasured rate, not a rate of zero")
		default:
			pct := int(float64(p.Orphans)/float64(p.Orders)*100 + 0.5)
			p.Rate = Value(fmt.Sprintf("%d%%", pct))
			if p.Orders < c.MinBucketOrders {
				p.Rate.Muted = true
				p.Rate.Title = fmt.Sprintf(
					"only %s orders in this bucket — below the minimum denominator of %s, so "+
						"one orphan moves this rate by %s points. Read the orphan count instead.",
					FormatCount(p.Orders), FormatCount(c.MinBucketOrders),
					FormatCount(int(100.0/float64(p.Orders)+0.5)))
			}
		}

		p.BarStep = barStep(p.Orphans, maxCount, c.BarSteps)
		out = append(out, p)
	}
	return out
}

// bucketStart floors a time to its bucket's left edge, against the UNIX EPOCH.
//
// This has to reproduce Postgres' floor(extract(epoch FROM ...) / n) * n
// EXACTLY, because the two results are matched by equality in a map. Go's
// time.Truncate looks like the same operation and is not: it floors against the
// zero time (year 1), which is 62,135,596,800 seconds earlier. The two agree
// only for widths that divide that gap.
func bucketStart(t time.Time, width time.Duration) time.Time {
	secs := int64(width / time.Second)
	if secs <= 0 {
		return t.UTC()
	}
	e := t.UTC().Unix()
	// Floor, not truncate-toward-zero: pre-1970 timestamps are not reachable
	// here, but a division that rounds the wrong way for negatives is a trap
	// left for whoever first backfills historical data.
	q := e / secs
	if e < 0 && e%secs != 0 {
		q--
	}
	return time.Unix(q*secs, 0).UTC()
}

// barStep quantises a count against the tallest bucket in the series.
//
// CEIL, NOT TRUNCATE, and a floor of 1 for any non-zero count. Truncating would
// give step 0 — which draws nothing at all — to every bucket below one step's
// worth of the maximum, so a day with one orphan beside a day with two hundred
// would render as a day with none. "Nothing happened here" is a claim the data
// does not make, and it is the one claim this chart must never make by
// accident. Same reasoning as RampStep, which is the other quantiser on this
// surface family.
func barStep(count, max, steps int) int {
	if count <= 0 || max <= 0 || steps <= 0 {
		return 0
	}
	step := int(math.Ceil(float64(count) / float64(max) * float64(steps)))
	if step < 1 {
		step = 1
	}
	if step > steps {
		step = steps
	}
	return step
}

// OrphanLane is the per-site half of 5.7, rendered.
type OrphanLane struct {
	Sites []OrphanSiteRow

	// LiveTotal and AgedTotal are plant-wide. Real measured counts; zero live
	// orphans is the healthy reading and prints as 0.
	LiveTotal int
	AgedTotal int
	Total     int
}

// OrphanSiteRow is one station's lane row.
type OrphanSiteRow struct {
	StationID string

	Live     int
	LiveText string
	Aged     int
	AgedText string
	Total    int

	// OldestLive is how long the oldest still-live finding has been sitting.
	//
	//   value  → there is a live finding and this is its age.
	//   na     → there are no live findings. THE QUESTION DOES NOT APPLY, which
	//            is a different claim from "we do not know how old it is", and a
	//            station that has been cleaned up must not read like a station
	//            whose timestamps went missing.
	OldestLive Cell
}

// BuildOrphanLane renders the per-site summary.
//
// `now` is a parameter and never time.Now(): the oldest-live age depends on it,
// and a function that reads the clock internally cannot be tested at a
// boundary. Same rule BuildEpisodeRow follows.
func BuildOrphanLane(sites []domain.OrphanSite, now time.Time) OrphanLane {
	lane := OrphanLane{}
	for _, s := range sites {
		row := OrphanSiteRow{
			StationID: s.StationID,
			Live:      s.Live,
			LiveText:  FormatCount(s.Live),
			Aged:      s.Aged,
			AgedText:  FormatCount(s.Aged),
			Total:     s.Total,
		}
		switch {
		case s.OldestLive == nil:
			// NOT no-data. There is no live finding, so there is no age to have
			// heard about — the question has not got a subject.
			row.OldestLive = NA("no live findings at this station")
		default:
			age := now.Sub(*s.OldestLive)
			if age < 0 {
				age = 0
			}
			row.OldestLive = Value(FormatDuration(age))
		}
		lane.Sites = append(lane.Sites, row)
		lane.LiveTotal += s.Live
		lane.AgedTotal += s.Aged
		lane.Total += s.Total
	}
	return lane
}
