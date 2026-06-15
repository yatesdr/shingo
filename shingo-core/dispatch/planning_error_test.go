package dispatch

import "testing"

// TestPlanningError_Transient pins which planning failures are transient contention
// (the dispatcher must QUEUE + retry these, not terminally fail the order). lane_locked
// is the one multi-window loaders exposed: three windows pulling empties in parallel
// contend on a buried lane's reshuffle lock, and the simple-retrieve path used to fail
// instead of queue — dropping an order that just needed to wait for the lock to clear.
func TestPlanningError_Transient(t *testing.T) {
	transient := []string{"claim_failed", "lane_locked"}
	for _, code := range transient {
		if !(&planningError{Code: code}).Transient() {
			t.Errorf("code %q should be transient (queue + retry), got terminal", code)
		}
	}
	terminal := []string{"no_source_bin", "no_bin", "reshuffle_error", "invalid_node", ""}
	for _, code := range terminal {
		if (&planningError{Code: code}).Transient() {
			t.Errorf("code %q should be terminal (fail), got transient", code)
		}
	}
	if (*planningError)(nil).Transient() {
		t.Error("nil planningError must not be transient")
	}
}
