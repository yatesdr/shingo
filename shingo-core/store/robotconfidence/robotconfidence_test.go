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
	s := Segment{Area: "a", Instance: "e1", FromX: 0, FromY: 0, ToX: 10, ToY: 0}
	if d := s.DistanceTo(5, 3); math.Abs(d-3) > 1e-9 {
		t.Errorf("alongside: got %v, want 3", d)
	}
	if d := s.DistanceTo(14, 0); math.Abs(d-4) > 1e-9 {
		t.Errorf("past the end: got %v, want 4 (to the endpoint)", d)
	}
	if d := s.DistanceTo(-3, 4); math.Abs(d-5) > 1e-9 {
		t.Errorf("before the start: got %v, want 5", d)
	}
	// A degenerate zero-length segment must not divide by zero.
	p := Segment{FromX: 2, FromY: 2, ToX: 2, ToY: 2}
	if d := p.DistanceTo(2, 5); math.Abs(d-3) > 1e-9 {
		t.Errorf("degenerate segment: got %v, want 3", d)
	}
}

func TestSegmentIndex_NearestAndOrphans(t *testing.T) {
	ix := NewSegmentIndex([]Segment{
		{Area: "a", Instance: "near", FromX: 0, FromY: 0, ToX: 10, ToY: 0},
		{Area: "a", Instance: "far", FromX: 0, FromY: 100, ToX: 10, ToY: 100},
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
	// Instance names are only unique WITHIN an area (scene_edges is
	// UNIQUE(area_name, instance_name)), so the key must carry both.
	s := Segment{Area: "weld-cell", Instance: "LM120"}
	area, instance := SplitKey(s.Key())
	if area != "weld-cell" || instance != "LM120" {
		t.Errorf("round trip gave (%q, %q)", area, instance)
	}
	a2 := Segment{Area: "press", Instance: "LM120"}
	if s.Key() == a2.Key() {
		t.Error("same instance name in two areas must not collide")
	}
}
