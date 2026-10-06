//go:build docker

package telemetry_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/telemetry"
)

// seedTerminalOrder inserts a terminal order row directly — the v2 stats COUNTS
// source (order_outcome.go). updated_at defaults to NOW(), which lands inside an
// unbounded filter window.
func seedTerminalOrder(t *testing.T, db *store.DB, uuid, station, status string) {
	t.Helper()
	if _, err := db.DB.Exec(
		`INSERT INTO orders (edge_uuid, station_id, status) VALUES ($1, $2, $3)`,
		uuid, station, status); err != nil {
		t.Fatalf("seed order %s (%s): %v", uuid, status, err)
	}
}

// TestCoverage_GetStatsV2 pins the corrected dashboard stats (plan §3.A / §8 #5).
// COUNTS come from the orders table — the complete terminal record, including
// failures that never became a robot mission — so success_rate =
// Confirmed/(Confirmed+Failed) with cancelled and skipped excluded. DURATIONS
// still come from mission_telemetry (only robot missions have an execution
// interval), so the two sources are seeded independently here.
func TestCoverage_GetStatsV2(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// Counts: 3 confirmed, 1 failed, 1 cancelled, 1 skipped at station V2.
	seedTerminalOrder(t, db, "v2-1", "V2", "confirmed")
	seedTerminalOrder(t, db, "v2-2", "V2", "confirmed")
	seedTerminalOrder(t, db, "v2-3", "V2", "confirmed")
	seedTerminalOrder(t, db, "v2-4", "V2", "failed")
	seedTerminalOrder(t, db, "v2-5", "V2", "cancelled")
	seedTerminalOrder(t, db, "v2-6", "V2", "skipped")
	// A confirmed order at another station must NOT leak into the V2 counts.
	seedTerminalOrder(t, db, "other-1", "OTHER", "confirmed")
	// 'delivered' is non-terminal (awaiting confirm) and must be excluded.
	seedTerminalOrder(t, db, "v2-deliv", "V2", "delivered")

	// Durations come from mission_telemetry. duration_ms>0: 1000,2000,3000,1500,500 → avg 1600.
	mtRows := []*telemetry.Mission{
		{OrderID: 3001, StationID: "V2", TerminalState: "FINISHED", DurationMS: 1000, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3002, StationID: "V2", TerminalState: "FINISHED", DurationMS: 2000, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3003, StationID: "V2", TerminalState: "delivered", DurationMS: 3000, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3004, StationID: "V2", TerminalState: "FAILED", DurationMS: 1500, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3005, StationID: "V2", TerminalState: "STOPPED", DurationMS: 500, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3006, StationID: "V2", TerminalState: "SKIPPED", DurationMS: 0, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
	}
	for _, r := range mtRows {
		if err := telemetry.UpsertMission(db.DB, r); err != nil {
			t.Fatalf("UpsertMission %d: %v", r.OrderID, err)
		}
	}

	s, err := telemetry.GetStatsV2(db.DB, telemetry.Filter{StationID: "V2"})
	if err != nil {
		t.Fatalf("GetStatsV2: %v", err)
	}
	if s.Total != 6 {
		t.Errorf("Total = %d, want 6 (terminal orders at V2; delivered + other-station excluded)", s.Total)
	}
	if s.Confirmed != 3 {
		t.Errorf("Confirmed = %d, want 3", s.Confirmed)
	}
	if s.Failed != 1 {
		t.Errorf("Failed = %d, want 1", s.Failed)
	}
	if s.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1", s.Cancelled)
	}
	if s.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", s.Skipped)
	}
	// 3 confirmed / (3 confirmed + 1 failed) = 75%. Cancelled + skipped excluded.
	if s.SuccessRate != 75.0 {
		t.Errorf("SuccessRate = %v, want 75 (cancelled+skipped excluded from denominator)", s.SuccessRate)
	}
	// Durations from mission_telemetry rows with duration_ms > 0 → avg 1600.
	if s.AvgDurationMS != 1600 {
		t.Errorf("AvgDurationMS = %d, want 1600", s.AvgDurationMS)
	}
}

func TestCoverage_GetStatsV2_EmptyPopulation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	s, err := telemetry.GetStatsV2(db.DB, telemetry.Filter{StationID: "NOBODY-V2"})
	if err != nil {
		t.Fatalf("GetStatsV2 (empty): %v", err)
	}
	if s.Total != 0 {
		t.Errorf("empty Total = %d, want 0", s.Total)
	}
	if s.SuccessRate != 0 {
		t.Errorf("empty SuccessRate = %v, want 0 (no divide-by-zero)", s.SuccessRate)
	}
}

