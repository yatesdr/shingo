//go:build docker

package www

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"testing"
	"time"

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
// Hour 01 is touched by all three robots, but never by two at once.
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

	// Per robot. busy_ms is finished-mission execution time only: R2's
	// 22 hours held on the open order count for nothing.
	wantBusy := map[string]int64{"R1": 150_000, "R2": 600_000, "R3": 60_000}
	wantMissions := map[string]int64{"R1": 2, "R2": 1, "R3": 1}
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

	// Hourly curve: distinct robots whose execution touched the hour. Hour 01
	// is all three (never two at once); hour 10 is R1; nothing else.
	if got.LoadGranularity != "hour" || len(got.LoadSeries) != 24 {
		t.Fatalf("load series = %s × %d, want hour × 24", got.LoadGranularity, len(got.LoadSeries))
	}
	wantConc := make([]int64, 24)
	wantConc[1], wantConc[10] = 3, 1
	for i, p := range got.LoadSeries {
		if p.Concurrency != wantConc[i] {
			t.Errorf("hour %02d concurrency = %d, want %d", i, p.Concurrency, wantConc[i])
		}
	}

	f := got.Fleet
	if f.Size != 3 || f.Missions != 4 {
		t.Errorf("size/missions = %d/%d, want 3/4", f.Size, f.Missions)
	}
	if f.PeakConcurrency != 3 || f.PeakHour != "01:00" || !f.CeilingReached {
		t.Errorf("peak = %d @ %s ceiling=%v, want 3 @ 01:00 ceiling=true", f.PeakConcurrency, f.PeakHour, f.CeilingReached)
	}
	// avg_load = mean hourly concurrency = 4/24; util = avg_load / size.
	if want := 4.0 / 24; !almostEqual(f.AvgLoad, want) {
		t.Errorf("avg_load = %v, want %v", f.AvgLoad, want)
	}
	if want := 4.0 / 24 / 3 * 100; !almostEqual(f.UtilPct, want) {
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
	// Peak is the busiest hour's distinct-robot count: hour 01 on the 10th.
	if f.PeakConcurrency != 3 || !f.CeilingReached {
		t.Errorf("peak = %d ceiling=%v, want 3 ceiling=true", f.PeakConcurrency, f.CeilingReached)
	}
	// util = mean of the seven daily averages / size; only the 10th has load
	// (4 robot-hours / 24 h).
	if want := 4.0 / 24 / 7 / 3 * 100; !almostEqual(f.UtilPct, want) {
		t.Errorf("util_pct = %v, want %v", f.UtilPct, want)
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
	now := time.Now()

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
			OrderID int64  `json:"order_id"`
			Status  string `json:"status"`
		} `json:"stuck_items"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&got), "decode alerts")

	// Stuck = created more than 2 × the 7-day P95 lead time (2 × 30 min) ago:
	// B (3 h) and C (9 h). A and D are younger than an hour.
	want := []int64{b, c}
	var ids []int64
	for _, it := range got.StuckItems {
		ids = append(ids, it.OrderID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if got.StuckMissions != len(want) || len(ids) != len(want) || (len(ids) == len(want) && !equalIDs(ids, want)) {
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
	// As it stands only the prose decides, and only the person and the fleet
	// stop match a pattern.
	if got.Cancelled != 8 || got.CancelledShingo != 1 || got.CancelledRDS != 1 || got.UnclassifiedStops != 6 {
		t.Errorf("cancelled %d = shingo %d + rds %d + unclassified %d, want 8 = 1 + 1 + 6",
			got.Cancelled, got.CancelledShingo, got.CancelledRDS, got.UnclassifiedStops)
	}
}
