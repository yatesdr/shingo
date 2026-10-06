//go:build docker

package www

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/store"
)

// Pins for the four Overview numbers the owner ruled on (R1–R4): fleet
// utilization, peak concurrency, the stuck-order count on the alert line, and
// the cancel-origin split on the Cancelled tile. One synthetic fixture per
// endpoint, built from raw rows so every timestamp is chosen, not inherited from
// whatever the lifecycle happened to stamp.
//
// The expected values are worked out by hand in the test bodies; each one says
// which robot or order produced it.

// fleetRobotsEngine is the real ServiceAccess with the robot cache replaced, so
// the fleet size and the robot ids are the fixture's rather than whatever the
// simulator's poll last wrote.
type fleetRobotsEngine struct {
	ServiceAccess
	robots []fleet.RobotStatus
}

func (e fleetRobotsEngine) GetAllCachedRobots() []fleet.RobotStatus { return e.robots }

// seedPinOrder inserts one order row with chosen timestamps and returns its id.
func seedPinOrder(t *testing.T, db *store.DB, uuid, status, robot string, created time.Time, completed *time.Time) int64 {
	t.Helper()
	var id int64
	err := db.DB.QueryRow(`INSERT INTO orders (edge_uuid, station_id, status, robot_id, created_at, updated_at, completed_at)
		VALUES ($1, 'pin-station', $2, $3, $4, $4, $5) RETURNING id`,
		uuid, status, robot, created, completed).Scan(&id)
	testutil.MustNoErr(t, err, "seed order "+uuid)
	return id
}

// seedPinHistory appends one order_history row at a chosen instant.
func seedPinHistory(t *testing.T, db *store.DB, orderID int64, status, detail string, at time.Time) {
	t.Helper()
	seedPinHistoryCode(t, db, orderID, status, detail, "", at)
}

// seedPinHistoryCode is seedPinHistory with the terminal code the writer sets.
func seedPinHistoryCode(t *testing.T, db *store.DB, orderID int64, status, detail, code string, at time.Time) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO order_history (order_id, status, detail, code, created_at) VALUES ($1, $2, $3, $4, $5)`,
		orderID, status, detail, code, at)
	testutil.MustNoErr(t, err, "seed history")
}

// seedPinMission writes the mission_telemetry summary row a finished mission has.
func seedPinMission(t *testing.T, db *store.DB, orderID int64, robot string, created, completed time.Time, durationMS int64) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO mission_telemetry (order_id, robot_id, station_id, terminal_state, core_created, core_completed, duration_ms)
		VALUES ($1, $2, 'pin-station', 'FINISHED', $3, $4, $5)`,
		orderID, robot, created, completed, durationMS)
	testutil.MustNoErr(t, err, "seed mission")
}

