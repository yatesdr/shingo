//go:build docker

package telemetry_test

import (
	"testing"

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
