package robotconfidence

import (
	"fmt"
	"math"
	"testing"
)

// buildFleet constructs the confound the residual exists to remove, in the
// shape it was actually measured in at Hopkinsville on 2026-08-05: robots
// parked in one area read 0.95–0.97 while robots parked in another read
// 0.67–0.79, and the gap is the FLOOR, not the robots.
//
//   - "good" segments sit at a fleet base of 0.95
//   - "bad" segments sit at a fleet base of 0.70
//   - AMR-SICK works only good segments, but is 0.10 below its peers there
//   - AMR-FINE works only bad segments, and matches its peers exactly
//
// Raw mean ranks AMR-FINE (0.70) below AMR-SICK (0.85) — precisely backwards.
// A correct residual has to invert that.
func buildFleet() (baseline map[string]map[string]CellStat, sick, fine map[string]CellStat) {
	const (
		goodBase = 0.95
		badBase  = 0.70
		offset   = 0.10
		n        = 10 // comfortably over Coverage.MinCellSamples
		segs     = 6  // exactly Coverage.MinCells
	)
	goodSeg := func(i int) string { return fmt.Sprintf("area-good\x00G%d", i) }
	badSeg := func(i int) string { return fmt.Sprintf("area-bad\x00B%d", i) }

	baseline = map[string]map[string]CellStat{}
	// Three peer robots that work everywhere and define the baseline.
	for _, peer := range []string{"AMR-P1", "AMR-P2", "AMR-P3"} {
		baseline[peer] = map[string]CellStat{}
		for i := 0; i < segs; i++ {
			baseline[peer][goodSeg(i)] = CellStat{N: n, Mean: goodBase}
			baseline[peer][badSeg(i)] = CellStat{N: n, Mean: badBase}
		}
	}

	sick = map[string]CellStat{}
	fine = map[string]CellStat{}
	baseline["AMR-SICK"] = map[string]CellStat{}
	baseline["AMR-FINE"] = map[string]CellStat{}
	for i := 0; i < segs; i++ {
		sick[goodSeg(i)] = CellStat{N: n, Mean: goodBase - offset}
		fine[badSeg(i)] = CellStat{N: n, Mean: badBase}
		baseline["AMR-SICK"][goodSeg(i)] = sick[goodSeg(i)]
		baseline["AMR-FINE"][badSeg(i)] = fine[badSeg(i)]
	}
	return baseline, sick, fine
}

// The headline test: if this passes, the statistic works. A robot that is
// genuinely degraded but works the easy half of the plant must score worse
// than a healthy robot that works the hard half — which is the opposite of
// what the raw numbers say.
func TestResidual_RanksCorrectlyWhereRawMeanRanksBackwards(t *testing.T) {
	baseline, sick, fine := buildFleet()
	medians := FleetMedians(baseline, DefaultCoverage)

	sickResidual, sickCells, sickOK := Residual(sick, medians, DefaultCoverage)
	fineResidual, fineCells, fineOK := Residual(fine, medians, DefaultCoverage)

	if !sickOK || !fineOK {
		t.Fatalf("both robots should clear coverage: sick ok=%v cells=%d, fine ok=%v cells=%d",
			sickOK, sickCells, fineOK, fineCells)
	}

	// The trap, asserted explicitly so the test documents what it is guarding
	// against: the raw means rank these two robots the WRONG way round.
	sickRawMean, fineRawMean := 0.85, 0.70
	if !(fineRawMean < sickRawMean) {
		t.Fatalf("fixture is wrong: raw mean must rank the healthy robot lower")
	}

	// The residual inverts it.
	if !(sickResidual < fineResidual) {
		t.Errorf("residual failed to invert the ranking: sick=%.4f fine=%.4f",
			sickResidual, fineResidual)
	}
	if math.Abs(sickResidual-(-0.10)) > 1e-9 {
		t.Errorf("sick residual = %.6f, want -0.10 (its uniform offset)", sickResidual)
	}
	if math.Abs(fineResidual) > 1e-9 {
		t.Errorf("fine residual = %.6f, want 0 (it matches its peers exactly)", fineResidual)
	}
}