// seedFinishedMission is one robot mission end to end: queued, picked up at
// start, delivered at end, confirmed a few seconds later.
func seedFinishedMission(t *testing.T, db *store.DB, uuid, robot string, start, end time.Time) {
	t.Helper()
	queued := start.Add(-time.Minute)
	confirmed := end.Add(5 * time.Second)
	id := seedPinOrder(t, db, uuid, "confirmed", robot, queued, &confirmed)
	seedPinHistory(t, db, id, "queued", "", queued)
	seedPinHistory(t, db, id, "in_transit", "", start)
	seedPinHistory(t, db, id, "delivered", "", end)
	seedPinHistory(t, db, id, "confirmed", "", confirmed)
	seedPinMission(t, db, id, robot, queued, confirmed, confirmed.Sub(queued).Milliseconds())
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// overviewFleetFixture seeds the R1/R2 day on 2026-03-10 (plant time), a day
// wholly in the past so every window edge is fixed:
//
//	R2  finished 01:10:00–01:20:00 (600 s), then held on an OPEN order:
//	    in_transit 02:00, staged 02:30, never released.
//	R1  finished 01:30:00–01:31:15 (75 s) and 10:00:00–10:01:15 (75 s).
//	R3  finished 01:40:00–01:41:00 (60 s).
//
// Hour 01 is touched by all three robots, but never by two at once. Two more
// missions cross a window edge, so the busy figures are seen clipped to it:
//
//	R3  in_transit 03-09 23:50, delivered 00:20 — crosses the day's start.
//	R1  in_transit 23:45, delivered 03-11 00:05 — crosses the day's end,
//	    and the week's (FleetWeek ends on the 10th).
func overviewFleetFixture(t *testing.T) (*Handlers, time.Time) {
	t.Helper()
	h, db := testHandlers(t)
	h.engine = fleetRobotsEngine{ServiceAccess: h.engine, robots: []fleet.RobotStatus{
		{VehicleID: "R1", Connected: true},
		{VehicleID: "R2", Connected: true},
		{VehicleID: "R3", Connected: true},
	}}

	day := time.Date(2026, 3, 10, 0, 0, 0, 0, plantLocation)
	at := func(hh, mm, ss int) time.Time {
		return day.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute + time.Duration(ss)*time.Second)
	}

	seedFinishedMission(t, db, "pin-r2-a", "R2", at(1, 10, 0), at(1, 20, 0))
	seedFinishedMission(t, db, "pin-r1-a", "R1", at(1, 30, 0), at(1, 31, 15))
	seedFinishedMission(t, db, "pin-r3-a", "R3", at(1, 40, 0), at(1, 41, 0))
	seedFinishedMission(t, db, "pin-r1-b", "R1", at(10, 0, 0), at(10, 1, 15))
	seedFinishedMission(t, db, "pin-r3-cross-start", "R3", at(-1, 50, 0), at(0, 20, 0))
	seedFinishedMission(t, db, "pin-r1-cross-end", "R1", at(23, 45, 0), at(24, 5, 0))

	held := seedPinOrder(t, db, "pin-r2-held", "staged", "R2", at(1, 55, 0), nil)
	seedPinHistory(t, db, held, "queued", "", at(1, 55, 0))
	seedPinHistory(t, db, held, "in_transit", "", at(2, 0, 0))
	seedPinHistory(t, db, held, "staged", "", at(2, 30, 0))
	return h, day
}

type fleetResponse struct {
	Fleet struct {
		Size            int64   `json:"size"`
		Missions        int64   `json:"missions"`
		AvgLoad         float64 `json:"avg_load"`
		PeakConcurrency int64   `json:"peak_concurrency"`
		PeakHour        string  `json:"peak_hour"`
		UtilPct         float64 `json:"util_pct"`
		CeilingReached  bool    `json:"ceiling_reached"`
	} `json:"fleet"`
	LoadGranularity string `json:"load_granularity"`
	LoadSeries      []struct {
		Hour        *time.Time `json:"hour"`
		Concurrency int64      `json:"concurrency"`
		Day         *time.Time `json:"day"`
		Peak        int64      `json:"peak"`
		Avg         float64    `json:"avg"`
	} `json:"load_series"`
	Robots []struct {
		VehicleID string  `json:"vehicle_id"`
		UtilPct   float64 `json:"util_pct"`
		Missions  int64   `json:"missions"`
		BusyMS    int64   `json:"busy_ms"`
	} `json:"robots"`
}

func getFleet(t *testing.T, h *Handlers, query string) fleetResponse {
	t.Helper()
	rec := getPlain(t, h.apiRobotsFleet, "/api/robots/fleet?"+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out fleetResponse
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&out), "decode fleet")
	return out
}

