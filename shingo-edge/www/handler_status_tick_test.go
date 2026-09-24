package www

import "testing"

// TestStatus_ReportsProductionTickLag: /status shows the shipper's backlog —
// shippable rows past the cursor and the age of the oldest — because the
// shipper is a goroutine that can stall with nothing else saying so.
func TestStatus_ReportsProductionTickLag(t *testing.T) {
	h, r := newTestHandlers(t)
	eng := h.orchestration.(*stubEngine)
	eng.statusTickPending, eng.statusTickOldestMS = 7, 12500

	body := getStatus(t, h, r)
	if got, ok := body["production_ticks_pending"].(float64); !ok || got != 7 {
		t.Errorf("production_ticks_pending = %v, want 7", body["production_ticks_pending"])
	}
	if got, ok := body["production_ticks_oldest_unsent_age_ms"].(float64); !ok || got != 12500 {
		t.Errorf("production_ticks_oldest_unsent_age_ms = %v, want 12500", body["production_ticks_oldest_unsent_age_ms"])
	}
}