// A segment observed by only one robot cannot yield a comparison: the
// "median" would be that robot's own value and every residual against it
// would be mechanically zero — the confound restated, not removed.
func TestFleetMedians_DropsSegmentsWithoutPeers(t *testing.T) {
	baseline := map[string]map[string]CellStat{
		"AMR-01": {"solo": {N: 50, Mean: 0.40}, "shared": {N: 50, Mean: 0.90}},
		"AMR-02": {"shared": {N: 50, Mean: 0.92}},
	}
	medians := FleetMedians(baseline, DefaultCoverage)
	if _, ok := medians["solo"]; ok {
		t.Error("a segment seen by one robot must not produce a fleet median")
	}
	if _, ok := medians["shared"]; !ok {
		t.Error("a segment seen by two robots should produce a fleet median")
	}
}

// A cell with too few samples is noise, and must not be allowed to swing a
// robot's residual.
func TestFleetMedians_DropsThinCells(t *testing.T) {
	baseline := map[string]map[string]CellStat{
		"AMR-01": {"seg": {N: 2, Mean: 0.10}},
		"AMR-02": {"seg": {N: 2, Mean: 0.10}},
	}
	if medians := FleetMedians(baseline, DefaultCoverage); len(medians) != 0 {
		t.Errorf("cells below MinCellSamples must not form a median, got %v", medians)
	}
}

// NULL and 0 are opposite findings here, and the distinction has to survive
// as far as the renderer. A robot compared to its peers in too few places has
// NOT scored zero — it has not been scored, and zero would read as the
// reassuring "indistinguishable from its peers".
func TestResidual_BelowMinimumCoverageIsNotZero(t *testing.T) {
	baseline, sick, _ := buildFleet()
	medians := FleetMedians(baseline, DefaultCoverage)

	// Keep one segment fewer than DefaultCoverage.MinCells.
	thin := map[string]CellStat{}
	kept := 0
	for seg, st := range sick {
		if kept >= DefaultCoverage.MinCells-1 {
			break
		}
		thin[seg] = st
		kept++
	}

	residual, cells, ok := Residual(thin, medians, DefaultCoverage)
	if ok {
		t.Fatalf("a robot with %d qualifying cells must not report a residual", cells)
	}
	if cells >= DefaultCoverage.MinCells {
		t.Fatalf("fixture is wrong: %d cells is not below the threshold", cells)
	}
	// The caller must store NULL. The zero value returned alongside ok=false
	// is not a score and must never be written as one.
	if residual != 0 {
		t.Errorf("unscored residual should return the zero value, got %v", residual)
	}
}

// A robot whose samples are thin on every segment clears no cells at all.
func TestResidual_ThinCellsDoNotCount(t *testing.T) {
	medians := map[string]float64{"a": 0.9, "b": 0.9, "c": 0.9}
	day := map[string]CellStat{
		"a": {N: 1, Mean: 0.5},
		"b": {N: 2, Mean: 0.5},
		"c": {N: 3, Mean: 0.5},
	}
	if _, cells, ok := Residual(day, medians, DefaultCoverage); ok || cells != 0 {
		t.Errorf("thin cells must not qualify: cells=%d ok=%v", cells, ok)
	}
}

func TestPercentileAndMedian(t *testing.T) {
	// p05 by nearest rank returns an OBSERVED reading, never an interpolated
	// one — for a floor-quality figure a value some robot actually reported
	// is worth more than a smoothed one.
	vals := make([]float64, 100)
	for i := range vals {
		vals[i] = float64(i+1) / 100 // 0.01 … 1.00
	}
	if got := Percentile(vals, 0.05); math.Abs(got-0.05) > 1e-9 {
		t.Errorf("Percentile(p05) = %v, want 0.05", got)
	}
	if got := Median([]float64{0.1, 0.9, 0.5}); got != 0.5 {
		t.Errorf("Median(odd) = %v, want 0.5", got)
	}
	if got := Median([]float64{0.2, 0.4}); math.Abs(got-0.3) > 1e-9 {
		t.Errorf("Median(even) = %v, want 0.3", got)
	}
	// Empty inputs return 0; callers gate on length before trusting it.
	if Percentile(nil, 0.05) != 0 || Median(nil) != 0 || Mean(nil) != 0 || Min(nil) != 0 {
		t.Error("empty inputs should return the zero value")
	}
}

// Median must not reorder its caller's slice — the roll-up reuses these
// accumulators across segments.
func TestMedianDoesNotMutateInput(t *testing.T) {
	in := []float64{0.9, 0.1, 0.5}
	Median(in)
	Percentile(in, 0.05)
	if in[0] != 0.9 || in[1] != 0.1 || in[2] != 0.5 {
		t.Errorf("input was reordered: %v", in)
	}
}

