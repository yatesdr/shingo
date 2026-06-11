package footprint

import (
	"testing"
	"time"
)

// TestPlantDayKeys_KeysInPlantZone pins half of the Q-036 day-key fix: the Go
// day axis must be built in the plant timezone, not UTC. Late evening at the
// plant is already the next UTC day, so a UTC-keyed axis labelled today's
// counts under tomorrow and the join dropped them.
func TestPlantDayKeys_KeysInPlantZone(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 2026-06-11 03:00 UTC == 2026-06-10 22:00 CDT — still June 10 at the plant.
	now := time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC)
	keys := plantDayKeys(now, chicago, 3)
	if len(keys) != 3 {
		t.Fatalf("len(keys) = %d, want 3", len(keys))
	}
	// Oldest first; the last entry is "today" in the plant zone.
	if got := keys[2].Format("2006-01-02"); got != "2026-06-10" {
		t.Errorf("latest day key = %s, want 2026-06-10 (plant-local, not UTC 06-11)", got)
	}
	if got := keys[0].Format("2006-01-02"); got != "2026-06-08" {
		t.Errorf("oldest day key = %s, want 2026-06-08", got)
	}
}