// TestOverviewPin_FleetDay pins /api/robots/fleet over the one-day window.
//
// The window is since=until=2026-03-10, which parseMissionFilter turns into
// [00:00, 24:00 − 1ns): 86,399,999 ms once Milliseconds() truncates.
func TestOverviewPin_FleetDay(t *testing.T) {
	t.Parallel()
	h, _ := overviewFleetFixture(t)
	got := getFleet(t, h, "since=2026-03-10&until=2026-03-10")

	const windowMS = 86_399_999.0

	// Per robot. busy_ms is time on an order: R2's 600 s finished mission plus
	// the 22 hours (02:00 to the window's end) it was held on the open order
	// (R2). A mission crossing an edge counts only inside the window: R3's
	// 00:00–00:20 and R1's 23:45–24:00. missions counts what finished in the
	// window, so R3's crossing mission is one and R1's is not.
	wantBusy := map[string]int64{"R1": 150_000 + 900_000, "R2": 600_000 + 79_200_000, "R3": 60_000 + 1_200_000}
	wantMissions := map[string]int64{"R1": 2, "R2": 1, "R3": 2}
	for _, r := range got.Robots {
		if r.BusyMS != wantBusy[r.VehicleID] {
			t.Errorf("%s busy_ms = %d, want %d", r.VehicleID, r.BusyMS, wantBusy[r.VehicleID])
		}
		if r.Missions != wantMissions[r.VehicleID] {
			t.Errorf("%s missions = %d, want %d", r.VehicleID, r.Missions, wantMissions[r.VehicleID])
		}
		if want := float64(wantBusy[r.VehicleID]) / windowMS * 100; !almostEqual(r.UtilPct, want) {
			t.Errorf("%s util_pct = %v, want %v", r.VehicleID, r.UtilPct, want)
		}
	}
	if len(got.Robots) != 3 {
		t.Fatalf("robots = %d rows, want 3", len(got.Robots))
	}

	// Hourly curve: the most robots on an order at one instant (R1). Hour 00
	// has R3's crossing mission; hour 01 is touched by all three but never by
	// two at once, so 1; from 02:00 R2 is held (R2), so every hour is at least
	// 1; at 10:00 and from 23:45 R1 runs beside it, 2. The peak is the first.
	if got.LoadGranularity != "hour" || len(got.LoadSeries) != 24 {
		t.Fatalf("load series = %s × %d, want hour × 24", got.LoadGranularity, len(got.LoadSeries))
	}
	wantConc := make([]int64, 24)
	for i := 0; i < 24; i++ {
		wantConc[i] = 1
	}
	wantConc[10], wantConc[23] = 2, 2
	for i, p := range got.LoadSeries {
		if p.Concurrency != wantConc[i] {
			t.Errorf("hour %02d concurrency = %d, want %d", i, p.Concurrency, wantConc[i])
		}
	}

	f := got.Fleet
	if f.Size != 3 || f.Missions != 5 {
		t.Errorf("size/missions = %d/%d, want 3/5", f.Size, f.Missions)
	}
	if f.PeakConcurrency != 2 || f.PeakHour != "10:00" || f.CeilingReached {
		t.Errorf("peak = %d @ %s ceiling=%v, want 2 @ 10:00 ceiling=false", f.PeakConcurrency, f.PeakHour, f.CeilingReached)
	}
	// util = Σ busy ÷ (robots × window) (R1); avg_load is the same in robots.
	const fleetBusy = 1_050_000 + 79_800_000 + 1_260_000
	if want := fleetBusy / windowMS; !almostEqual(f.AvgLoad, want) {
		t.Errorf("avg_load = %v, want %v", f.AvgLoad, want)
	}
	if want := fleetBusy / (3 * windowMS) * 100; !almostEqual(f.UtilPct, want) {
		t.Errorf("util_pct = %v, want %v", f.UtilPct, want)
	}
}

