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

// Segment is one drivable path segment from the synced scene.
//
// IT IS DRAWN AND MEASURED AS THE CURVE THE ROBOT DRIVES. The handles were
// in scene_edges all along — migration v62 added them, 294 of Springfield's
// 405 rows carry a complete pair — and this package's loader simply did not
// select them, so every distance here was measured against the chord. At
// Springfield that chord sits up to 1.302 m from the painted lane, and the
// consequence is not the distance error: it is that 20.3% of samples snap to
// a DIFFERENT segment than they belong to. Every per-lane figure was computed
// on that attribution.
//
// Ctrl1X/Ctrl1Y/Ctrl2X/Ctrl2Y are nil TOGETHER on a lane the fleet drives
// straight. Three of four coordinates describe no cubic. Test for presence,
// never for zero — the origin is a real coordinate on a plant map, and a
// handle defaulted to (0,0) sweeps the lane tens of metres across the floor.
type Segment struct {
	Area string
	// Instance is the directed vendor name ("LM10-LM48"). Kept for reference
	// and for joining back to scene_edges; it is NOT the aggregation key.
	Instance string
	FromName string
	ToName   string
	FromX    float64
	FromY    float64
	ToX      float64
	ToY      float64
	Ctrl1X   *float64
	Ctrl1Y   *float64
	Ctrl2X   *float64
	Ctrl2Y   *float64
}

// Curved reports whether the segment carries a complete handle pair.
func (s Segment) Curved() bool {
	return s.Ctrl1X != nil && s.Ctrl1Y != nil && s.Ctrl2X != nil && s.Ctrl2Y != nil
}

// Lane is the segment's UNDIRECTED identity: the endpoint pair, sorted.
//
// THIS IS A CORRECTNESS FIX, NOT A TIDY-UP. scene_edges stores every drivable
// lane twice — at Springfield 405 directed rows are 193 reciprocal pairs plus
// 19 genuinely one-way lanes, i.e. 212 pieces of floor. The two rows of a
// pair have identical geometry, so a sample sitting on one sits exactly as
// close to the other and the winner is decided by float noise: 81.7% of
// samples have a second-best directed edge within 5 cm of the best. Merging
// to the lane drops that to 23.6%, and what remains is genuine junction
// geometry rather than a coin toss.
//
// Aggregating on the directed name therefore splits one lane's readings
// arbitrarily between its twins: LM73-LM14 shows 48 samples and LM14-LM73
// shows 116, and they are one piece of floor. Every n, every percentile and
// every minimum-sample threshold was up to 2x wrong depending on which twin
// won the toss, and a lane could fall below the minimum purely because its
// twin took the readings.
//
// The 19 one-way lanes are unaffected — they have no twin to merge with — and
// that includes the LM13-LM141-LM140-LM14 corridor, which is the strongest
// finding in the dataset. Its numbers do not move under this change. Worth
// knowing before somebody reads that as a discrepancy.
// Returns "" when the segment has no endpoint names — see Keyable.
func (s Segment) Lane() string {
	if !s.Keyable() {
		return ""
	}
	a, b := s.FromName, s.ToName
	if b < a {
		a, b = b, a
	}
	return a + "-" + b
}

// Keyable reports whether this segment can be aggregated at all.
//
// scene_edges declares from_name/to_name NOT NULL with an empty-string
// default, so a scene synced by an older Core carries two empty names and
// there is no honest lane key to be made from them.
//
// THE TEMPTING FIX IS WORSE THAN THE PROBLEM. Falling back to the directed
// instance name looks harmless and is not: those rows would key at 405-lane
// granularity while every other row in the same table keys at 212, with
// nothing in the schema saying which is which. That is the same defect as a
// silent drop — a number that looks measured and is not comparable to the row
// beside it — wearing a different hat. Sorting two empty names is no better:
// every such row keys the same way and the whole plant merges into one
// aggregate that reads like a real measurement.
//
// So an unkeyable edge is QUARANTINED, exactly as a foreign-map sample is.
// The segment stays IN the index deliberately — dropping it would let its
// samples snap to whichever neighbour happens to be within tolerance, and at
// Springfield 23.6% of samples have a rival lane within 5 cm, so that is a
// near-certain silent misattribution rather than a theoretical one. Instead
// the sample lands here, is counted, and is excluded from every statistic,
// with both counts reaching the job's log line.
//
// The real fix is upstream — scene sync should refuse to write an edge it
// cannot name — and this is the guard that says so out loud until it does.
func (s Segment) Keyable() bool { return s.FromName != "" && s.ToName != "" }

// Key is the segment's identity in the roll-up maps and in
// lane_confidence_daily.
//
// AREA-SCOPED, and that is not decoration. scene_edges is UNIQUE(area_name,
// instance_name), so names are only unique within an area. Springfield has
// exactly one area, which is precisely the condition under which dropping the
// area from the key ships undetected and breaks at the first multi-area plant.
func (s Segment) Key() string { return s.Area + "\x00" + s.Lane() }