// ── Geometry ───────────────────────────────────────────────────────────────

func TestSegmentDistance_MeasuresToTheSegmentNotTheLine(t *testing.T) {
	// A 10 m segment along x. A point well past its end must measure to the
	// ENDPOINT, not to the infinite line through it — otherwise a robot
	// parked far beyond a long aisle snaps onto it as though alongside.
	s := Segment{Area: "a", Instance: "e1", FromName: "A", ToName: "B", FromX: 0, FromY: 0, ToX: 10, ToY: 0}
	if d := s.ChordDistanceTo(5, 3); math.Abs(d-3) > 1e-9 {
		t.Errorf("alongside: got %v, want 3", d)
	}
	if d := s.ChordDistanceTo(14, 0); math.Abs(d-4) > 1e-9 {
		t.Errorf("past the end: got %v, want 4 (to the endpoint)", d)
	}
	if d := s.ChordDistanceTo(-3, 4); math.Abs(d-5) > 1e-9 {
		t.Errorf("before the start: got %v, want 5", d)
	}
	// A degenerate zero-length segment must not divide by zero.
	p := Segment{FromX: 2, FromY: 2, ToX: 2, ToY: 2}
	if d := p.ChordDistanceTo(2, 5); math.Abs(d-3) > 1e-9 {
		t.Errorf("degenerate segment: got %v, want 3", d)
	}
}

func TestSegmentIndex_NearestAndOrphans(t *testing.T) {
	ix := NewSegmentIndex([]Segment{
		{Area: "a", Instance: "near", FromName: "A", ToName: "B", FromX: 0, FromY: 0, ToX: 10, ToY: 0},
		{Area: "a", Instance: "far", FromName: "C", ToName: "D", FromX: 0, FromY: 100, ToX: 10, ToY: 100},
	})
	got, ok := ix.Nearest(5, 0.5, 2.0)
	if !ok || got.Instance != "near" {
		t.Errorf("Nearest = %q ok=%v, want \"near\"", got.Instance, ok)
	}
	// Beyond tolerance of everything: an orphan. Still a real reading for the
	// robot's own mean, but it belongs to no segment.
	if _, ok := ix.Nearest(5, 40, 2.0); ok {
		t.Error("a sample 40 m from every segment must not snap to one")
	}
}

func TestSegmentKeyRoundTrip(t *testing.T) {
	// Names are only unique WITHIN an area (scene_edges is UNIQUE(area_name,
	// instance_name)), so the key must carry both.
	s := Segment{Area: "weld-cell", Instance: "LM119-LM120", FromName: "LM119", ToName: "LM120"}
	area, lane := SplitKey(s.Key())
	if area != "weld-cell" || lane != "LM119-LM120" {
		t.Errorf("round trip gave (%q, %q)", area, lane)
	}
	a2 := Segment{Area: "press", Instance: "LM119-LM120", FromName: "LM119", ToName: "LM120"}
	if s.Key() == a2.Key() {
		t.Error("the same lane name in two areas must not collide")
	}
}

// The two directed rows of a reciprocal pair are ONE lane.
//
// This is the correctness bug. scene_edges stores every drivable lane twice —
// 405 directed rows at Springfield are 212 physical lanes — and the twins
// have identical geometry, so which one a sample snapped to was decided by
// float noise. LM73-LM14 showed 48 readings and LM14-LM73 showed 116, and
// they are one piece of floor.
func TestSegmentLane_ReciprocalTwinsShareOneKey(t *testing.T) {
	fwd := Segment{Area: "a", Instance: "LM73-LM14", FromName: "LM73", ToName: "LM14",
		FromX: 0, FromY: 0, ToX: 5, ToY: 0}
	rev := Segment{Area: "a", Instance: "LM14-LM73", FromName: "LM14", ToName: "LM73",
		FromX: 5, FromY: 0, ToX: 0, ToY: 0}
	if fwd.Key() != rev.Key() {
		t.Errorf("reciprocal twins must aggregate as one lane: %q vs %q", fwd.Key(), rev.Key())
	}
	if fwd.Lane() != "LM14-LM73" {
		t.Errorf("Lane() must be the sorted pair, got %q", fwd.Lane())
	}
	// The directed name survives as an attribute — it is how a row joins
	// back to scene_edges — it is simply not the aggregation key.
	if fwd.Instance == rev.Instance {
		t.Error("the directed instance names should still differ")
	}
	// A genuinely one-way lane has no twin and is unaffected. The
	// LM13-LM141-LM140-LM14 corridor is entirely one-way, so the strongest
	// finding in the dataset does not move under this change.
	oneWay := Segment{Area: "a", Instance: "LM13-LM141", FromName: "LM13", ToName: "LM141"}
	if oneWay.Key() == fwd.Key() {
		t.Error("distinct lanes must not collide")
	}
}

