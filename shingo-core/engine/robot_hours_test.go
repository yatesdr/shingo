package engine

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/store/robothours"
)

func connectedRobot(vid string) fleet.RobotStatus {
	return fleet.RobotStatus{VehicleID: vid, Connected: true, BatteryLevel: 80, OdoTotal: 1000, LiftCount: 500}
}

// TestRobotHours_PollDtCap pins dt: zero before the first poll, the real gap when it is
// short, and never more than two polls — an outage credits nobody.
func TestRobotHours_PollDtCap(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		last     time.Time
		now      time.Time
		wantSecs float64
	}{
		{"first poll after boot", time.Time{}, t0, 0},
		{"normal poll", t0, t0.Add(2 * time.Second), 2},
		{"late poll under the cap", t0, t0.Add(3 * time.Second), 3},
		{"fleet down ten minutes", t0, t0.Add(10 * time.Minute), 4},
		{"clock went backwards", t0, t0.Add(-time.Second), 0},
	}
	for _, c := range cases {
		if got := pollDt(c.last, c.now).Seconds(); got != c.wantSecs {
			t.Errorf("%s: dt = %vs, want %vs", c.name, got, c.wantSecs)
		}
	}
}

// TestObserve_Rules pins observe's rules, one row each.
func TestObserve_Rules(t *testing.T) {
	t.Parallel()
	dt := 2 * time.Second
	base := connectedRobot("AMR-01")
	with := func(f func(*fleet.RobotStatus)) fleet.RobotStatus {
		r := base
		f(&r)
		return r
	}
	alarm := func(code int, ts int64) []fleet.RobotAlarm {
		return []fleet.RobotAlarm{{Code: code, Severity: "error", Timestamp: ts}}
	}
	disconnected := with(func(r *fleet.RobotStatus) { r.Connected = false })

	cases := []struct {
		name string
		prev *fleet.RobotStatus
		cur  fleet.RobotStatus
		want hourObservation
	}{
		{"boot: first sight seeds, counts nothing but its levels",
			nil, with(func(r *fleet.RobotStatus) { r.Charging = true; r.Alarms = alarm(54901, 100) }),
			hourObservation{battery: 80, liftCount: 500}},
		{"reconnect: prev disconnected is a first sight",
			&disconnected, with(func(r *fleet.RobotStatus) { r.OdoTotal = 1500; r.Alarms = alarm(54901, 100) }),
			hourObservation{battery: 80, liftCount: 500}},
		{"disconnected reading accrues nothing",
			&base, with(func(r *fleet.RobotStatus) { r.Connected = false; r.OdoTotal = 1200; r.Alarms = alarm(54901, 0) }),
			hourObservation{}},
		{"connected, idle", &base, base,
			hourObservation{connected: dt, battery: 80, liftCount: 500}},
		{"charging and blocked accrue", &base, with(func(r *fleet.RobotStatus) { r.Charging = true; r.Blocked = true }),
			hourObservation{connected: dt, charging: dt, blocked: dt, chargeStart: true, battery: 80, liftCount: 500}},
		{"still charging is not a new start",
			robotPtr(with(func(r *fleet.RobotStatus) { r.Charging = true })), with(func(r *fleet.RobotStatus) { r.Charging = true }),
			hourObservation{connected: dt, charging: dt, battery: 80, liftCount: 500}},
		{"odo delta is metres", &base, with(func(r *fleet.RobotStatus) { r.OdoTotal = 1003.5 }),
			hourObservation{connected: dt, metres: 3.5, battery: 80, liftCount: 500}},
		{"negative odo is a reboot, ignored", &base, with(func(r *fleet.RobotStatus) { r.OdoTotal = 10 }),
			hourObservation{connected: dt, battery: 80, liftCount: 500}},
		{"OdoToday is never read", &base, with(func(r *fleet.RobotStatus) { r.OdoToday = 999 }),
			hourObservation{connected: dt, battery: 80, liftCount: 500}},
		{"battery and lift 0 skipped", &base, with(func(r *fleet.RobotStatus) { r.BatteryLevel = 0; r.LiftCount = 0 }),
			hourObservation{connected: dt}},
		{"onset: new code", &base, with(func(r *fleet.RobotStatus) { r.Alarms = alarm(54901, 100) }),
			hourObservation{connected: dt, onsets: []int{54901}, battery: 80, liftCount: 500}},
		{"latched, same timestamp: no onset",
			robotPtr(with(func(r *fleet.RobotStatus) { r.Alarms = alarm(54901, 100) })), with(func(r *fleet.RobotStatus) { r.Alarms = alarm(54901, 100) }),
			hourObservation{connected: dt, battery: 80, liftCount: 500}},
		{"latched, no timestamp: no onset",
			robotPtr(with(func(r *fleet.RobotStatus) { r.Alarms = alarm(54901, 0) })), with(func(r *fleet.RobotStatus) { r.Alarms = alarm(54901, 0) }),
			hourObservation{connected: dt, battery: 80, liftCount: 500}},
		{"re-raised: timestamp moved counts again",
			robotPtr(with(func(r *fleet.RobotStatus) { r.Alarms = alarm(54901, 100) })), with(func(r *fleet.RobotStatus) { r.Alarms = alarm(54901, 160) }),
			hourObservation{connected: dt, onsets: []int{54901}, battery: 80, liftCount: 500}},
		{"one code twice in the list is one onset", &base,
			with(func(r *fleet.RobotStatus) {
				r.Alarms = []fleet.RobotAlarm{{Code: 52101, Severity: "error"}, {Code: 52101, Severity: "warning"}}
			}),
			hourObservation{connected: dt, onsets: []int{52101}, battery: 80, liftCount: 500}},
	}
	for _, c := range cases {
		if got := observe(c.prev, c.cur, dt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func robotPtr(r fleet.RobotStatus) *fleet.RobotStatus { return &r }

// TestRobotHours_OnsetOncePerLatch drives the tap over a run of polls: a code
// latched across many polls is one onset, its clearance and return is a
// second, and first_at is the first one's poll time.
func TestRobotHours_OnsetOncePerLatch(t *testing.T) {
	t.Parallel()
	tap := newRobotHoursTap()
	t0 := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	r := connectedRobot("AMR-11")
	latched := r
	latched.Alarms = []fleet.RobotAlarm{{Code: 54901}}

	polls := []fleet.RobotStatus{r, latched, latched, latched, r, latched, latched}
	for i, p := range polls {
		tap.observePoll([]fleet.RobotStatus{p}, t0.Add(time.Duration(i)*2*time.Second))
	}
	ot := tap.buckets[t0].robots["AMR-11"].onsets[54901]
	if ot == nil || ot.n != 2 || !ot.first.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("54901 = %+v, want 2 onsets first at t0+2s", ot)
	}
	if got := tap.buckets[t0].robots["AMR-11"].connected; got != 12*time.Second {
		t.Errorf("connected = %v, want 12s (six credited polls, the first seeded)", got)
	}
}

// TestRobotHours_DroppedRobotReturnsAsFirstSight pins that a robot missing
// from a poll loses its prev, so its return — carrying an alarm and a jumped
// odometer — counts nothing.
func TestRobotHours_DroppedRobotReturnsAsFirstSight(t *testing.T) {
	t.Parallel()
	tap := newRobotHoursTap()
	t0 := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	a, b := connectedRobot("AMR-01"), connectedRobot("AMR-02")
	tap.observePoll([]fleet.RobotStatus{a, b}, t0)
	tap.observePoll([]fleet.RobotStatus{a}, t0.Add(2*time.Second))
	if _, ok := tap.prev["AMR-02"]; ok {
		t.Fatal("AMR-02 kept its prev after leaving the poll")
	}
	back := b
	back.OdoTotal += 400
	back.Alarms = []fleet.RobotAlarm{{Code: 54901}}
	tap.observePoll([]fleet.RobotStatus{a, back}, t0.Add(4*time.Second))
	tl := tap.buckets[t0].robots["AMR-02"]
	if tl.connected != 0 || tl.metres != 0 || len(tl.onsets) != 0 {
		t.Errorf("returning robot accrued %+v, want nothing", tl)
	}
}

// TestRobotHours_BucketRowsAndRollover pins the bookkeeping: polls land in the
// UTC hour of their time, rows are unique per key, a flush on rollover writes
// both hours and drops the closed one, and the open one keeps running.
func TestRobotHours_BucketRowsAndRollover(t *testing.T) {
	t.Parallel()
	tap := newRobotHoursTap()
	h0 := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	r := connectedRobot("AMR-01")
	alarmed := r
	alarmed.Alarms = []fleet.RobotAlarm{{Code: 54901}, {Code: 52101}}

	tap.observePoll([]fleet.RobotStatus{r}, h0.Add(59*time.Minute+56*time.Second))
	tap.lastAttempt = h0.Add(59 * time.Minute) // flushed recently: not due on cadence
	tap.failed = false
	tap.observePoll([]fleet.RobotStatus{alarmed}, h0.Add(59*time.Minute+58*time.Second))
	if tap.flushDue(h0.Add(59*time.Minute + 58*time.Second)) {
		t.Fatal("due inside the hour before the cadence")
	}
	h1 := h0.Add(time.Hour)
	tap.observePoll([]fleet.RobotStatus{alarmed}, h1)
	if !tap.flushDue(h1) {
		t.Fatal("not due after the hour closed")
	}

	var (
		wroteHours  []robothours.Hour
		wroteOnsets []robothours.Onset
		loaded      []time.Time
	)
	load := func(h time.Time) (map[string]robothours.Hour, map[string][]robothours.Onset, error) {
		loaded = append(loaded, h)
		return nil, nil, nil
	}
	write := func(h []robothours.Hour, o []robothours.Onset) error {
		wroteHours, wroteOnsets = h, o
		return nil
	}
	if err := tap.flush(h1, load, write); err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || !loaded[0].Equal(h0) || !loaded[1].Equal(h1) {
		t.Errorf("seeded %v, want both hours, oldest first", loaded)
	}
	if len(wroteHours) != 2 || wroteHours[0].ConnectedS != 2 || wroteHours[1].ConnectedS != 2 {
		t.Errorf("hours = %+v, want one row per hour of 2 s each", wroteHours)
	}
	seen := map[string]bool{}
	for _, o := range wroteOnsets {
		k := o.Hour.String() + o.VehicleID + string(rune(o.Code))
		if seen[k] {
			t.Errorf("duplicate onset key %+v", o)
		}
		seen[k] = true
	}
	if len(wroteOnsets) != 2 || !wroteOnsets[0].Hour.Equal(h0) {
		t.Errorf("onsets = %+v, want 52101 and 54901 once each in the closed hour", wroteOnsets)
	}
	if _, ok := tap.buckets[h0]; ok {
		t.Error("the closed hour was kept after a successful flush")
	}
	if b := tap.buckets[h1]; b == nil || !b.seeded {
		t.Error("the open hour was dropped or lost its seed")
	}
	if tap.flushDue(h1.Add(2 * time.Second)) {
		t.Error("due again on the next poll")
	}
}

// TestRobotHours_FailedSeedNeverWritesUnseeded pins the restart rule: a bucket
// whose stored row could not be read is not written (it would replace the
// stored totals with smaller ones), keeps counting, retries at the cadence, and
// on success writes stored + counted.
func TestRobotHours_FailedSeedNeverWritesUnseeded(t *testing.T) {
	t.Parallel()
	tap := newRobotHoursTap()
	h0 := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	r := connectedRobot("AMR-01")
	tap.observePoll([]fleet.RobotStatus{r}, h0.Add(10*time.Minute))
	r2 := r
	r2.OdoTotal += 50
	r2.Alarms = []fleet.RobotAlarm{{Code: 54901}}
	tap.observePoll([]fleet.RobotStatus{r2}, h0.Add(10*time.Minute+2*time.Second))

	wrote := false
	write := func([]robothours.Hour, []robothours.Onset) error { wrote = true; return nil }
	failLoad := func(time.Time) (map[string]robothours.Hour, map[string][]robothours.Onset, error) {
		return nil, nil, errors.New("db down")
	}
	now := h0.Add(10*time.Minute + 2*time.Second)
	if err := tap.flush(now, failLoad, write); err == nil || wrote {
		t.Fatalf("flush with a failed seed: err=%v wrote=%v, want error and no write", err, wrote)
	}
	if tap.flushDue(now.Add(2 * time.Second)) {
		t.Error("a failed flush is retried on the next poll, not the cadence")
	}
	if !tap.flushDue(now.Add(robotHoursFlushEvery)) {
		t.Error("a failed flush is not retried at the cadence")
	}

	bm, lc := 40.0, 480
	stored := map[string]robothours.Hour{"AMR-01": {Hour: h0, VehicleID: "AMR-01", ConnectedS: 600, Metres: 100,
		ChargeStarts: 1, BatteryMin: &bm, LiftCount: &lc}}
	storedOnsets := map[string][]robothours.Onset{"AMR-01": {{Hour: h0, VehicleID: "AMR-01", Code: 54901, Onsets: 3, FirstAt: h0.Add(time.Minute)}}}
	var got []robothours.Hour
	var gotOnsets []robothours.Onset
	err := tap.flush(now.Add(robotHoursFlushEvery),
		func(time.Time) (map[string]robothours.Hour, map[string][]robothours.Onset, error) {
			return stored, storedOnsets, nil
		},
		func(h []robothours.Hour, o []robothours.Onset) error { got, gotOnsets = h, o; return nil })
	if err != nil {
		t.Fatal(err)
	}
	g := got[0]
	if g.ConnectedS != 602 || g.Metres != 150 || g.ChargeStarts != 1 || *g.BatteryMin != 40 || *g.LiftCount != 500 {
		t.Errorf("merged row = %+v (battery %v lift %v), want stored + counted", g, *g.BatteryMin, *g.LiftCount)
	}
	if len(gotOnsets) != 1 || gotOnsets[0].Onsets != 4 || !gotOnsets[0].FirstAt.Equal(h0.Add(time.Minute)) {
		t.Errorf("merged onsets = %+v, want 4 with the stored first_at", gotOnsets)
	}
}
