//go:build docker

package telemetry_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/telemetry"
)

// TestCoverage_GetStatsV2 pins the corrected dashboard stats (plan §3.A /
// §8 #5): success_rate = Confirmed/(Confirmed+Failed) with cancelled and
// skipped excluded from the denominator. A STOPPED mission with no
// order_history defaults to cancelled (classifyStops LEFT JOIN path), so it
// must not drag the success rate down. The grace/timeout→failed
// reclassification is covered by domain.TestClassifyTermination (no DB).
func TestCoverage_GetStatsV2(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	rows := []*telemetry.Mission{
		{OrderID: 3001, StationID: "V2", TerminalState: "FINISHED", DurationMS: 1000, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3002, StationID: "V2", TerminalState: "FINISHED", DurationMS: 2000, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3003, StationID: "V2", TerminalState: "delivered", DurationMS: 3000, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3004, StationID: "V2", TerminalState: "FAILED", DurationMS: 1500, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3005, StationID: "V2", TerminalState: "STOPPED", DurationMS: 500, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
		{OrderID: 3006, StationID: "V2", TerminalState: "SKIPPED", DurationMS: 0, BlocksJSON: "[]", ErrorsJSON: "[]", WarningsJSON: "[]", NoticesJSON: "[]"},
	}
	for _, r := range rows {
		if err := telemetry.UpsertMission(db.DB, r); err != nil {
			t.Fatalf("UpsertMission %d: %v", r.OrderID, err)
		}
	}

	s, err := telemetry.GetStatsV2(db.DB, telemetry.Filter{StationID: "V2"})
	if err != nil {
		t.Fatalf("GetStatsV2: %v", err)
	}
	if s.Total != 6 {
		t.Errorf("Total = %d, want 6", s.Total)
	}
	if s.Confirmed != 3 {
		t.Errorf("Confirmed = %d, want 3", s.Confirmed)
	}
	if s.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (no system stops without order_history)", s.Failed)
	}
	if s.Cancelled != 1 {
		t.Errorf("Cancelled = %d, want 1 (STOPPED w/o history defaults to cancelled)", s.Cancelled)
	}
	if s.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", s.Skipped)
	}
	// 3 confirmed / (3 confirmed + 1 failed) = 75%. Cancelled + skipped excluded.
	if s.SuccessRate != 75.0 {
		t.Errorf("SuccessRate = %v, want 75 (cancelled+skipped excluded from denominator)", s.SuccessRate)
	}
	// Durations over rows with duration_ms > 0: 1000,2000,3000,1500,500 → avg 1600.
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
