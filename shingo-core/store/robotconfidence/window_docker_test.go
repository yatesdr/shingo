//go:build docker

package robotconfidence_test

import (
	"math"
	"testing"
	"time"

	"shingocore/store/robotconfidence"
)

// The windowed board statistic, end to end against a real database.
//
// The in-process test proves the histogram arithmetic. This proves the part
// that arithmetic cannot: that the distribution survives being written to
// Postgres as an INTEGER[] and read back — the direction pgx's shim does not
// support, which is why parsePGInt32Array exists and why v78's round-trip test
// pinning only the WRITE direction was half a behaviour pinned.

// A WINDOW IS THE SUM OF ITS DAYS, and the answer matches the readings.
//
// Three days rolled up separately, then read as one window. The window's p50
// must match the percentile over all three days' raw readings — which is the
// entire justification for storing a histogram instead of re-running the snap.
func TestLaneWindows_SumsDaysAndAgreesWithTheReadings(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	// Deliberately different distributions per day, so a window that silently
	// used only the last day would land somewhere visibly wrong.
	perDay := [][]float64{
		{0.95, 0.93, 0.91, 0.90},
		{0.55, 0.52, 0.50, 0.48},
		{0.20, 0.18, 0.15, 0.12},
	}
	var raw []float64
	for d, vals := range perDay {
		day := testDay.AddDate(0, 0, -d)
		var batch []robotconfidence.Sample
		for i, v := range vals {
			batch = append(batch, sample("AMR-01",
				day.Add(time.Duration(9+i)*time.Hour), v, float64(i), 0, 1))
			raw = append(raw, v)
		}
		insert(t, db, batch...)
		if _, err := db.RollUpRobotConfidence(day, rollUpCfg()); err != nil {
			t.Fatalf("roll-up day -%d: %v", d, err)
		}
	}

	from := testDay.AddDate(0, 0, -2)
	to := testDay.AddDate(0, 0, 1) // exclusive
	windows, err := db.LaneWindows(from, to)
	if err != nil {
		t.Fatalf("lane windows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("got %d lane windows, want 1: %v", len(windows), windows)
	}
	var w *robotconfidence.LaneWindow
	for _, v := range windows {
		w = v
	}

	if w.Samples != len(raw) {
		t.Errorf("Samples = %d, want %d — counts re-aggregate exactly", w.Samples, len(raw))
	}
	if w.Days != 3 {
		t.Errorf("Days = %d, want 3 — a window that cannot say how many days it "+
			"actually found is a thirty-day label over two days of data", w.Days)
	}
	if w.HistIncomplete {
		t.Error("HistIncomplete on rows this test just wrote — the INTEGER[] did " +
			"not survive the round trip")
	}

	for _, p := range []float64{0.05, 0.50, 0.95} {
		want := robotconfidence.Percentile(raw, p)
		got, ok := w.PercentileEstimate(p)
		if !ok {
			t.Fatalf("p%.0f: no estimate", p*100)
		}
		if math.Abs(got-want) > robotconfidence.HistBinWidth {
			t.Errorf("p%.0f estimate %.4f vs raw %.4f — off by more than one bin "+
				"width; the window is not the sum of its days",
				p*100, got, want)
		}
	}
}

// A WINDOW SPANNING AN EDIT SAYS SO.
//
// The version key splits an edited lane into one row per geometry, which is the
// whole point of it — but a window then sums across the edit. That is a
// legitimate thing to ask for and a dangerous thing to present silently, so the
// count of distinct versions travels with the answer.
func TestLaneWindows_ReportsWhenTheWindowSpansAnEdit(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)
	addNamedSegmentNoVersion(t, db, "area-a", "LM1-LM2", "LM1", "LM2", 0, 0, 10, 0)

	// One version in force until midday, a second afterwards.
	noon := testDay.Add(12 * time.Hour)
	openFirstLaneVersion(t, db, "area-a", "LM1-LM2", testDay, &noon)
	openLaneVersion(t, db, "area-a", "LM1-LM2", noon, nil)

	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.40, 1, 0, 1),
		sample("AMR-01", testDay.Add(15*time.Hour), 0.95, 2, 0, 1),
	)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	windows, err := db.LaneWindows(testDay, testDay.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("lane windows: %v", err)
	}
	var w *robotconfidence.LaneWindow
	for _, v := range windows {
		w = v
	}
	if w == nil {
		t.Fatal("no window for the edited lane")
	}
	if w.Versions != 2 {
		t.Errorf("Versions = %d, want 2 — a window summing across an edit must "+
			"say so, or it presents a blend as a measurement", w.Versions)
	}
	if w.Days != 1 {
		t.Errorf("Days = %d, want 1 — two rows on one day is one day of data, "+
			"and counting rows would report it as two", w.Days)
	}
	if w.Samples != 2 {
		t.Errorf("Samples = %d, want 2", w.Samples)
	}
}