// TestOverviewPin_FleetWeek pins the multi-day (daily rollup) path over the
// same fixture: 2026-03-04..2026-03-10.
func TestOverviewPin_FleetWeek(t *testing.T) {
	t.Parallel()
	h, _ := overviewFleetFixture(t)
	got := getFleet(t, h, "since=2026-03-04&until=2026-03-10")

	if got.LoadGranularity != "day" {
		t.Fatalf("granularity = %s, want day", got.LoadGranularity)
	}
	f := got.Fleet
	// Peak: R2 held plus R1 at 10:00 on the 10th (R1 + R2).
	if f.PeakConcurrency != 2 || f.CeilingReached {
		t.Errorf("peak = %d ceiling=%v, want 2 ceiling=false", f.PeakConcurrency, f.CeilingReached)
	}
	// util = Σ busy ÷ (robots × seven days − 1 ns) (R1 + R2). R3's crossing
	// mission is wholly inside the week; R1's counts its 15 minutes before the
	// week's end.
	const weekMS = 7*86_400_000.0 - 1
	const weekBusy = 80_010_000 + 1_800_000 + 900_000
	if want := weekBusy / (3 * weekMS) * 100; !almostEqual(f.UtilPct, want) {
		t.Errorf("util_pct = %v, want %v", f.UtilPct, want)
	}
	// The 9th's average is R3's 600 s before midnight; the 10th's is its
	// robot-seconds on orders over the day: 810 s finished + 79,200 s held +
	// R3's 1,200 s after midnight + R1's 900 s before the next.
	if n := len(got.LoadSeries); n != 7 {
		t.Fatalf("day buckets = %d, want 7", n)
	}
	wantAvg := map[int]float64{5: 600.0 / 86_400, 6: 82_110.0 / 86_400}
	var sumAvg float64
	for i, d := range got.LoadSeries {
		if !almostEqual(d.Avg, wantAvg[i]) {
			t.Errorf("day %d avg = %v, want %v", i, d.Avg, wantAvg[i])
		}
		sumAvg += d.Avg
	}
	// One interval set, at both edges: the week's avg_load is the mean of its
	// daily averages, and a day's avg_load is that day's daily average, on a
	// day with a mission crossing each of its edges.
	if want := sumAvg / 7; !almostEqual(f.AvgLoad, want) {
		t.Errorf("week avg_load = %v, but the daily averages' mean = %v", f.AvgLoad, want)
	}
	day := getFleet(t, h, "since=2026-03-10&until=2026-03-10")
	if got, want := day.Fleet.AvgLoad, got.LoadSeries[6].Avg; !almostEqual(got, want) {
		t.Errorf("10th avg_load = %v, but the 10th's daily avg = %v", got, want)
	}
}

// overviewOverlapFixture seeds one robot on two orders at once, on 2026-03-22
// (plant time, no DST change in the day or the week before it):
//
//	R1  open order A: in_transit 02:00, staged 02:30, never released;
//	    open order B: in_transit 03:00, never finished. Both open to the
//	    window's end, so 03:00–24:00 is covered twice;
//	    open order N: dispatched 01:00 and never acknowledged, so no robot
//	    has taken it and it is not busy time.
//	R2  a finished mission 03:30–03:50 that faulted 03:35–03:40; then
//	    open order C: in_transit 05:00, staged 05:30, with a finished
//	    mission 06:00–06:10 inside it.
//	R3  open order D: in_transit 08:00, faulted 09:00, in_transit 09:30,
//	    staged 10:00; a finished mission 09:10–09:20 inside the fault.
//
// Nothing in Core stops a robot holding two open orders: an order whose
// terminal report was lost stays open while the robot runs the next one.
func overviewOverlapFixture(t *testing.T) *Handlers {
	t.Helper()
	h, db := testHandlers(t)
	h.engine = fleetRobotsEngine{ServiceAccess: h.engine, robots: []fleet.RobotStatus{
		{VehicleID: "R1", Connected: true},
		{VehicleID: "R2", Connected: true},
		{VehicleID: "R3", Connected: true},
	}}
	day := time.Date(2026, 3, 22, 0, 0, 0, 0, plantLocation)
	at := func(hh, mm int) time.Time {
		return day.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
	}
	open := func(uuid, robot, status string, steps ...any) {
		id := seedPinOrder(t, db, uuid, status, robot, at(0, 30), nil)
		seedPinHistory(t, db, id, "queued", "", at(0, 30))
		for i := 0; i < len(steps); i += 2 {
			seedPinHistory(t, db, id, steps[i].(string), "", steps[i+1].(time.Time))
		}
	}
	open("ov-r1-a", "R1", "staged", "in_transit", at(2, 0), "staged", at(2, 30))
	open("ov-r1-b", "R1", "in_transit", "in_transit", at(3, 0))
	open("ov-r1-n", "R1", "dispatched", "dispatched", at(1, 0))
	_, err := db.DB.Exec(`UPDATE orders SET vendor_order_id = 'pin-vendor-n' WHERE edge_uuid = 'ov-r1-n'`)
	testutil.MustNoErr(t, err, "vendor id on the dispatched order")
	faultedMission := seedPinOrder(t, db, "ov-r2-f", "confirmed", "R2", at(3, 29), nil)
	for _, s := range []struct {
		status string
		at     time.Time
	}{{"queued", at(3, 29)}, {"in_transit", at(3, 30)}, {"faulted", at(3, 35)}, {"in_transit", at(3, 40)}, {"delivered", at(3, 50)}, {"confirmed", at(3, 51)}} {
		seedPinHistory(t, db, faultedMission, s.status, "", s.at)
	}
	seedPinMission(t, db, faultedMission, "R2", at(3, 29), at(3, 51), 22*60_000)
	open("ov-r2-c", "R2", "staged", "in_transit", at(5, 0), "staged", at(5, 30))
	seedFinishedMission(t, db, "ov-r2-m", "R2", at(6, 0), at(6, 10))
	open("ov-r3-d", "R3", "staged", "in_transit", at(8, 0), "faulted", at(9, 0), "in_transit", at(9, 30), "staged", at(10, 0))
	seedFinishedMission(t, db, "ov-r3-m", "R3", at(9, 10), at(9, 20))
	return h
}