// A curved lane is measured along its curve, and the difference is metres.
//
// LM10-LM113 is the widest bow in the Springfield scene: a DegenerateBezier
// sitting 1.302 m off its own 7.17 m chord. The class name says nothing —
// 292 of 405 segments are called DegenerateBezier and most of those are
// straight lines spelled as a cubic. Only the geometry knows.
func TestSegmentIndex_CurvedLaneIsMeasuredAlongItsCurve(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	curved := Segment{
		Area: "a", Instance: "LM10-LM113", FromName: "LM10", ToName: "LM113",
		FromX: -0.254, FromY: 33.554, ToX: -6.572, ToY: 36.951,
		Ctrl1X: f(-0.198), Ctrl1Y: f(36.401), Ctrl2X: f(-5.065), Ctrl2Y: f(36.951),
	}
	ix := NewSegmentIndex([]Segment{curved})

	// The scene's worst chord departure. dashboard-map.js documents 1.30 m
	// for this lane and an independent Python recomputation gave 1.302; both
	// must keep agreeing or the drawn lane and the snapped lane have parted.
	if dev := ix.MaxDeviation(); math.Abs(dev-1.302) > 0.01 {
		t.Errorf("MaxDeviation = %.4f m, want ~1.302 m for LM10-LM113", dev)
	}

	// A point sitting ON the curve at its midpoint is far from the chord.
	mx, my := curved.cubicPoint(0.5)
	if d := ix.distanceTo(0, mx, my); d > 0.02 {
		t.Errorf("a point on the curve should snap to it: got %.4f m", d)
	}
	if d := curved.ChordDistanceTo(mx, my); d < 1.0 {
		t.Errorf("the same point should be far from the CHORD; got %.4f m — "+
			"if this is small the fixture is no longer a bowed lane", d)
	}
}

// Partial or non-finite handles are not a curve.
//
// Three of four coordinates describe no cubic, and the renderer would have to
// invent the fourth. The same rule dashboard-map.js applies, applied here, so
// the drawn lane and the measured lane cannot disagree about which segments
// are curved.
func TestSegment_PartialHandlesAreNotACurve(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	base := Segment{Area: "a", FromName: "A", ToName: "B", FromX: 0, FromY: 0, ToX: 10, ToY: 0}
	for name, s := range map[string]Segment{
		"ctrl1 only":    {Ctrl1X: f(1), Ctrl1Y: f(1)},
		"three of four": {Ctrl1X: f(1), Ctrl1Y: f(1), Ctrl2X: f(2)},
		"ctrl2 only":    {Ctrl2X: f(2), Ctrl2Y: f(2)},
		"x without a y": {Ctrl1X: f(1), Ctrl2X: f(2)},
		"none at all":   {},
	} {
		seg := base
		seg.Ctrl1X, seg.Ctrl1Y, seg.Ctrl2X, seg.Ctrl2Y = s.Ctrl1X, s.Ctrl1Y, s.Ctrl2X, s.Ctrl2Y
		if seg.Curved() {
			t.Errorf("%s: must not be treated as a curve", name)
		}
		if dev := NewSegmentIndex([]Segment{seg}).MaxDeviation(); dev != 0 {
			t.Errorf("%s: a non-curve must have zero deviation, got %v", name, dev)
		}
	}
}

