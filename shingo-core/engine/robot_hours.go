// robot_hours.go — the hourly robot record, tapped off the existing robot poll.
//
// Core polls every robot every 2 seconds and, before this, kept none of it:
// the E-Maint snapshot is the cache of the last poll and nothing else. This
// keeps running totals per robot per UTC hour (connected, charging and blocked
// seconds, metres, charge starts, battery minimum, the lift counter last seen)
// and alarm ONSETS per code, and writes them to robot_hours and
// robot_alarm_onsets (store/robothours). It adds no polling.

package engine

import (
	"context"
	"sort"
	"time"

	"shingocore/fleet"
	"shingocore/store/robothours"
)

// robotHoursFlushEvery is how often the running totals are written while an
// hour is open. The hour's row is a running total, so this bounds what a crash
// can lose, not what the row will eventually say.
const robotHoursFlushEvery = 5 * time.Minute

// robotHoursFlushTimeout bounds one flush (the first-flush seed read and the
// upsert together). The caller is the 2-second robot poll, which the confidence
// sampler, the carried-bin sweep and every SSE robot update ride on; a database
// that cannot answer in a second costs one skipped flush, retried at the next
// cadence, rather than a stalled poll.
const robotHoursFlushTimeout = 1 * time.Second

// robotHoursTap is the hourly record's memory between polls.
//
// Single-writer by construction: every field is touched only from
// robotRefreshLoop's goroutine, the only caller of tapRobotHours. No lock, and
// none needed — but nothing else may reach in here without changing that.
type robotHoursTap struct {
	// prev is each robot's reading on the previous poll, for the edges
	// (onsets, charge starts) and the odometer delta. A robot missing from a
	// poll is dropped, so its return is a first sight.
	prev     map[string]fleet.RobotStatus
	lastPoll time.Time
	buckets  map[time.Time]*hourBucket
	// lastAttempt is when a flush was last tried; failed says it did not land.
	// A failed flush is retried at the 5-minute cadence, not on the next poll:
	// a database that is down would otherwise be asked once every 2 seconds,
	// each time for up to robotHoursFlushTimeout, on the poll goroutine.
	lastAttempt time.Time
	failed      bool
}

func newRobotHoursTap() *robotHoursTap {
	return &robotHoursTap{
		prev:    map[string]fleet.RobotStatus{},
		buckets: map[time.Time]*hourBucket{},
	}
}

// hourBucket is one UTC hour's running totals for every robot polled in it.
//
// seeded says the stored row for this hour has been merged in. Until it has,
// the bucket holds only what THIS process saw, and it is never written as it
// stands: the upsert replaces, so writing a partial total over the row a
// previous process left (a restart mid-hour) would shrink it.
type hourBucket struct {
	hour   time.Time
	robots map[string]*robotHourTally
	seeded bool
}

type robotHourTally struct {
	connected, charging, blocked time.Duration
	metres                       float64
	chargeStarts                 int
	batteryMin                   float64 // 0 = no reading yet
	liftCount                    int     // 0 = no reading yet
	onsets                       map[int]*onsetTally
}

type onsetTally struct {
	n     int
	first time.Time
}

// hourObservation is what one poll of one robot adds to its hour.
type hourObservation struct {
	connected, charging, blocked time.Duration
	metres                       float64
	chargeStart                  bool
	onsets                       []int
	battery                      float64 // 0 = no reading
	liftCount                    int     // 0 = no reading
}

// observe is the whole rule for one robot on one poll: prev is its reading on
// the previous poll (nil when it was not in it), dt the time this poll may
// credit.
//
// FIRST SIGHT COUNTS NOTHING. A robot with no previous reading, or whose
// previous reading was disconnected — boot, a reconnect, a robot back in the
// list — is seeded: no seconds, no metres, no onsets, no charge start. Every
// alarm a robot carries when Core first sees it would otherwise be counted as
// raised at that moment, and Springfield's Core restarts around fifteen times a
// week; the onset count is the trial's primary number. Its levels (battery,
// lift counter) are readings, not counts, and are kept.
//
// Only a connected reading accrues. A disconnected robot's report is RDS's
// last word about it, not a fact about now.
//
// ODO IS METRES. The tree never stated the unit. Read off the live record
// (2026-10-15 /robotsStatus at both plants): HK AMR-01's lifetime odo is
// 82,824 after ~152 days of runtime, and SPR AMR-07's today_odo is 3,345 by
// its evening — as kilometres that would be 3,345 km in one robot day. No
// route-length cross-check was done; nothing in the record has one. Metres
// come from OdoTotal deltas only: OdoToday resets on the robot's own GMT+8
// midnight, which is neither UTC nor the plant's. A negative delta is the
// counter restarting (a reboot) and is ignored rather than credited.
func observe(prev *fleet.RobotStatus, cur fleet.RobotStatus, dt time.Duration) hourObservation {
	var o hourObservation
	if !cur.Connected {
		return o
	}
	if cur.BatteryLevel > 0 {
		o.battery = cur.BatteryLevel
	}
	if cur.LiftCount > 0 {
		o.liftCount = cur.LiftCount
	}
	if prev == nil || !prev.Connected {
		return o
	}

	o.connected = dt
	if cur.Charging {
		o.charging = dt
	}
	if cur.Blocked {
		o.blocked = dt
	}
	if d := cur.OdoTotal - prev.OdoTotal; d > 0 {
		o.metres = d
	}
	o.chargeStart = cur.Charging && !prev.Charging

	// Onset: a code present now and absent before, or present in both with
	// the vendor's raise time moved (raised again between two polls, which a
	// present/absent diff cannot see). Zero Timestamp means the vendor did not
	// say, so a latched code with no timestamp counts once, at its onset.
	before := make(map[int]int64, len(prev.Alarms))
	for _, a := range prev.Alarms {
		if _, ok := before[a.Code]; !ok {
			before[a.Code] = a.Timestamp
		}
	}
	counted := map[int]bool{}
	for _, a := range cur.Alarms {
		if counted[a.Code] {
			continue
		}
		counted[a.Code] = true
		ts, was := before[a.Code]
		if !was || (a.Timestamp != 0 && ts != 0 && a.Timestamp != ts) {
			o.onsets = append(o.onsets, a.Code)
		}
	}
	return o
}