// TestOverviewPin_OverlapIsCountedOnce pins one robot on two overlapping
// intervals. A robot is busy or it is not: two orders at once are not twice the
// work, so busy time is the union of a robot's intervals, not their sum, and the
// daily average is the union per hour. Faulted time is busy (R9): a faulted
// robot is on its order and cannot take other work. Every figure reads the same
// intervals, so the day's avg_load is the day's average.
func TestOverviewPin_OverlapIsCountedOnce(t *testing.T) {
	t.Parallel()
	h := overviewOverlapFixture(t)
	const windowMS = 86_399_999.0

	day := getFleet(t, h, "since=2026-03-22&until=2026-03-22")
	// The union, not the sum. R1 02:00–24:00 once (B lies inside A, N was never
	// acknowledged); R2 the faulted mission's 20 min + 05:00–24:00 (the 06:00
	// mission lies inside C); R3 08:00–24:00, its fault and the 09:10 mission
	// inside it included.
	wantBusy := map[string]int64{"R1": 79_200_000, "R2": 69_600_000, "R3": 57_600_000}
	for _, r := range day.Robots {
		if r.BusyMS != wantBusy[r.VehicleID] {
			t.Errorf("day %s busy_ms = %d, want %d", r.VehicleID, r.BusyMS, wantBusy[r.VehicleID])
		}
		if want := float64(wantBusy[r.VehicleID]) / windowMS * 100; !almostEqual(r.UtilPct, want) {
			t.Errorf("day %s util_pct = %v, want %v", r.VehicleID, r.UtilPct, want)
		}
	}
	// Peak counts robots, not intervals: R1's two orders are one robot.
	if f := day.Fleet; f.PeakConcurrency != 3 || f.PeakHour != "08:00" || !f.CeilingReached {
		t.Errorf("peak = %d @ %s ceiling=%v, want 3 @ 08:00 ceiling=true", f.PeakConcurrency, f.PeakHour, f.CeilingReached)
	}
	wantConc := []int64{0, 0, 1, 2, 1, 2, 2, 2}
	for i, p := range day.LoadSeries {
		want := int64(3)
		if i < len(wantConc) {
			want = wantConc[i]
		}
		if p.Concurrency != want {
			t.Errorf("hour %02d concurrency = %d, want %d", i, p.Concurrency, want)
		}
	}
	const dayFleetBusy = 79_200_000 + 69_600_000 + 57_600_000
	if want := dayFleetBusy / (3 * windowMS) * 100; !almostEqual(day.Fleet.UtilPct, want) {
		t.Errorf("day util_pct = %v, want %v", day.Fleet.UtilPct, want)
	}

	week := getFleet(t, h, "since=2026-03-16&until=2026-03-22")
	const weekMS = 7*86_400_000.0 - 1
	if want := 206_400_000 / (3 * weekMS) * 100; !almostEqual(week.Fleet.UtilPct, want) {
		t.Errorf("week util_pct = %v, want %v", week.Fleet.UtilPct, want)
	}
	// The 22nd's average: each robot's merged seconds in the day. R1 79,200;
	// R2 1,200 + 68,400; R3 57,600.
	if n := len(week.LoadSeries); n != 7 {
		t.Fatalf("day buckets = %d, want 7", n)
	}
	if got, want := week.LoadSeries[6].Avg, 206_400.0/86_400; !almostEqual(got, want) {
		t.Errorf("22nd avg = %v, want %v", got, want)
	}
	if p := week.LoadSeries[6].Peak; p != 3 {
		t.Errorf("22nd peak = %d, want 3", p)
	}
	// One interval set: the day card's avg_load and the 22nd's daily average
	// are the same robot-time, over a window 1 ms shorter.
	if got, want := day.Fleet.AvgLoad, week.LoadSeries[6].Avg; !almostEqual(got, want) {
		t.Errorf("day avg_load = %v, but the 22nd's daily avg = %v", got, want)
	}
}

