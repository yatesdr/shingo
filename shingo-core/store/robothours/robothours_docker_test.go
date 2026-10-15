//go:build docker

package robothours_test

import (
	"context"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/robothours"
)

func fptr(f float64) *float64 { return &f }
func iptr(i int) *int         { return &i }

// TestFlush_ReplaceAcrossTwoFlushes pins the replace upsert: a second flush of
// the same hour carries the running total and the row holds that total, not
// the sum of both flushes; a retried identical flush changes nothing.
func TestFlush_ReplaceAcrossTwoFlushes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	h := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)

	first := robothours.Hour{Hour: h, VehicleID: "AMR-01", ConnectedS: 300, ChargingS: 60, Metres: 12.5,
		ChargeStarts: 1, BatteryMin: fptr(80), LiftCount: iptr(1000)}
	if err := robothours.Flush(context.Background(), db.DB, []robothours.Hour{first}, nil); err != nil {
		t.Fatalf("flush 1: %v", err)
	}
	second := first
	second.ConnectedS, second.Metres, second.BatteryMin, second.LiftCount = 600, 30, fptr(72), iptr(1004)
	for i := 0; i < 2; i++ { // the second pass is a retry of the same totals
		if err := robothours.Flush(context.Background(), db.DB, []robothours.Hour{second}, nil); err != nil {
			t.Fatalf("flush 2 (pass %d): %v", i, err)
		}
	}
	hours, _, err := robothours.LoadHour(context.Background(), db.DB, h)
	if err != nil {
		t.Fatal(err)
	}
	got := hours["AMR-01"]
	if got.ConnectedS != 600 || got.ChargingS != 60 || got.Metres != 30 || got.ChargeStarts != 1 ||
		got.BatteryMin == nil || *got.BatteryMin != 72 || got.LiftCount == nil || *got.LiftCount != 1004 {
		t.Errorf("row = %+v, want the second flush's totals exactly", got)
	}

	// A robot never connected in the hour has no battery or lift reading:
	// NULL, not 0.
	if err := robothours.Flush(context.Background(), db.DB, []robothours.Hour{{Hour: h, VehicleID: "AMR-02"}}, nil); err != nil {
		t.Fatal(err)
	}
	hours, _, err = robothours.LoadHour(context.Background(), db.DB, h)
	testutil.MustNoErr(t, err, "robothours.LoadHour")
	if r := hours["AMR-02"]; r.BatteryMin != nil || r.LiftCount != nil {
		t.Errorf("AMR-02 = %+v, want NULL battery and lift count", r)
	}
}

