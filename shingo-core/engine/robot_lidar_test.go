package engine

import (
	"testing"
	"time"

	"shingocore/fleet"
)

// TestLidarKeep pins the parked test across a probe: the frame is kept only
// when a poll after it still shows the robot connected, idle, at the same
// station and within 5 cm (DATA §6's limit, not the confidence dead band).
func TestLidarKeep(t *testing.T) {
	t.Parallel()
	before := fleet.RobotStatus{VehicleID: "AMR-01", Connected: true, CurrentStation: "PP57", X: 10, Y: 20}
	at := func(f func(*fleet.RobotStatus)) fleet.RobotStatus { r := before; f(&r); return r }
	cases := []struct {
		name  string
		after fleet.RobotStatus
		seen  bool
		want  string
	}{
		{"still parked", before, true, ""},
		{"moved 4 cm", at(func(r *fleet.RobotStatus) { r.X += 0.04 }), true, ""},
		{"moved 6 cm", at(func(r *fleet.RobotStatus) { r.X += 0.036; r.Y += 0.048 }), true, "moved"},
		{"station changed", at(func(r *fleet.RobotStatus) { r.CurrentStation = "PP58" }), true, "station changed"},
		{"left the station", at(func(r *fleet.RobotStatus) { r.CurrentStation = "" }), true, "station changed"},
		{"became busy", at(func(r *fleet.RobotStatus) { r.Busy = true }), true, "busy"},
		{"disconnected", at(func(r *fleet.RobotStatus) { r.Connected = false }), true, "disconnected"},
		{"not in the later poll", fleet.RobotStatus{}, false, "gone"},
	}
	for _, c := range cases {
		if got := lidarKeep(before, c.after, c.seen); got != c.want {
			t.Errorf("%s: lidarKeep = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestLidarCandidate: only a connected, idle robot at a station is probed.
func TestLidarCandidate(t *testing.T) {
	t.Parallel()
	ok := fleet.RobotStatus{VehicleID: "AMR-01", Connected: true, CurrentStation: "PP57"}
	for name, r := range map[string]fleet.RobotStatus{
		"no station":   {VehicleID: "AMR-01", Connected: true},
		"busy":         {VehicleID: "AMR-01", Connected: true, Busy: true, CurrentStation: "PP57"},
		"disconnected": {VehicleID: "AMR-01", CurrentStation: "PP57"},
		"keyless":      {Connected: true, CurrentStation: "PP57"},
	} {
		if lidarCandidate(r) {
			t.Errorf("%s was a candidate", name)
		}
	}
	if !lidarCandidate(ok) {
		t.Error("a parked robot was not a candidate")
	}
}

// TestNextLidarRound pins the clock-aligned schedule that keeps a restart from
// adding a round: always lidarRoundOffset past a UTC hour, strictly later.
func TestNextLidarRound(t *testing.T) {
	t.Parallel()
	h := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	for _, c := range []struct{ now, want time.Time }{
		{h, h.Add(5 * time.Minute)},
		{h.Add(4 * time.Minute), h.Add(5 * time.Minute)},
		{h.Add(5 * time.Minute), h.Add(65 * time.Minute)},
		{h.Add(30 * time.Minute), h.Add(65 * time.Minute)},
	} {
		if got := nextLidarRound(c.now); !got.Equal(c.want) {
			t.Errorf("nextLidarRound(%s) = %s, want %s", c.now.Format("15:04"), got.Format("15:04"), c.want.Format("15:04"))
		}
	}
}

// TestLidarRows: one row per lidar, carrying the bracket's station and the
// round's hour, nil quartiles staying nil.
func TestLidarRows(t *testing.T) {
	t.Parallel()
	med := 32.06
	h := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	rows := lidarRows(h, "AMR-01", "PP57", []fleet.LidarStats{
		{Lidar: "laser", UseForLoc: true, Beams: 360, Valid: 358, RSSIMedValid: &med, NearValid: 2},
		{Lidar: "laser2", Beams: 359},
	})
	if len(rows) != 2 || rows[0].Hour != h || rows[0].Station != "PP57" || !rows[0].UseForLoc ||
		rows[0].Valid != 358 || *rows[0].RSSIMedValid != med || rows[0].NearValid != 2 ||
		rows[1].Lidar != "laser2" || rows[1].RSSIMedValid != nil {
		t.Errorf("rows = %+v", rows)
	}
}