// TestOverviewPin_StuckAlerts pins /api/missions/alerts' stuck count.
//
// Times are relative to now because the handler reads the live clock. The
// history that sets the thresholds is five confirmed orders two-plus days ago:
// queued→dispatched 2 min, in_transit→delivered 10 min, lead time 30 min.
//
//	A  in_transit for 30 min, created 35 min ago
//	B  created 3 h ago, entered in_transit 1 min ago
//	C  staged for 8 h 40 min (the held robot), created 9 h ago
//	D  queued for 10 min, created 10 min ago
func TestOverviewPin_StuckAlerts(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	now := time.Now().Truncate(time.Microsecond) // Postgres keeps microseconds

	for i := 0; i < 5; i++ {
		t0 := now.Add(-48*time.Hour - time.Duration(i)*time.Hour)
		done := t0.Add(30 * time.Minute)
		id := seedPinOrder(t, db, "pin-dwell-"+string(rune('a'+i)), "confirmed", "R9", t0, &done)
		seedPinHistory(t, db, id, "queued", "", t0)
		seedPinHistory(t, db, id, "dispatched", "", t0.Add(2*time.Minute))
		seedPinHistory(t, db, id, "in_transit", "", t0.Add(2*time.Minute+10*time.Second))
		seedPinHistory(t, db, id, "delivered", "", t0.Add(12*time.Minute+10*time.Second))
		seedPinHistory(t, db, id, "confirmed", "", done)
		seedPinMission(t, db, id, "R9", t0, done, done.Sub(t0).Milliseconds())
	}

	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	a := seedPinOrder(t, db, "pin-stuck-a", "in_transit", "R1", ago(35*time.Minute), nil)
	seedPinHistory(t, db, a, "queued", "", ago(35*time.Minute))
	seedPinHistory(t, db, a, "dispatched", "", ago(33*time.Minute))
	seedPinHistory(t, db, a, "in_transit", "", ago(30*time.Minute))

	b := seedPinOrder(t, db, "pin-stuck-b", "in_transit", "R3", ago(3*time.Hour), nil)
	seedPinHistory(t, db, b, "queued", "", ago(3*time.Hour))
	seedPinHistory(t, db, b, "dispatched", "", ago(3*time.Hour-time.Minute))
	seedPinHistory(t, db, b, "in_transit", "", ago(time.Minute))

	c := seedPinOrder(t, db, "pin-stuck-c", "staged", "R2", ago(9*time.Hour), nil)
	seedPinHistory(t, db, c, "queued", "", ago(9*time.Hour))
	seedPinHistory(t, db, c, "in_transit", "", ago(8*time.Hour+50*time.Minute))
	seedPinHistory(t, db, c, "staged", "", ago(8*time.Hour+40*time.Minute))

	d := seedPinOrder(t, db, "pin-stuck-d", "queued", "", ago(10*time.Minute), nil)
	seedPinHistory(t, db, d, "queued", "", ago(10*time.Minute))

	rec := getPlain(t, h.apiMissionsAlerts, "/api/missions/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		StuckMissions int `json:"stuck_missions"`
		StuckItems    []struct {
			OrderID     int64     `json:"order_id"`
			Status      string    `json:"status"`
			StatusSince time.Time `json:"status_since"`
			InStatusMS  int64     `json:"in_status_ms"`
			ThresholdMS int64     `json:"threshold_ms"`
		} `json:"stuck_items"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&got), "decode alerts")

	// Stuck = in the current status, with no transition, for more than twice
	// that status's 7-day p95 dwell (R3):
	//	A  in_transit 30 min > 2 × 10 min transit           → stuck
	//	B  in_transit 1 min                                   → moving
	//	C  staged 8 h 40 min; no staged samples, 30 min floor → stuck
	//	D  queued 10 min > 2 × 2 min time-to-dispatch         → stuck
	wantThreshold := map[int64]time.Duration{a: 20 * time.Minute, c: 30 * time.Minute, d: 4 * time.Minute}
	wantSince := map[int64]time.Duration{a: 30 * time.Minute, c: 8*time.Hour + 40*time.Minute, d: 10 * time.Minute}
	want := []int64{a, c, d}
	var ids []int64
	for _, it := range got.StuckItems {
		ids = append(ids, it.OrderID)
		if it.ThresholdMS != wantThreshold[it.OrderID].Milliseconds() {
			t.Errorf("order %d threshold_ms = %d, want %d", it.OrderID, it.ThresholdMS, wantThreshold[it.OrderID].Milliseconds())
		}
		if !it.StatusSince.Equal(ago(wantSince[it.OrderID])) {
			t.Errorf("order %d status_since = %v, want %v", it.OrderID, it.StatusSince, ago(wantSince[it.OrderID]))
		}
		if it.InStatusMS < wantSince[it.OrderID].Milliseconds() {
			t.Errorf("order %d in_status_ms = %d, want ≥ %d", it.OrderID, it.InStatusMS, wantSince[it.OrderID].Milliseconds())
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if got.StuckMissions != len(want) || !equalIDs(ids, want) {
		t.Errorf("stuck = %d %v, want %d %v (A=%d B=%d C=%d D=%d)", got.StuckMissions, ids, len(want), want, a, b, c, d)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOverviewPin_CancelSplit pins the Cancelled tile's origin split
// (/api/missions/stats/v2) over cancels on 2026-03-10, one per kind of writer,
// each carrying the order_history.code its real writer sets: a person, the
// fleet, two of Core's own teardowns, an Edge changeover and the compound child
// it cascades to, a swap peer, and an Edge changeover written before the code
// column existed.
func TestOverviewPin_CancelSplit(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, plantLocation)

	for i, c := range []struct{ detail, code string }{
		{"cancelled by admin", "operator_cancelled"},
		{"fleet order stopped", ""},
		{"reshuffle dissolved: the dig's plan went stale; re-planning", ""},
		{"abandoned: stuck in staged past 30m0s", ""},
		{"changeover cancelled: the keep-staged spot goes back to the outgoing style", "operator_cancelled"},
		{"parent order cancelled: changeover cancelled: the keep-staged spot goes back to the outgoing style", "operator_cancelled"},
		{"coordinated swap supply (order 7) cancelled; the evac leg cannot go alone", "peer_terminal"},
		{"changeover cancelled: the keep-staged spot goes back to the outgoing style", ""},
	} {
		done := at.Add(time.Duration(i) * time.Minute)
		id := seedPinOrder(t, db, "pin-cancel-"+string(rune('a'+i)), "cancelled", "", at, &done)
		seedPinHistory(t, db, id, "queued", "", at)
		seedPinHistoryCode(t, db, id, "cancelled", c.detail, c.code, done)
	}

	rec := getPlain(t, h.apiMissionStatsV2, "/api/missions/stats/v2?since=2026-03-10&until=2026-03-10")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Cancelled         int64 `json:"cancelled"`
		CancelledShingo   int64 `json:"cancelled_shingo"`
		CancelledRDS      int64 `json:"cancelled_rds"`
		UnclassifiedStops int64 `json:"unclassified_stops"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&got), "decode stats")
	// A coded row is classified by its code (R4 extended): the two coded Edge
	// changeover rows join the person, Core's two teardowns and the swap peer
	// as shingo. The fleet stop is RDS. The changeover written before the code
	// column has only its prose, which matches no pattern, and stays visible.
	if got.Cancelled != 8 || got.CancelledShingo != 6 || got.CancelledRDS != 1 || got.UnclassifiedStops != 1 {
		t.Errorf("cancelled %d = shingo %d + rds %d + unclassified %d, want 8 = 6 + 1 + 1",
			got.Cancelled, got.CancelledShingo, got.CancelledRDS, got.UnclassifiedStops)
	}
}

