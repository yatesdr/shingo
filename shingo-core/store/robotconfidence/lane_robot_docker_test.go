//go:build docker

package robotconfidence_test

import (
	"math"
	"testing"
	"time"

	"shingocore/store/robotconfidence"
)

// The per-AMR grain, end to end.
//
// LaneWindows merges every robot that drove a lane into one histogram, and that
// merge is irreducible. LaneRobotWindows is the cut the map switches to when an
// operator picks one AMR: same window, same lane, readings from that vehicle
// only. This pins the three things that grain must get right that the lane test
// cannot — that two robots on the same lane come back separate, that each
// matches its own readings and not the union, and that the sentinel still
// survives the round trip at this grain.

// TWO ROBOTS ON ONE LANE COME BACK AS TWO WINDOWS, EACH ITS OWN.
func TestLaneRobotWindows_SeparatesRobotsOnTheSameLane(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	// Two robots, deliberately different distributions on the same lane, so a
	// query that silently merged them would land visibly between the two.
	amr1 := []float64{0.95, 0.93, 0.91, 0.90}
	amr2 := []float64{0.20, 0.18, 0.15, 0.12}
	var batch []robotconfidence.Sample
	for i, v := range amr1 {
		batch = append(batch, sample("AMR-01",
			testDay.Add(time.Duration(9+i)*time.Hour), v, float64(i), 0, 1))
	}
	for i, v := range amr2 {
		batch = append(batch, sample("AMR-02",
			testDay.Add(time.Duration(9+i)*time.Hour), v, float64(i), 0, 1))
	}
	insert(t, db, batch...)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}

	from, to := testDay, testDay.AddDate(0, 0, 1)
	for _, tc := range []struct {
		robot string
		want  []float64
	}{
		{"AMR-01", amr1},
		{"AMR-02", amr2},
	} {
		windows, err := db.LaneRobotWindows(from, to, tc.robot)
		if err != nil {
			t.Fatalf("%s: lane robot windows: %v", tc.robot, err)
		}
		if len(windows) != 1 {
			t.Fatalf("%s: got %d windows, want 1", tc.robot, len(windows))
		}
		var w *robotconfidence.LaneWindow
		for _, v := range windows {
			w = v
		}
		if w.Samples != len(tc.want) {
			t.Errorf("%s: Samples = %d, want %d — the window must hold only this "+
				"robot's ticks, not the lane's", tc.robot, w.Samples, len(tc.want))
		}
		// The union would sit between the two robots; each robot's own p50 is
		// its own readings, and a merge would miss both.
		want := robotconfidence.Percentile(tc.want, 0.50)
		got, ok := w.PercentileEstimate(0.50)
		if !ok {
			t.Fatalf("%s: no p50 estimate", tc.robot)
		}
		if math.Abs(got-want) > robotconfidence.HistBinWidth {
			t.Errorf("%s: p50 estimate %.4f vs this robot's readings %.4f — off by "+
				"more than a bin; the grain is leaking the other robot in",
				tc.robot, got, want)
		}
	}
}

// THE SENTINEL SURVIVES THE ROUND TRIP AT THE ROBOT GRAIN.
//
// A robot whose every reading on a lane was a no-estimate must come back out
// banding exactly zero, not interpolated into "> 0". The lane test pins this at
// the merged grain; this pins it one grain finer, because the per-robot write
// loop is its own code path.
func TestLaneRobotWindows_SentinelSurvivesTheRoundTrip(t *testing.T) {
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
	windows, err := db.LaneRobotWindows(testDay, testDay.AddDate(0, 0, 1), "AMR-01")
	if err != nil {
		t.Fatalf("lane robot windows: %v", err)
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
			"per-robot round trip", w.Hist.SentinelCount())
	}
	got, ok := w.PercentileEstimate(0.50)
	if !ok || got != 0 {
		t.Errorf("p50 = %v (ok=%v), want exactly 0 — this robot is blind on this "+
			"lane, and any non-zero bands it as working", got, ok)
	}
}

// A ROBOT THAT NEVER DROVE THE LANE ANSWERS NOTHING.
//
// The other side of the separation: an AMR with no readings on a lane must not
// get a zero-valued row back (which would band as blind) — it must be absent,
// which is the page's nodata. Empty fleet is not tested here because the caller
// falls back to LaneWindows for it; an unknown vehicle is the empty answer.
func TestLaneRobotWindows_AbsentRobotAnswersNothing(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)
	insert(t, db,
		sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 1, 0, 1),
	)
	if _, err := db.RollUpRobotConfidence(testDay, rollUpCfg()); err != nil {
		t.Fatalf("roll-up: %v", err)
	}
	windows, err := db.LaneRobotWindows(testDay, testDay.AddDate(0, 0, 1), "AMR-99")
	if err != nil {
		t.Fatalf("lane robot windows: %v", err)
	}
	if len(windows) != 0 {
		t.Errorf("got %d windows for a robot that never drove, want 0 — absence is "+
			"nodata, and a zero row would band a clean lane blind", len(windows))
	}
}
