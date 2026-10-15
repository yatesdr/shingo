package engine

import (
	"errors"
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/store/cleaning"
)

// idleRobot is a robot every cleaning guard passes: connected, dispatchable,
// localized, deck empty and certain, well charged.
func idleRobot(id string) fleet.RobotStatus {
	return fleet.RobotStatus{VehicleID: id, Connected: true, Available: true,
		RelocStatus: 1, JackState: 3, BatteryLevel: 80}
}

// TestCleanRefusal_EveryGuard pins each guard's words and that a robot passing
// all of them is eligible. Order matters only for which sentence a robot with
// two problems shows; each case breaks exactly one.
func TestCleanRefusal_EveryGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*fleet.RobotStatus, *cleaning.View)
		want string
	}{
		{"eligible", func(*fleet.RobotStatus, *cleaning.View) {}, ""},
		{"disconnected", func(r *fleet.RobotStatus, _ *cleaning.View) { r.Connected = false }, "not connected"},
		{"undispatchable", func(r *fleet.RobotStatus, _ *cleaning.View) { r.Available = false }, "not dispatchable"},
		{"emergency", func(r *fleet.RobotStatus, _ *cleaning.View) { r.Emergency = true }, "emergency stop"},
		{"error", func(r *fleet.RobotStatus, _ *cleaning.View) { r.IsError = true }, "in error"},
		{"blocked", func(r *fleet.RobotStatus, _ *cleaning.View) { r.Blocked = true }, "blocked"},
		{"charging", func(r *fleet.RobotStatus, _ *cleaning.View) { r.Charging = true }, "charging"},
		{"busy", func(r *fleet.RobotStatus, _ *cleaning.View) { r.Busy = true }, "on a task"},
		{"carrying", func(r *fleet.RobotStatus, _ *cleaning.View) { r.JackState = 1 }, "may be carrying a bin"},
		{"deck uncertain", func(r *fleet.RobotStatus, _ *cleaning.View) { r.JackState = 2 }, "may be carrying a bin"},
		{"IsLoaded alone, no jack report", func(r *fleet.RobotStatus, _ *cleaning.View) { r.JackState = 0; r.IsLoaded = true }, "may be carrying a bin"},
		{"reloc failed", func(r *fleet.RobotStatus, _ *cleaning.View) { r.RelocStatus = 0 }, "not localized"},
		{"relocing", func(r *fleet.RobotStatus, _ *cleaning.View) { r.RelocStatus = 2 }, "not localized"},
		{"reloc completed unconfirmed is fine", func(r *fleet.RobotStatus, _ *cleaning.View) { r.RelocStatus = 3 }, ""},
		{"low battery", func(r *fleet.RobotStatus, _ *cleaning.View) { r.BatteryLevel = 24 }, "battery 24% is below 25%"},
		{"battery at floor is fine", func(r *fleet.RobotStatus, _ *cleaning.View) { r.BatteryLevel = 25 }, ""},
		{"opted out", func(_ *fleet.RobotStatus, v *cleaning.View) { v.OptedOut = true }, "opted out of cleaning"},
	}
	for _, c := range cases {
		r, v := idleRobot("AMR-01"), cleaning.View{}
		c.mut(&r, &v)
		if got := CleanRefusal(r, v.OptedOut, 25); got != c.want {
			t.Errorf("%s: CleanRefusal = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestPickCleaningRobot pins due-from-dispatch, the longest-since pick, one
// robot per tick, and that the guard runs before the pick.
func TestPickCleaningRobot(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	s := cleaning.Setup{Points: []string{"LM513", "LM514"}, Frequency: 4 * time.Hour, MinBattery: 25}
	robots := []fleet.RobotStatus{idleRobot("AMR-01"), idleRobot("AMR-02"), idleRobot("AMR-03")}

	cases := []struct {
		name  string
		views map[string]cleaning.View
		want  string
	}{
		{"none due", map[string]cleaning.View{
			"AMR-01": {LastDispatched: at(time.Hour)}, "AMR-02": {LastDispatched: at(2 * time.Hour)},
			"AMR-03": {LastDispatched: at(3*time.Hour + 59*time.Minute)}}, ""},
		// A mission that FAILED still counts: the view is the last dispatch of
		// any state, so a robot whose missions fail waits its frequency like
		// any other instead of being sent every minute.
		{"due measured from dispatch, longest since wins", map[string]cleaning.View{
			"AMR-01": {LastDispatched: at(5 * time.Hour)}, "AMR-02": {LastDispatched: at(9 * time.Hour)},
			"AMR-03": {LastDispatched: at(time.Hour)}}, "AMR-02"},
		{"exactly the frequency is due", map[string]cleaning.View{
			"AMR-01": {LastDispatched: at(4 * time.Hour)}, "AMR-02": {LastDispatched: at(time.Hour)},
			"AMR-03": {LastDispatched: at(time.Hour)}}, "AMR-01"},
		{"never sent goes first, then vehicle id", map[string]cleaning.View{
			"AMR-01": {LastDispatched: at(50 * time.Hour)}}, "AMR-02"},
		{"an ineligible robot is skipped even when most overdue", map[string]cleaning.View{
			"AMR-01": {OptedOut: true}, "AMR-02": {LastDispatched: at(5 * time.Hour)},
			"AMR-03": {LastDispatched: at(6 * time.Hour)}}, "AMR-03"},
	}
	for _, c := range cases {
		if got := pickCleaningRobot(now, s, robots, c.views); got != c.want {
			t.Errorf("%s: pick = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCleaningFinalState pins the state map, including STOPPED → cancelled
// (never finished), FAILED final despite rds.IsTerminal, a read error leaving
// the row open, and the age rule asking for a vendor cancel.
func TestCleaningFinalState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	young := cleaning.Mission{DispatchedAt: now.Add(-5 * time.Minute)}
	old := cleaning.Mission{DispatchedAt: now.Add(-31 * time.Minute)}
	d := func(s string) *fleet.VendorOrderDetail { return &fleet.VendorOrderDetail{State: s} }
	readErr := errors.New("rds down")
	cases := []struct {
		name       string
		m          cleaning.Mission
		detail     *fleet.VendorOrderDetail
		err        error
		wantState  string
		wantCancel bool
	}{
		{"finished", young, d("FINISHED"), nil, cleaning.StateFinished, false},
		{"stopped is cancelled", young, d("STOPPED"), nil, cleaning.StateCancelled, false},
		{"failed is final", young, d("FAILED"), nil, cleaning.StateFailed, false},
		{"running stays open", young, d("RUNNING"), nil, "", false},
		{"read error stays open", young, nil, readErr, "", false},
		{"running past max age: cancel then failed", old, d("RUNNING"), nil, cleaning.StateFailed, true},
		{"unreadable past max age: cancel then failed", old, nil, readErr, cleaning.StateFailed, true},
		{"finished past max age is still finished", old, d("FINISHED"), nil, cleaning.StateFinished, false},
	}
	for _, c := range cases {
		st, cancel := cleaningFinalState(c.m, c.detail, c.err, now)
		if st != c.wantState || cancel != c.wantCancel {
			t.Errorf("%s: = (%q, %v), want (%q, %v)", c.name, st, cancel, c.wantState, c.wantCancel)
		}
	}
}

// TestParseCleaningFrequency pins the segment's four answers (4, 8, 12, 24 h),
// typed with or without a space or as a Go duration, and that the old short
// ones and 3 h are not among them.
func TestParseCleaningFrequency(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{"4h": true, "8 h": true, "12h0m0s": true, " 24 h ": true,
		"1h": false, "2h": false, "3h": false, "30m": false, "": false, "soon": false} {
		if _, ok := parseCleaningFrequency(in); ok != want {
			t.Errorf("parseCleaningFrequency(%q) ok = %v, want %v", in, ok, want)
		}
	}
}

// TestValidateCleaningSetup pins the field errors: only 4/8/12/24 h and a
// percent battery floor. There are no active hours to check.
func TestValidateCleaningSetup(t *testing.T) {
	t.Parallel()
	ok := cleaning.Setup{Frequency: 24 * time.Hour, MinBattery: 25}
	if f := validateCleaningSetup(ok); len(f) != 0 {
		t.Errorf("valid setup refused: %v", f)
	}
	bad := cleaning.Setup{Frequency: 2 * time.Hour, MinBattery: 101}
	f := validateCleaningSetup(bad)
	for _, k := range []string{"frequency", "min_battery"} {
		if f[k] == "" {
			t.Errorf("%s not refused: %v", k, f)
		}
	}
}

// TestCleanNowRefusal pins the manual verb's words in order, so the pop-up's
// disabled reason and the POST's refusal are the same sentence.
func TestCleanNowRefusal(t *testing.T) {
	t.Parallel()
	r := idleRobot("AMR-01")
	full := CleanNowFacts{SetUp: true, CanReport: true, Robot: &r, MinBattery: 25}
	cases := []struct {
		name string
		mut  func(*CleanNowFacts)
		want string
	}{
		{"may", func(*CleanNowFacts) {}, ""},
		{"no setup", func(f *CleanNowFacts) { f.SetUp = false }, "no clean mission is set up"},
		{"point problem: lowest row's words", func(f *CleanNowFacts) {
			f.PointProblems = map[string]string{"points.1": "LM514 is not in the scene", "points.0": "LM513 is not in the scene"}
		}, "LM513 is not in the scene"},
		{"no seam", func(f *CleanNowFacts) { f.CanReport = false }, "the fleet cannot report a mission's state"},
		{"another open", func(f *CleanNowFacts) { f.AnyOpen = true }, "another robot is at the knife"},
		{"not in poll", func(f *CleanNowFacts) { f.Robot = nil }, "not in the fleet's latest report"},
		{"guard words", func(f *CleanNowFacts) { rc := *f.Robot; rc.Charging = true; f.Robot = &rc }, "charging"},
	}
	for _, c := range cases {
		f := full
		c.mut(&f)
		if got := CleanNowRefusal(f); got != c.want {
			t.Errorf("%s: = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCleaningWords pins the live words for the setup block's title.
func TestCleaningWords(t *testing.T) {
	t.Parallel()
	base := CleaningWordsFacts{Points: []string{"LM513"}, CanReport: true}
	cases := []struct {
		name string
		mut  func(*CleaningWordsFacts)
		want string
	}{
		{"running", func(*CleaningWordsFacts) {}, "running"},
		{"off", func(f *CleaningWordsFacts) { f.Points = nil }, "off"},
		{"point problem", func(f *CleaningWordsFacts) {
			f.PointProblems = map[string]string{"points.0": "LM513 is not in the scene"}
		}, "stopped: LM513 is not in the scene"},
		{"no seam", func(f *CleaningWordsFacts) { f.CanReport = false }, "stopped: the fleet cannot report a mission's state"},
	}
	for _, c := range cases {
		f := base
		c.mut(&f)
		if got := CleaningWords(f); got != c.want {
			t.Errorf("%s: = %q, want %q", c.name, got, c.want)
		}
	}
}
