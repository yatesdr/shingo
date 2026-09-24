package heartbeat

import (
	"testing"
	"time"
)

// TestPin_ResetGapIsAStop: the Edge ships no reset today, but the math has no
// rule for one — a reset row is an ordinary fire, so the silence before it is
// a stop and its delta is parts. (The jump half is
// TestHeartbeatMath_JumpsDoNotCount.)
func TestPin_ResetGapIsAStop(t *testing.T) {
	t.Parallel()
	th := DefaultThresholds()
	target := 20 * time.Second
	events := []PartEvent{ev(0, 1, ""), ev(20, 1, ""), ev(400, 5, "reset")}
	m := ComputeMetrics(events, base, base.Add(420*time.Second), target, th)
	if m.Parts != 7 {
		t.Errorf("Parts = %d, want 7", m.Parts)
	}
	if m.StopCount != 1 || m.TotalDowntimeMS != 380000 {
		t.Errorf("stops = %d downtime %d ms, want 1 of 380000 (the reset's gap is a stop)", m.StopCount, m.TotalDowntimeMS)
	}
	cs := ComputeCellState(events, target, base.Add(401*time.Second), th)
	if cs.CurrentCycleMS != 76000 {
		t.Errorf("CurrentCycleMS = %d, want 76000 (380 s over the reset's 5)", cs.CurrentCycleMS)
	}
}