// seedCompletedOrder inserts a terminal order completed at an exact instant —
// the bucketing timestamp GetTimeseries reads (COALESCE(completed_at, updated_at)).
func seedCompletedOrder(t *testing.T, db *store.DB, uuid, station, status string, at time.Time) {
	t.Helper()
	if _, err := db.DB.Exec(
		`INSERT INTO orders (edge_uuid, station_id, status, completed_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		uuid, station, status, at.UTC()); err != nil {
		t.Fatalf("seed order %s: %v", uuid, err)
	}
}

type tsBucket struct {
	start string // RFC3339 UTC
	total int64
}

func timeseriesShape(t *testing.T, got []telemetry.Bucket) []tsBucket {
	t.Helper()
	out := make([]tsBucket, len(got))
	for i, b := range got {
		out[i] = tsBucket{b.BucketStart.UTC().Format(time.RFC3339), b.Total}
	}
	return out
}

func checkTimeseries(t *testing.T, got []telemetry.Bucket, want []tsBucket) {
	t.Helper()
	shape := timeseriesShape(t, got)
	if len(shape) != len(want) {
		t.Fatalf("buckets = %v, want %v", shape, want)
	}
	for i := range want {
		if shape[i] != want[i] {
			t.Fatalf("bucket %d = %v, want %v (all: %v)", i, shape[i], want[i], shape)
		}
	}
}

// plantDayWindow is the filter parseMissionFilter builds for bare
// since/until dates: plant-local midnight to the last nanosecond of until.
func plantDayWindow(t *testing.T, station string, loc *time.Location, since, until string) telemetry.Filter {
	t.Helper()
	s, err := time.ParseInLocation("2006-01-02", since, loc)
	if err != nil {
		t.Fatal(err)
	}
	u, err := time.ParseInLocation("2006-01-02", until, loc)
	if err != nil {
		t.Fatal(err)
	}
	su, uu := s.UTC(), u.Add(24*time.Hour-time.Nanosecond).UTC()
	return telemetry.Filter{StationID: station, Since: &su, Until: &uu}
}

// TestTimeseries_DayBucketsArePlantDays pins the day series under a non-UTC
// plant zone, with completions between 00:00 and 05:00 UTC — the hours that are
// still the PREVIOUS day in America/Chicago. The range total is the same five
// orders whatever the bucketing; only the per-day split may move. The window is
// in the past so "now" does not clip it.
func TestTimeseries_DayBucketsArePlantDays(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	chicago := mustLoc(t, "America/Chicago")
	seedCompletedOrder(t, db, "pd-1", "PD", "confirmed", mustTime(t, "2026-09-04T23:30:00Z")) // Sep 4 18:30 CDT
	seedCompletedOrder(t, db, "pd-2", "PD", "confirmed", mustTime(t, "2026-09-05T02:00:00Z")) // Sep 4 21:00 CDT
	seedCompletedOrder(t, db, "pd-3", "PD", "cancelled", mustTime(t, "2026-09-05T04:59:00Z")) // Sep 4 23:59 CDT
	seedCompletedOrder(t, db, "pd-4", "PD", "confirmed", mustTime(t, "2026-09-05T06:00:00Z")) // Sep 5 01:00 CDT
	seedCompletedOrder(t, db, "pd-5", "PD", "failed", mustTime(t, "2026-09-06T15:00:00Z"))    // Sep 6 10:00 CDT

	// Five plant days, Sep 3 (empty) through Sep 7 (empty): exactly five buckets,
	// each starting on a Chicago midnight (05:00Z under CDT).
	f := plantDayWindow(t, "PD", chicago, "2026-09-03", "2026-09-07")
	got, err := telemetry.GetTimeseries(db.DB, f, "day", chicago)
	if err != nil {
		t.Fatalf("GetTimeseries: %v", err)
	}
	checkTimeseries(t, got, []tsBucket{
		{"2026-09-03T05:00:00Z", 0},
		{"2026-09-04T05:00:00Z", 3},
		{"2026-09-05T05:00:00Z", 1},
		{"2026-09-06T05:00:00Z", 1},
		{"2026-09-07T05:00:00Z", 0},
	})
}

// TestTimeseries_DayBucketsAcrossFallBack runs the SQL half of the plant-day cut
// across a 25-hour day (2025-11-02, America/Chicago). 05:30Z on Nov 3 is still
// Nov 2 (23:30 CST), and Nov 3 starts at 06:00Z, not 05:00Z.
func TestTimeseries_DayBucketsAcrossFallBack(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	chicago := mustLoc(t, "America/Chicago")
	seedCompletedOrder(t, db, "fb-1", "FB", "confirmed", mustTime(t, "2025-11-03T05:30:00Z")) // Nov 2 23:30 CST
	seedCompletedOrder(t, db, "fb-2", "FB", "confirmed", mustTime(t, "2025-11-03T06:30:00Z")) // Nov 3 00:30 CST

	f := plantDayWindow(t, "FB", chicago, "2025-11-01", "2025-11-03")
	got, err := telemetry.GetTimeseries(db.DB, f, "day", chicago)
	if err != nil {
		t.Fatalf("GetTimeseries: %v", err)
	}
	checkTimeseries(t, got, []tsBucket{
		{"2025-11-01T05:00:00Z", 0},
		{"2025-11-02T05:00:00Z", 1},
		{"2025-11-03T06:00:00Z", 1},
	})
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
