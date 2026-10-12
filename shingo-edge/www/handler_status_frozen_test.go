package www

import "testing"

// A frozen loop shows on /status: the Edge answers, so every other field can
// read healthy while its PLC poll has stopped (the freeze alarm, plant build).
func TestStatus_NamesAFrozenLoop(t *testing.T) {
	h, r := newTestHandlers(t)
	if body := getStatus(t, h, r); body["frozen"] != nil {
		t.Fatalf("frozen = %v with no stall; want the field absent", body["frozen"])
	}
	h.orchestration.(*stubEngine).statusFrozen = []string{"the PLC poll has not turned over for 31s"}
	body := getStatus(t, h, r)
	got, _ := body["frozen"].([]any)
	if len(got) != 1 || got[0] != "the PLC poll has not turned over for 31s" {
		t.Fatalf("frozen = %v; want the stalled loop named", body["frozen"])
	}
}
