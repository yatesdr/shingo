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

// TestPlantDayStart pins the Fleet Load day-truncation fix: a UTC-normalized
// end-of-plant-day time (as parseMissionFilter produces) must truncate to the
// plant's calendar day, not the next UTC day. Truncating in the raw UTC
// location was charting tomorrow's all-zero day (0% fleet utilization).
func TestPlantDayStart(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	orig := plantLocation
	plantLocation = chicago
	defer func() { plantLocation = orig }()

	// End of plant day 2026-06-10 (CDT=UTC-5) == 2026-06-11 04:59:59.999 UTC.
	until := time.Date(2026, 6, 11, 4, 59, 59, 999000000, time.UTC)
	got := plantDayStart(until)
	want := time.Date(2026, 6, 10, 0, 0, 0, 0, chicago)
	if !got.Equal(want) {
		t.Errorf("plantDayStart(%v) = %v, want %v (plant calendar day, not next UTC day)", until.UTC(), got, want)
	}
	// A time already at plant midnight is a no-op.
	if got2 := plantDayStart(want); !got2.Equal(want) {
		t.Errorf("plantDayStart(midnight) = %v, want %v", got2, want)
	}
}
