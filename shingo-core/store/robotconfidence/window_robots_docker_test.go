//go:build docker

package robotconfidence_test

import (
	"testing"
	"time"
)

// The robots count on a lane window — the field the board has carried since
// it shipped and which read zero on every lane because the window query never
// selected the column.
//
// A separate file from window_docker_test.go: that one carries unrelated
// in-flight edits, and this test should not have to decide whether to sweep
// them into a commit.

// A ROBOT DRIVING THE SAME LANE ON TWO DAYS IS ONE ROBOT. The daily rows each
// count the robots that drove the lane that day; the window must report the
// widest day, never the sum, or a two-robot lane reads as four robots over a
// weekend and "how many AMRs actually run this aisle" stops being answerable.
func TestLaneWindows_RobotsIsTheWidestDayNotASum(t *testing.T) {
	t.Parallel()
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "LM1-LM2", 0, 0, 10, 0)

	// Day 1: two robots. Day 2: one of them again — same lane, same robot.
	day1, day2 := testDay.AddDate(0, 0, -1), testDay
	insert(t, db,
		sample("AMR-01", day1.Add(1*time.Hour), 0.90, 2, 0, 1),
		sample("AMR-02", day1.Add(2*time.Hour), 0.88, 3, 0, 1),
		sample("AMR-01", day2.Add(1*time.Hour), 0.91, 2, 0, 1),
	)
	if _, err := db.RollUpRobotConfidence(day1, rollUpCfg()); err != nil {
		t.Fatalf("roll-up day 1: %v", err)
	}
	if _, err := db.RollUpRobotConfidence(day2, rollUpCfg()); err != nil {
		t.Fatalf("roll-up day 2: %v", err)
	}

	windows, err := db.LaneWindows(day1, day2.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("lane windows: %v", err)
	}
	w, ok := windows["area-a\x00"+laneOf("LM1-LM2")]
	if !ok {
		t.Fatalf("no window for the lane: %v", windows)
	}
	if w.Robots != 2 {
		t.Errorf("Robots = %d, want 2 (the widest day; the sum would read 3)",
			w.Robots)
	}

	// The per-robot cut reads the same column from lane_robot_confidence_daily,
	// where every row's count is 1 by construction — pinned so a future schema
	// change cannot silently zero it the way the fleet query was zeroed.
	robotWindows, err := db.LaneRobotWindows(day1, day2.AddDate(0, 0, 1), "AMR-01")
	if err != nil {
		t.Fatalf("lane robot windows: %v", err)
	}
	rw, ok := robotWindows["area-a\x00"+laneOf("LM1-LM2")]
	if !ok {
		t.Fatalf("no robot window for the lane: %v", robotWindows)
	}
	if rw.Robots != 1 {
		t.Errorf("robot window Robots = %d, want 1", rw.Robots)
	}
}