// pollDt is the time one poll may credit: since the last poll, capped at twice
// the poll interval. robotRefreshLoop skips while the fleet is down, so the
// first poll after an outage would otherwise credit every robot with the whole
// outage as connected. Zero before the first poll.
func pollDt(lastPoll, now time.Time) time.Duration {
	if lastPoll.IsZero() {
		return 0
	}
	dt := now.Sub(lastPoll)
	if dt < 0 {
		return 0
	}
	if limit := 2 * robotPollInterval; dt > limit {
		return limit
	}
	return dt
}

// observePoll folds one poll into the bucket of the UTC hour it lands in. A
// poll's dt is credited whole to that hour, so the hour boundary is off by at
// most 2×robotPollInterval — four seconds in an hour.
func (t *robotHoursTap) observePoll(robots []fleet.RobotStatus, now time.Time) {
	dt := pollDt(t.lastPoll, now)
	t.lastPoll = now
	hour := now.UTC().Truncate(time.Hour)
	b := t.buckets[hour]
	if b == nil {
		b = &hourBucket{hour: hour, robots: map[string]*robotHourTally{}}
		t.buckets[hour] = b
	}

	seen := make(map[string]bool, len(robots))
	for _, r := range robots {
		if r.VehicleID == "" {
			continue
		}
		seen[r.VehicleID] = true
		var prev *fleet.RobotStatus
		if p, ok := t.prev[r.VehicleID]; ok {
			prev = &p
		}
		b.add(r.VehicleID, observe(prev, r, dt), now)
		t.prev[r.VehicleID] = r
	}
	for vid := range t.prev {
		if !seen[vid] {
			delete(t.prev, vid)
		}
	}
}

// add credits one observation. Every polled robot gets a tally, connected or
// not, so an hour in which a robot was listed but never connected is a row of
// zeros rather than no row: "polled and down" and "not polled" differ.
func (b *hourBucket) add(vid string, o hourObservation, now time.Time) {
	t := b.robots[vid]
	if t == nil {
		t = &robotHourTally{onsets: map[int]*onsetTally{}}
		b.robots[vid] = t
	}
	t.connected += o.connected
	t.charging += o.charging
	t.blocked += o.blocked
	t.metres += o.metres
	if o.chargeStart {
		t.chargeStarts++
	}
	if o.battery > 0 && (t.batteryMin == 0 || o.battery < t.batteryMin) {
		t.batteryMin = o.battery
	}
	if o.liftCount > 0 {
		t.liftCount = o.liftCount
	}
	for _, code := range o.onsets {
		if ot := t.onsets[code]; ot != nil {
			ot.n++
		} else {
			t.onsets[code] = &onsetTally{n: 1, first: now}
		}
	}
}

// seed merges the stored row for this hour into what this process counted:
// sums add, the battery minimum is the lower, the lift counter keeps this
// process's later reading, onsets add and keep the earlier first_at.
func (b *hourBucket) seed(hours map[string]robothours.Hour, onsets map[string][]robothours.Onset) {
	for vid, h := range hours {
		t := b.robots[vid]
		if t == nil {
			t = &robotHourTally{onsets: map[int]*onsetTally{}}
			b.robots[vid] = t
		}
		t.connected += time.Duration(h.ConnectedS) * time.Second
		t.charging += time.Duration(h.ChargingS) * time.Second
		t.blocked += time.Duration(h.BlockedS) * time.Second
		t.metres += h.Metres
		t.chargeStarts += h.ChargeStarts
		if h.BatteryMin != nil && *h.BatteryMin > 0 && (t.batteryMin == 0 || *h.BatteryMin < t.batteryMin) {
			t.batteryMin = *h.BatteryMin
		}
		if t.liftCount == 0 && h.LiftCount != nil {
			t.liftCount = *h.LiftCount
		}
	}
	for vid, list := range onsets {
		t := b.robots[vid]
		if t == nil {
			t = &robotHourTally{onsets: map[int]*onsetTally{}}
			b.robots[vid] = t
		}
		for _, o := range list {
			if ot := t.onsets[o.Code]; ot != nil {
				ot.n += o.Onsets
				if o.FirstAt.Before(ot.first) {
					ot.first = o.FirstAt
				}
			} else {
				t.onsets[o.Code] = &onsetTally{n: o.Onsets, first: o.FirstAt}
			}
		}
	}
	b.seeded = true
}

