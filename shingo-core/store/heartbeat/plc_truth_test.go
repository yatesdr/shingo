package heartbeat

import (
	"testing"
	"time"
)

// The PLC is the truth (close-out 2b). A jump or a reset is parts: the Edge
// counted them, it only did not see them one stroke at a time. So its delta
// counts, and the gap that ENDS in it is "counter offline" — not a stop, not
// downtime, and not a cycle or a target sample, because the parts inside it
// were made at times nobody saw.

// TestPLCTruth_JumpIsParts is the inverted TestHeartbeatMath_JumpsDoNotCount
// (deleted with this change; it said a jump counted toward nothing because an
// Edge operator's Confirm never reached Core).
// The jump sits inside a 300 s silence: the 150 s before it is counter
// offline, the 150 s after it (ending in an ordinary fire) is a stop.
func TestPLCTruth_JumpIsParts(t *testing.T) {
	t.Parallel()
	th := DefaultThresholds()
	target := 20 * time.Second
	events := []PartEvent{ev(0, 1, ""), ev(20, 1, ""), ev(170, 900, "jump"), ev(320, 1, ""), ev(340, 1, "")}
	m := ComputeMetrics(events, base, base.Add(360*time.Second), target, th)
	if m.Parts != 904 {
		t.Errorf("Parts = %d, want 904 (the jump's 900 are parts)", m.Parts)
	}
	if m.StopCount != 1 || m.TotalDowntimeMS != 150000 {
		t.Errorf("stops = %d downtime %d ms, want 1 of 150000 (only the gap after the jump)", m.StopCount, m.TotalDowntimeMS)
	}
	if m.CounterOfflineCount != 1 || m.CounterOfflineMS != 150000 {
		t.Errorf("counter offline = %d of %d ms, want 1 of 150000 (the gap before the jump)", m.CounterOfflineCount, m.CounterOfflineMS)
	}
	off := ComputeCounterOffline(events)
	if len(off) != 1 || off[0].Kind != KindCounterOffline || !off[0].End.Equal(base.Add(170*time.Second)) {
		t.Errorf("ComputeCounterOffline = %+v, want one gap ending at the jump", off)
	}
	if got := EstimateTarget(events); got != 20*time.Second {
		t.Errorf("EstimateTarget = %v, want 20s", got)
	}
	cs := ComputeCellState(events[:3], target, base.Add(171*time.Second), th)
	if cs.LastFire == nil || !cs.LastFire.Equal(base.Add(170*time.Second)) {
		t.Errorf("LastFire = %v, want the jump at +170s", cs.LastFire)
	}
	if cs.CurrentCycleMS != 0 {
		t.Errorf("CurrentCycleMS = %d, want 0 (the last gap ended in a jump: its timing is unknown)", cs.CurrentCycleMS)
	}
	if cs.PartsLastHour != 902 {
		t.Errorf("PartsLastHour = %d, want 902", cs.PartsLastHour)
	}
}

// TestPLCTruth_ResetGapIsCounterOffline is the inverted TestPin_ResetGapIsAStop:
// a reset row was an ordinary fire, so the silence before it was a stop.
func TestPLCTruth_ResetGapIsCounterOffline(t *testing.T) {
	t.Parallel()
	th := DefaultThresholds()
	target := 20 * time.Second
	events := []PartEvent{ev(0, 1, ""), ev(20, 1, ""), ev(400, 5, "reset")}
	m := ComputeMetrics(events, base, base.Add(420*time.Second), target, th)
	if m.Parts != 7 {
		t.Errorf("Parts = %d, want 7", m.Parts)
	}
	if m.StopCount != 0 || m.TotalDowntimeMS != 0 {
		t.Errorf("stops = %d downtime %d ms, want none (the reset's gap is counter offline)", m.StopCount, m.TotalDowntimeMS)
	}
	if m.CounterOfflineCount != 1 || m.CounterOfflineMS != 380000 {
		t.Errorf("counter offline = %d of %d ms, want 1 of 380000", m.CounterOfflineCount, m.CounterOfflineMS)
	}
	cs := ComputeCellState(events, target, base.Add(401*time.Second), th)
	if cs.CurrentCycleMS != 0 {
		t.Errorf("CurrentCycleMS = %d, want 0 (the last gap ended in a reset)", cs.CurrentCycleMS)
	}
}