// SplitKey reverses Key.
func SplitKey(key string) (area, lane string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// cubicSamples is the polyline resolution a curved segment is flattened to.
//
// 24 matches dashboard-map.js's CUBIC_SAMPLES so the lane a sample is
// attributed to and the lane drawn on screen are the same curve at the same
// resolution. Divergence between those two would be invisible and would make
// the map disagree with its own numbers.
const cubicSamples = 24

// cubicPoint evaluates the cubic Bezier at t.
func (s Segment) cubicPoint(t float64) (float64, float64) {
	mt := 1 - t
	a := mt * mt * mt
	b := 3 * mt * mt * t
	c := 3 * mt * t * t
	d := t * t * t
	return a*s.FromX + b**s.Ctrl1X + c**s.Ctrl2X + d*s.ToX,
		a*s.FromY + b**s.Ctrl1Y + c**s.Ctrl2Y + d*s.ToY
}

// pointToChord is the distance from (x, y) to the straight segment between
// (ax, ay) and (bx, by), measured to the nearest point ON it — not to the
// infinite line through it. A robot near one END of a long lane must not snap
// to it as though it were alongside.
func pointToChord(x, y, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		return math.Hypot(x-ax, y-ay)
	}
	t := ((x-ax)*dx + (y-ay)*dy) / lenSq
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-(ax+t*dx), y-(ay+t*dy))
}

// ChordDistanceTo is the distance to the segment's straight chord. It is not
// the answer — it is the cheap bound the index prunes with.
func (s Segment) ChordDistanceTo(x, y float64) float64 {
	return pointToChord(x, y, s.FromX, s.FromY, s.ToX, s.ToY)
}

// SegmentIndex answers "which path segment is this sample on".
//
// Each segment is flattened ONCE at construction, not per sample. A straight
// lane flattens to its two endpoints and costs exactly what it did before;
// a curved one becomes a 24-step polyline.
//
// The lookup is still a linear scan, and it is now pruned by the chord. The
// naive form — every sample against every polyline vertex — is 24x the work
// it used to be, which is fine for one plant-day today and is not fine at the
// stated target of 40 robots on a 5x map, where the 14-day baseline pass
// would run to tens of billions of distance evaluations. The prune below
// removes that without changing a single answer.
type SegmentIndex struct {
	segments []Segment
	// poly[i] is segment i flattened: x0,y0,x1,y1,... Always at least the
	// two endpoints.
	poly [][]float64
	// dev[i] bounds how far segment i's true curve departs from its chord.
	// Zero for a straight lane. This is what makes the prune exact: the true
	// distance to a curve can be smaller OR larger than the distance to its
	// chord, but never by more than dev, so chordDist-dev is a genuine lower
	// bound on the true distance and a segment failing it cannot win.
	dev []float64
}

// NewSegmentIndex builds an index over the scene's segments, flattening each
// curved one to a polyline and recording its worst departure from its chord.
func NewSegmentIndex(segments []Segment) *SegmentIndex {
	ix := &SegmentIndex{
		segments: segments,
		poly:     make([][]float64, len(segments)),
		dev:      make([]float64, len(segments)),
	}
	for i, s := range segments {
		if !s.Curved() {
			ix.poly[i] = []float64{s.FromX, s.FromY, s.ToX, s.ToY}
			continue
		}
		pts := make([]float64, 0, 2*(cubicSamples+1))
		worst := 0.0
		for k := 0; k <= cubicSamples; k++ {
			x, y := s.cubicPoint(float64(k) / cubicSamples)
			pts = append(pts, x, y)
			if d := pointToChord(x, y, s.FromX, s.FromY, s.ToX, s.ToY); d > worst {
				worst = d
			}
		}
		ix.poly[i], ix.dev[i] = pts, worst
	}
	return ix
}

// Len reports how many segments the index holds.
func (ix *SegmentIndex) Len() int { return len(ix.segments) }

// MaxDeviation is the scene's worst curve-to-chord departure, in metres. It
// is the number that says how wrong a chord-based snap would be: 1.302 m at
// Springfield, on LM10-LM113.
func (ix *SegmentIndex) MaxDeviation() float64 {
	worst := 0.0
	for _, d := range ix.dev {
		if d > worst {
			worst = d
		}
	}
	return worst
}

// distanceTo is the true distance from (x, y) to segment i's flattened path.
func (ix *SegmentIndex) distanceTo(i int, x, y float64) float64 {
	p := ix.poly[i]
	best := math.Inf(1)
	for k := 0; k+3 < len(p); k += 2 {
		if d := pointToChord(x, y, p[k], p[k+1], p[k+2], p[k+3]); d < best {
			best = d
		}
	}
	return best
}

// Nearest returns the closest segment to (x, y) and whether one was found
// within tolerance.
//
// A sample beyond tolerance of every segment is an ORPHAN: still a real
// reading, still counted toward the robot's own mean, but belonging to no
// lane and contributing to no lane statistic. Callers should count orphans —
// at Springfield the count is currently ZERO, with the worst observed
// distance 0.877 m against a 1.0 m tolerance and nothing at all between 1 m
// and 5 m, which makes it a clean regression signal: if it ever becomes
// non-zero either the scene has drifted from the floor or a robot is driving
// somewhere it should not be.
func (ix *SegmentIndex) Nearest(x, y, tolerance float64) (Segment, bool) {
	if len(ix.segments) == 0 {
		return Segment{}, false
	}
	// Pass 1 — chord distance for every segment. One point-to-segment test
	// each, the same cost the whole lookup used to be.
	chord := make([]float64, len(ix.segments))
	lead := 0
	for i := range ix.segments {
		chord[i] = ix.segments[i].ChordDistanceTo(x, y)
		if chord[i] < chord[lead] {
			lead = i
		}
	}
	// Pass 2 — resolve the best chord candidate for real, then test only
	// those segments whose lower bound could still beat it.
	best := ix.distanceTo(lead, x, y)
	bestIdx := lead
	for i := range ix.segments {
		if i == lead || chord[i]-ix.dev[i] > best {
			continue
		}
		if d := ix.distanceTo(i, x, y); d < best {
			best, bestIdx = d, i
		}
	}
	if best > tolerance {
		return Segment{}, false
	}
	return ix.segments[bestIdx], true
}
