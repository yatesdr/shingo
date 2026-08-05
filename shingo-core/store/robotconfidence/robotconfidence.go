// Package robotconfidence collects and aggregates SEER localization
// confidence (rbk_report.confidence, 0.0–1.0) sampled off Core's existing
// 2-second /robotsStatus poll.
//
// This file holds the analytical math and the geometry — all pure functions,
// no database. store.go is the SQL shell and rollup.go orchestrates the
// nightly job. Same split as store/heartbeat.
//
// The one idea worth understanding before reading anything else: LOCATION
// DOMINATES CONFIDENCE. Measured at Hopkinsville 2026-08-05, robots parked in
// one area read 0.95–0.97 while robots parked in another read 0.67–0.79 — a
// gap far larger than any difference between the robots themselves. So a
// robot's raw mean confidence is very nearly a measurement of where it spent
// its shift, and comparing robots on it ranks them by route, not by health.
// The residual below exists to remove exactly that confound.
package robotconfidence

import (
	"math"
	"sort"
)

// ── Minimum coverage ───────────────────────────────────────────────────────

// Coverage holds the thresholds that decide whether a residual can honestly
// be computed at all.
//
// These are deliberately Go constants rather than YAML config. Retention and
// dead-bands are deployment tunables — a plant can hold more or fewer days
// without changing what any number means. These three change the DEFINITION
// of the statistic: two plants running different values would publish figures
// called "residual" that are not comparable to each other. Changing them is a
// code change, reviewed as one.
type Coverage struct {
	// MinCellSamples is how many samples a robot needs on one segment before
	// that segment contributes to its residual. Below this the segment mean
	// is a couple of readings and swamps the average with noise.
	MinCellSamples int
	// MinPeerRobots is how many distinct robots a segment needs before its
	// fleet median means anything. At 1 the "median" is just the single
	// robot's own value, so every residual against it is mechanically 0 —
	// the confound restated, not removed.
	MinPeerRobots int
	// MinCells is how many qualifying segments a robot needs before its
	// residual is reported at all. Below this the robot has been compared to
	// the fleet in too few places for the comparison to survive one unusual
	// corner of the plant.
	MinCells int
}

// DefaultCoverage is the shipped threshold set.
var DefaultCoverage = Coverage{MinCellSamples: 8, MinPeerRobots: 2, MinCells: 6}

// CellStat is one robot's aggregate over one segment: how many samples and
// their mean confidence. Only reloc_status = 1 (SUCCESS) samples are ever
// accumulated into a CellStat — see rollup.go.
type CellStat struct {
	N    int
	Mean float64
}

// ── The residual ───────────────────────────────────────────────────────────

// FleetMedians reduces a per-robot, per-segment baseline to one median per
// segment: the median over robots of that robot's mean confidence there.
//
// Median rather than mean, because the input is small (rarely more than a
// dozen robots) and one badly-localized robot parked on a segment all day
// would drag a mean down far enough to make every healthy robot look good
// there — inventing a problem in the opposite direction.
//
// Segments observed by fewer than cov.MinPeerRobots robots are omitted
// entirely rather than given a weak median. A caller looking up such a
// segment gets no entry, which is the honest answer.
func FleetMedians(baseline map[string]map[string]CellStat, cov Coverage) map[string]float64 {
	bySegment := map[string][]float64{}
	for _, segments := range baseline {
		for seg, st := range segments {
			if st.N < cov.MinCellSamples {
				continue
			}
			bySegment[seg] = append(bySegment[seg], st.Mean)
		}
	}
	out := make(map[string]float64, len(bySegment))
	for seg, means := range bySegment {
		if len(means) < cov.MinPeerRobots {
			continue
		}
		out[seg] = Median(means)
	}
	return out
}

// Residual scores one robot against the fleet, holding location fixed.
//
//	residual = Σ_c n[c]·(mean[c] − fleetMedian[c]) / Σ_c n[c]
//
// Every segment the robot worked is compared only against other robots ON
// THAT SAME SEGMENT, and the per-segment differences are then weighted by how
// much time the robot actually spent there. A consistently negative residual
// is a robot problem. A near-zero residual with a low raw mean is a route
// problem — the robot is doing fine everywhere it goes, it just goes to bad
// places.
//
// The third return is false when the robot cleared too few qualifying
// segments. That case MUST be stored as NULL, never as 0.0: 0.0 means
// "measured, and indistinguishable from its peers", which is a real and
// reassuring finding, while NULL means "not enough overlap to say anything".
// Coalescing the second into the first reports confidence the data does not
// contain, in the reassuring direction.
func Residual(day map[string]CellStat, medians map[string]float64, cov Coverage) (float64, int, bool) {
	var weighted, weight float64
	cells := 0
	for seg, st := range day {
		if st.N < cov.MinCellSamples {
			continue
		}
		median, ok := medians[seg]
		if !ok {
			continue // too few peers on this segment for a comparison
		}
		cells++
		weighted += float64(st.N) * (st.Mean - median)
		weight += float64(st.N)
	}
	if cells < cov.MinCells || weight == 0 {
		return 0, cells, false
	}
	return weighted / weight, cells, true
}