// TestOverviewPin_FleetPlantDays pins the seven-day Fleet Load rollup's day
// buckets under a plant zone that is not UTC (America/Chicago), over a window
// that spans the 2026-11-01 fall-back (a 25-hour plant day). Two missions
// finish between 00:00 and 05:00 UTC, which is the previous evening at the
// plant:
//
//	R1  2026-10-30 03:00Z–03:30Z (1800 s) = Oct 29 22:00 CDT
//	R2  2026-11-02 02:00Z–02:10Z (600 s)  = Nov 1 20:00 CST
//
// Not parallel: it swaps the package's plantLocation and the default clock
// (the window is after today's date, so "now" is pinned past it), both of
// which the parallel tests read.
func TestOverviewPin_FleetPlantDays(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	testutil.MustNoErr(t, err, "load zone")
	orig := plantLocation
	plantLocation = chicago
	t.Cleanup(func() { plantLocation = orig })

	h, db := testHandlers(t)
	prevClock := clock.Default()
	clock.SetDefault(clock.NewManual(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)))
	t.Cleanup(func() { clock.SetDefault(prevClock) })
	h.engine = fleetRobotsEngine{ServiceAccess: h.engine, robots: []fleet.RobotStatus{
		{VehicleID: "R1", Connected: true}, {VehicleID: "R2", Connected: true},
	}}
	utc := func(mo time.Month, d, hh, mm int) time.Time { return time.Date(2026, mo, d, hh, mm, 0, 0, time.UTC) }
	seedFinishedMission(t, db, "pin-dst-r1", "R1", utc(10, 30, 3, 0), utc(10, 30, 3, 30))
	seedFinishedMission(t, db, "pin-dst-r2", "R2", utc(11, 2, 2, 0), utc(11, 2, 2, 10))

	got := getFleet(t, h, "since=2026-10-29&until=2026-11-04")
	if got.LoadGranularity != "day" {
		t.Fatalf("granularity = %s, want day", got.LoadGranularity)
	}

	// Buckets are plant days: seven of them, each labelled with its Chicago
	// midnight, and each mission on the evening it ran. Nov 1 is 25 hours long,
	// so its average divides by 90,000 s.
	local := func(mo time.Month, d int) time.Time { return time.Date(2026, mo, d, 0, 0, 0, 0, chicago) }
	wantDays := []time.Time{
		local(10, 29), local(10, 30), local(10, 31), local(11, 1), local(11, 2), local(11, 3), local(11, 4),
	}
	wantAvg := map[time.Time]float64{local(10, 29): 1800.0 / 86400, local(11, 1): 600.0 / 90000}
	if len(got.LoadSeries) != len(wantDays) {
		t.Fatalf("day buckets = %d, want %d", len(got.LoadSeries), len(wantDays))
	}
	for i, p := range got.LoadSeries {
		if p.Day == nil || !p.Day.Equal(wantDays[i]) {
			t.Errorf("bucket %d day = %v, want %v", i, p.Day, wantDays[i])
			continue
		}
		wantPeak := int64(0)
		if wantAvg[wantDays[i]] > 0 {
			wantPeak = 1
		}
		if p.Peak != wantPeak || !almostEqual(p.Avg, wantAvg[wantDays[i]]) {
			t.Errorf("%s peak/avg = %d/%v, want %d/%v", wantDays[i].Format("Jan 2"), p.Peak, p.Avg, wantPeak, wantAvg[wantDays[i]])
		}
	}
	// The range totals do not depend on how the days are cut.
	const weekMS = 7*86_400_000 + 3_600_000 - 1 // the window includes the extra fall-back hour
	if f := got.Fleet; f.PeakConcurrency != 1 || !almostEqual(f.UtilPct, 2_400_000.0/(2*weekMS)*100) {
		t.Errorf("peak/util = %d/%v, want 1/%v", f.PeakConcurrency, f.UtilPct, 2_400_000.0/(2*weekMS)*100)
	}
}