// The chord prune must never change an answer.
//
// Nearest resolves the best chord candidate first and then skips any segment
// whose chordDist-deviation cannot beat it. That is only sound because the
// true distance to a curve differs from the distance to its chord by at most
// the curve's own maximum departure. This test asserts the property directly
// against brute force over a spread of geometry — if the bound is ever wrong,
// the failure is a silently mis-attributed sample, which is exactly the class
// of bug this whole commit exists to fix.
func TestSegmentIndex_PruneAgreesWithBruteForce(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	var segs []Segment
	// A deterministic spread: straight lanes, gentle bows and severe bows,
	// laid on a grid so many of them are plausible rivals for any sample.
	for i := 0; i < 40; i++ {
		x := float64(i%8) * 3
		y := float64(i/8) * 3
		s := Segment{
			Area: "a", Instance: fmt.Sprintf("e%d", i),
			FromName: fmt.Sprintf("N%d", i), ToName: fmt.Sprintf("N%d", i+1),
			FromX: x, FromY: y, ToX: x + 2.5, ToY: y + 1.0,
		}
		if i%3 != 0 { // two thirds carry handles, with varying bow
			bow := 0.2 + float64(i%5)
			s.Ctrl1X, s.Ctrl1Y = f(x+0.8), f(y+bow)
			s.Ctrl2X, s.Ctrl2Y = f(x+1.7), f(y+bow)
		}
		segs = append(segs, s)
	}
	ix := NewSegmentIndex(segs)

	brute := func(x, y, tol float64) (string, bool) {
		best, bestIdx := math.Inf(1), -1
		for i := range segs {
			if d := ix.distanceTo(i, x, y); d < best {
				best, bestIdx = d, i
			}
		}
		if bestIdx < 0 || best > tol {
			return "", false
		}
		return segs[bestIdx].Instance, true
	}

	const tol = 1.0
	checked := 0
	for xi := 0; xi <= 120; xi++ {
		for yi := 0; yi <= 60; yi++ {
			x, y := float64(xi)*0.2-1, float64(yi)*0.2-1
			wantName, wantOK := brute(x, y, tol)
			got, gotOK := ix.Nearest(x, y, tol)
			if gotOK != wantOK {
				t.Fatalf("(%.2f, %.2f): pruned ok=%v, brute ok=%v", x, y, gotOK, wantOK)
			}
			if wantOK {
				// Ties are legitimate — two lanes can be exactly equidistant
				// — so compare the DISTANCE, which is what the prune claims
				// to preserve, rather than the winner's name.
				gotD := ix.distanceTo(indexOf(segs, got.Instance), x, y)
				wantD := ix.distanceTo(indexOf(segs, wantName), x, y)
				if math.Abs(gotD-wantD) > 1e-12 {
					t.Fatalf("(%.2f, %.2f): pruned picked %s at %.9f, brute picked %s at %.9f",
						x, y, got.Instance, gotD, wantName, wantD)
				}
			}
			checked++
		}
	}
	if checked < 5000 {
		t.Fatalf("only %d points checked — the sweep is too thin to prove anything", checked)
	}
}

func indexOf(segs []Segment, instance string) int {
	for i := range segs {
		if segs[i].Instance == instance {
			return i
		}
	}
	return -1
}

// An edge with no endpoint names cannot be keyed, and must not pretend to be.
//
// scene_edges declares from_name/to_name NOT NULL with an empty default, so
// an older sync leaves them blank. The tempting fallbacks are both wrong in
// the same way: keying on the directed instance name puts those rows at
// 405-lane granularity in a table where everything else is at 212, and
// sorting two empty names merges the whole plant into one aggregate. Either
// produces a number that looks measured and is not comparable to the row
// beside it.
func TestSegment_UnnamedEndpointsAreNotKeyable(t *testing.T) {
	named := Segment{Area: "a", Instance: "LM1-LM2", FromName: "LM1", ToName: "LM2"}
	if !named.Keyable() || named.Lane() == "" {
		t.Fatal("a fully named segment must be keyable")
	}
	for name, s := range map[string]Segment{
		"both blank": {Area: "a", Instance: "LM1-LM2"},
		"from blank": {Area: "a", Instance: "LM1-LM2", ToName: "LM2"},
		"to blank":   {Area: "a", Instance: "LM1-LM2", FromName: "LM1"},
	} {
		if s.Keyable() {
			t.Errorf("%s: must not be keyable", name)
		}
		// Not the instance name, and not "-". Empty, so nothing downstream
		// can mistake it for an identity.
		if got := s.Lane(); got != "" {
			t.Errorf("%s: Lane() = %q, want \"\" — a fallback here would key "+
				"this row at a different granularity than the rest of the table", name, got)
		}
	}
	// Two unkeyable edges must not collapse onto each other either.
	a := Segment{Area: "x", Instance: "E1"}
	b := Segment{Area: "x", Instance: "E2"}
	if a.Keyable() || b.Keyable() {
		t.Fatal("fixture: both should be unkeyable")
	}
	if a.Lane() != "" || b.Lane() != "" {
		t.Error("unkeyable segments must not produce a shared lane identity")
	}
}