// TestFlush_OnsetsAccumulateAcrossFlushes pins onsets across two flushes of
// one hour: the second carries the running total (2 of 54901, a new 52101) and
// the stored rows hold it, first_at from the first onset.
func TestFlush_OnsetsAccumulateAcrossFlushes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	h := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	t1 := h.Add(5 * time.Minute)
	hr := []robothours.Hour{{Hour: h, VehicleID: "AMR-11", ConnectedS: 300}}

	if err := robothours.Flush(context.Background(), db.DB, hr, []robothours.Onset{{Hour: h, VehicleID: "AMR-11", Code: 54901, Onsets: 1, FirstAt: t1}}); err != nil {
		t.Fatal(err)
	}
	if err := robothours.Flush(context.Background(), db.DB, hr, []robothours.Onset{
		{Hour: h, VehicleID: "AMR-11", Code: 54901, Onsets: 2, FirstAt: t1},
		{Hour: h, VehicleID: "AMR-11", Code: 52101, Onsets: 1, FirstAt: h.Add(20 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	_, onsets, err := robothours.LoadHour(context.Background(), db.DB, h)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]int{}
	for _, o := range onsets["AMR-11"] {
		got[o.Code] = o.Onsets
		if o.Code == 54901 && !o.FirstAt.Equal(t1) {
			t.Errorf("54901 first_at = %v, want %v", o.FirstAt, t1)
		}
	}
	if got[54901] != 2 || got[52101] != 1 || len(got) != 2 {
		t.Errorf("onsets = %v, want 54901:2 52101:1", got)
	}
}

// TestDayRows_PlantLocalAcrossDST pins read-time bucketing: a UTC-hour series
// covering Central's 2026-11-01 (25 hours, fall back) lands every hour of that
// plant day in one row, the neighbouring days stay separate, onsets ride their
// day, and lifts are the difference from the previous day's last count.
func TestDayRows_PlantLocalAcrossDST(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata: %v", err)
	}
	// 2026-11-01 00:00 CDT = 05:00Z; 2026-11-02 00:00 CST = 06:00Z. 25 hours.
	start := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	var hours []robothours.Hour
	lift := 500
	for h := start.Add(-2 * time.Hour); h.Before(end.Add(2 * time.Hour)); h = h.Add(time.Hour) {
		lift++
		hours = append(hours, robothours.Hour{Hour: h, VehicleID: "AMR-01", ConnectedS: 3600, Metres: 100,
			BatteryMin: fptr(50), LiftCount: iptr(lift)})
	}
	onsets := []robothours.Onset{
		{Hour: start, VehicleID: "AMR-01", Code: 54901, Onsets: 1, FirstAt: start},
		{Hour: end.Add(-time.Hour), VehicleID: "AMR-01", Code: 54901, Onsets: 2, FirstAt: end.Add(-time.Hour)},
		{Hour: end, VehicleID: "AMR-01", Code: 54901, Onsets: 7, FirstAt: end}, // Nov 2
	}
	if err := robothours.Flush(context.Background(), db.DB, hours, onsets); err != nil {
		t.Fatal(err)
	}

	rows, err := robothours.DayRows(db.DB, loc, "2026-11-01", "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	byDay := map[string]robothours.DayRow{}
	for _, r := range rows {
		byDay[r.Day] = r
	}
	if _, ok := byDay["2026-10-31"]; ok {
		t.Error("the look-back day leaked into the result")
	}
	d := byDay["2026-11-01"]
	if d.Hours != 25 || d.ConnectedS != 25*3600 || d.Metres != 2500 {
		t.Errorf("Nov 1 = %d hours, %d s, %.0f m; want 25 hours of 3600 s and 100 m", d.Hours, d.ConnectedS, d.Metres)
	}
	if d.Onsets[54901] != 3 {
		t.Errorf("Nov 1 54901 = %d, want 3", d.Onsets[54901])
	}
	if d.Lifts == nil || *d.Lifts != 25 {
		t.Errorf("Nov 1 lifts = %v, want 25 (one per hour since Oct 31's last)", d.Lifts)
	}
	if n := byDay["2026-11-02"]; n.Hours != 2 || n.Onsets[54901] != 7 {
		t.Errorf("Nov 2 = %d hours, 54901 %d; want 2 and 7", n.Hours, n.Onsets[54901])
	}
}

// TestLidarHours_InsertReplacePrune pins the lidar rows: one per lidar, a
// second probe in the hour replaces, nil quartiles stay NULL, and rows past 90
// days are pruned.
func TestLidarHours_InsertReplacePrune(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	old := now.Add(-91 * 24 * time.Hour)

	rows := []robothours.LidarHour{
		{Hour: now, VehicleID: "AMR-01", Lidar: "laser", Station: "PP57", UseForLoc: true, Beams: 360, Valid: 358,
			RSSIMedValid: fptr(30.06), RSSIP25Valid: fptr(29.06), RSSIP75Valid: fptr(31.06), NearValid: 0},
		{Hour: now, VehicleID: "AMR-01", Lidar: "laser2", Station: "PP57", Beams: 359, Valid: 0},
		{Hour: old, VehicleID: "AMR-01", Lidar: "laser", Station: "PP57", Beams: 360, Valid: 300},
	}
	if err := robothours.InsertLidarHours(db.DB, rows); err != nil {
		t.Fatal(err)
	}
	rows[0].Valid = 357
	if err := robothours.InsertLidarHours(db.DB, rows[:1]); err != nil {
		t.Fatal(err)
	}
	var (
		valid  int
		nilMed bool
	)
	db.DB.QueryRow(`SELECT valid FROM robot_lidar_hours WHERE hour = $1 AND lidar = 'laser'`, now).Scan(&valid)
	db.DB.QueryRow(`SELECT rssi_med_valid IS NULL FROM robot_lidar_hours WHERE hour = $1 AND lidar = 'laser2'`, now).Scan(&nilMed)
	if valid != 357 || !nilMed {
		t.Errorf("valid=%d nilMed=%v, want 357 and NULL median for a lidar with no valid beam", valid, nilMed)
	}
	n, err := robothours.PruneLidarHours(db.DB, now)
	if err != nil || n != 1 {
		t.Errorf("prune = %d, %v; want the one 91-day-old row", n, err)
	}
}
