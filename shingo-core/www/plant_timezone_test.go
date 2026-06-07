package www

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseMissionFilterPlantTimezone pins the plant-local-at-server date
// resolution (Q-004): a bare YYYY-MM-DD filter resolves to midnight in the
// plant timezone, normalized to UTC for the timestamptz comparison.
func TestParseMissionFilterPlantTimezone(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	orig := plantLocation
	plantLocation = chicago
	defer func() { plantLocation = orig }()

	req := httptest.NewRequest("GET", "/api/missions?since=2026-06-06&until=2026-06-06", nil)
	f := parseMissionFilter(req)
	if f.Since == nil || f.Until == nil {
		t.Fatal("since/until not set")
	}

	// 2026-06-06 00:00 America/Chicago (CDT = UTC-5) == 2026-06-06 05:00 UTC.
	wantSince := time.Date(2026, 6, 6, 5, 0, 0, 0, time.UTC)
	if !f.Since.Equal(wantSince) {
		t.Errorf("Since = %v, want %v", f.Since.UTC(), wantSince)
	}
	// until = end of the plant day = 2026-06-07 05:00 UTC minus 1ns.
	nextDayUTC := time.Date(2026, 6, 7, 5, 0, 0, 0, time.UTC)
	if !f.Until.Before(nextDayUTC) || f.Until.Before(nextDayUTC.Add(-2*time.Second)) {
		t.Errorf("Until = %v, want just before %v", f.Until.UTC(), nextDayUTC)
	}
}