// The sentinel survives the round trip as its own bin.
//
// A lane whose every reading was a no-estimate must come back out of the
// database banding exactly zero, not interpolated into the vendor's "> 0" band.
func TestLaneWindows_SentinelSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	noEstimate := math.Copysign(0, -1)
	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), noEstimate, 1, 0, 1),
		sample("AMR-01", testDay.Add(10*time.Hour), noEstimate, 2, 0, 1),
		sample("AMR-01", testDay.Add(11*time.Hour), noEstimate, 3, 0, 1),
	)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	windows, err := db.LaneWindows(testDay, testDay.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("lane windows: %v", err)
	}
	var w *robotconfidence.LaneWindow
	for _, v := range windows {
		w = v
	}
	if w == nil {
		t.Fatal("no window")
	}
	if w.Hist.SentinelCount() != 3 {
		t.Errorf("sentinel count = %d, want 3 — the bin did not survive the "+
			"INTEGER[] round trip", w.Hist.SentinelCount())
	}
	got, ok := w.PercentileEstimate(0.50)
	if !ok || got != 0 {
		t.Errorf("p50 = %v (ok=%v), want exactly 0 — this lane is blind, and any "+
			"non-zero value bands it as working", got, ok)
	}
}

// A ZONE WITH READINGS AND NO GEOMETRY MUST STILL BE READABLE.
//
// This is the state every plant is in between the confidence collection
// starting and the first successful map fetch: area_confidence_daily fills up
// while scene_areas is still empty. An earlier cut of the board keyed its zone
// list to the polygons, so those numbers were written and never read — the
// exact defect this project keeps finding, rebuilt one table over.
func TestAreaWindows_ReadableWithoutAnyGeometry(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)
	// Deliberately NO scene_areas row: attribution comes from the robot's own
	// area_ids, which does not need the map to have been fetched.
	insert(t, db,
		withAreaIDs(sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1), "8"),
		withAreaIDs(sample("AMR-01", testDay.Add(10*time.Hour), math.Copysign(0, -1), 2, 0, 1), "8"),
	)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	zones, err := db.AreaWindows(testDay, testDay.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("area windows: %v", err)
	}
	w := zones["08"]
	if w == nil {
		t.Fatalf("zone 08 absent from the window; got %v", zones)
	}
	if w.Samples != 2 || w.SentinelSamples != 1 {
		t.Errorf("samples=%d sentinel=%d, want 2/1", w.Samples, w.SentinelSamples)
	}
	if w.Class != "" {
		t.Errorf("class = %q, want empty — the map sync has not run, and inventing "+
			"a class would be worse than admitting we do not know", w.Class)
	}
	if w.Days != 1 {
		t.Errorf("Days = %d, want 1", w.Days)
	}
	// The statistic still works: half the readings were misses, so the
	// unconditioned p50 is the zero it counts them as.
	v, ok := w.PercentileEstimate(0.50)
	if !ok || v != 0 {
		t.Errorf("p50 = %v (ok=%v), want 0", v, ok)
	}
}