// rows renders the bucket as the store's rows. One per robot and one per
// (robot, code): the keys are map keys, so they are unique, which the
// multi-row upsert requires. Sorted so a flush is deterministic.
func (b *hourBucket) rows() ([]robothours.Hour, []robothours.Onset) {
	vids := make([]string, 0, len(b.robots))
	for vid := range b.robots {
		vids = append(vids, vid)
	}
	sort.Strings(vids)
	var (
		hours  []robothours.Hour
		onsets []robothours.Onset
	)
	for _, vid := range vids {
		t := b.robots[vid]
		h := robothours.Hour{
			Hour:         b.hour,
			VehicleID:    vid,
			ConnectedS:   int(t.connected.Round(time.Second) / time.Second),
			ChargingS:    int(t.charging.Round(time.Second) / time.Second),
			BlockedS:     int(t.blocked.Round(time.Second) / time.Second),
			Metres:       t.metres,
			ChargeStarts: t.chargeStarts,
		}
		if t.batteryMin > 0 {
			v := t.batteryMin
			h.BatteryMin = &v
		}
		if t.liftCount > 0 {
			v := t.liftCount
			h.LiftCount = &v
		}
		hours = append(hours, h)
		codes := make([]int, 0, len(t.onsets))
		for c := range t.onsets {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for _, c := range codes {
			ot := t.onsets[c]
			onsets = append(onsets, robothours.Onset{Hour: b.hour, VehicleID: vid, Code: c, Onsets: ot.n, FirstAt: ot.first})
		}
	}
	return hours, onsets
}

// flushDue reports whether this poll should write. Every robotHoursFlushEvery,
// and at once when an hour has closed — unless the last attempt failed, in
// which case only the cadence retries it.
func (t *robotHoursTap) flushDue(now time.Time) bool {
	if now.Sub(t.lastAttempt) >= robotHoursFlushEvery {
		return true
	}
	if t.failed {
		return false
	}
	current := now.UTC().Truncate(time.Hour)
	for h := range t.buckets {
		if h.Before(current) {
			return true
		}
	}
	return false
}

// flush writes every bucket in one call to write, seeding any not yet seeded
// with load first. Closed hours (before now's) are dropped once written; the
// open hour stays, its totals still running.
//
// THE SEED READ HAPPENS AT A BUCKET'S FIRST FLUSH, NOT ITS FIRST POLL. The
// stored row cannot change underneath this process (it is the only writer, and
// the row it would read was left by a previous one), so merging it in at the
// first flush gives the same total as seeding at first touch, keeps the read
// off every other poll, and makes a failed read cost nothing but a later
// write: the bucket keeps counting and is never written unseeded.
func (t *robotHoursTap) flush(now time.Time,
	load func(hour time.Time) (map[string]robothours.Hour, map[string][]robothours.Onset, error),
	write func([]robothours.Hour, []robothours.Onset) error) error {
	t.lastAttempt = now
	t.failed = true

	order := make([]time.Time, 0, len(t.buckets))
	for h := range t.buckets {
		order = append(order, h)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Before(order[j]) })

	var (
		hours  []robothours.Hour
		onsets []robothours.Onset
	)
	for _, h := range order {
		b := t.buckets[h]
		if !b.seeded {
			sh, so, err := load(h)
			if err != nil {
				return err
			}
			b.seed(sh, so)
		}
		bh, bo := b.rows()
		hours = append(hours, bh...)
		onsets = append(onsets, bo...)
	}
	if err := write(hours, onsets); err != nil {
		return err
	}
	t.failed = false
	current := now.UTC().Truncate(time.Hour)
	for _, h := range order {
		if h.Before(current) {
			delete(t.buckets, h)
		}
	}
	return nil
}

// tapRobotHours folds one poll into the hourly record and writes it when due.
// Called from robotRefreshLoop, which skips while the fleet is down.
func (e *Engine) tapRobotHours(robots []fleet.RobotStatus, now time.Time) {
	if e.cfg == nil || e.db == nil {
		return
	}
	if e.robotHours == nil {
		e.robotHours = newRobotHoursTap()
	}
	t := e.robotHours
	t.observePoll(robots, now)
	if !t.flushDue(now) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), robotHoursFlushTimeout)
	defer cancel()
	db := e.db.DB
	err := t.flush(now,
		func(hour time.Time) (map[string]robothours.Hour, map[string][]robothours.Onset, error) {
			return robothours.LoadHour(ctx, db, hour)
		},
		func(h []robothours.Hour, o []robothours.Onset) error {
			return robothours.Flush(ctx, db, h, o)
		})
	if err != nil {
		e.dbg("engine: robot hours: flush: %v", err)
	}
}