// ── Descriptive statistics ─────────────────────────────────────────────────

// Median returns the median of vals. Returns 0 for an empty input; callers
// gate on length before treating the result as meaningful.
func Median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// Percentile returns the p-th percentile of vals (p in [0,1]) by nearest
// rank on the sorted values.
//
// Nearest rank rather than interpolation because the figure that matters here
// is p05 — "how bad does it actually get in the worst 5% of readings" — and
// interpolating produces a value no robot ever reported. For a floor-quality
// number, an observed reading is worth more than a smoothed one.
//
// NOTE FOR ANY FUTURE LONGER-WINDOW AGGREGATE: percentiles do not re-
// aggregate. A 14-day p05 is NOT the mean of fourteen daily p05s, and cannot
// be computed from them. Longer windows must go back to the raw samples,
// which is one of the four reasons the raw table is not redundant with the
// daily roll-up.
func Percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// Mean returns the arithmetic mean of vals, or 0 for an empty input.
func Mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// Min returns the smallest of vals, or 0 for an empty input.
func Min(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

// ── Geometry: snapping a sample to a scene segment ─────────────────────────

// Segment is one drivable path segment from the synced scene, flattened to
// its straight chord.
//
// THE CHORD IS AN APPROXIMATION AND IT IS NOT ALWAYS A SMALL ONE. scene_edges
// stores only the endpoints; the vendor's curved segments carry Bezier
// control handles that the table does not keep, and at Springfield a curve
// drawn as its chord sits up to 1.30 m from the lane the robot actually
// drives (see fleet.SceneEdge). SnapTolerance therefore has to be generous
// enough to cover that, which is the main reason it is configurable rather
// than a tight constant.
type Segment struct {
	Area     string
	Instance string
	FromX    float64
	FromY    float64
	ToX      float64
	ToY      float64
}

// Key is the segment's identity in the roll-up maps and in
// segment_confidence_daily. Area and instance together, because instance
// names are only unique within an area (scene_edges is UNIQUE(area_name,
// instance_name)).
func (s Segment) Key() string { return s.Area + "\x00" + s.Instance }

// SplitKey reverses Key.
func SplitKey(key string) (area, instance string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// DistanceTo returns the shortest distance from (x, y) to the segment,
// measured to the nearest point ON the segment — not to the infinite line
// through it. A robot near one END of a long path must not snap to it as
// though it were alongside.
func (s Segment) DistanceTo(x, y float64) float64 {
	dx, dy := s.ToX-s.FromX, s.ToY-s.FromY
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		// Degenerate zero-length segment; fall back to point distance.
		return math.Hypot(x-s.FromX, y-s.FromY)
	}
	// Projection parameter of the point onto the segment, clamped to [0,1]
	// so the closest point stays within the segment's extent.
	t := ((x-s.FromX)*dx + (y-s.FromY)*dy) / lenSq
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-(s.FromX+t*dx), y-(s.FromY+t*dy))
}

// SegmentIndex answers "which path segment is this sample on".
//
// The lookup is a linear scan over every segment. That is deliberate: a plant
// scene runs a few hundred segments, the roll-up is a once-a-night job, and a
// spatial index would be a second thing to keep correct in exchange for
// milliseconds nobody is waiting on. If a scene ever grows large enough for
// this to show up in the job's runtime, bucket the segments by a coarse grid
// — but measure first.
type SegmentIndex struct {
	segments []Segment
}

// NewSegmentIndex builds an index over the scene's segments.
func NewSegmentIndex(segments []Segment) *SegmentIndex {
	return &SegmentIndex{segments: segments}
}

// Len reports how many segments the index holds.
func (ix *SegmentIndex) Len() int { return len(ix.segments) }

// Nearest returns the closest segment to (x, y) and whether one was found
// within tolerance. A sample beyond tolerance of every segment is an ORPHAN:
// it is still a real reading and still counts toward the robot's own mean,
// but it belongs to no segment and contributes to no segment statistic.
// Callers should count orphans — a sudden rise in them means the scene was
// re-synced and the coordinates no longer line up with the paths.
func (ix *SegmentIndex) Nearest(x, y, tolerance float64) (Segment, bool) {
	best := math.Inf(1)
	bestIdx := -1
	for i := range ix.segments {
		if d := ix.segments[i].DistanceTo(x, y); d < best {
			best, bestIdx = d, i
		}
	}
	if bestIdx < 0 || best > tolerance {
		return Segment{}, false
	}
	return ix.segments[bestIdx], true
}
