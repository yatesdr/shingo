package heartbeat

import (
	"testing"
	"time"
)

// ev builds one event at a second-offset from base with the given delta and
// anomaly.
func ev(sec int, delta int64, anomaly string) PartEvent {
	return PartEvent{CellID: "STN-A", RecordedAt: base.Add(time.Duration(sec) * time.Second), Delta: delta, Anomaly: anomaly}
}

// Parts are counted by delta, not by row. A poll pass that saw three strokes
// is one row carrying delta = 3, and it is three parts. Counting rows made the
// dashboards' part counts depend on the poll rate.
func TestComputeMetrics_PartsAreSumOfDelta(t *testing.T) {
	t.Parallel()
	events := []PartEvent{ev(0, 1, ""), ev(20, 3, ""), ev(40, 2, "")}
	m := ComputeMetrics(events, base, base.Add(60*time.Second), 20*time.Second, DefaultThresholds())
	if m.Parts != 6 {
		t.Errorf("Parts = %d, want 6 (1+3+2)", m.Parts)
	}
}

func TestComputeCellState_PartsLastHourIsSumOfDelta(t *testing.T) {
	t.Parallel()
	events := []PartEvent{ev(0, 1, ""), ev(20, 3, ""), ev(40, 2, "")}
	cs := ComputeCellState(events, 20*time.Second, base.Add(50*time.Second), DefaultThresholds())
	if cs.PartsLastHour != 6 {
		t.Errorf("PartsLastHour = %d, want 6", cs.PartsLastHour)
	}
}

// A tick carrying delta parts over a gap made each part in gap/delta.
func TestComputeCellState_CurrentCycleDividesByDelta(t *testing.T) {
	t.Parallel()
	events := []PartEvent{ev(0, 1, ""), ev(21, 3, "")}
	cs := ComputeCellState(events, 7*time.Second, base.Add(22*time.Second), DefaultThresholds())
	if cs.CurrentCycleMS != 7000 {
		t.Errorf("CurrentCycleMS = %d, want 7000 (21 s over 3 parts)", cs.CurrentCycleMS)
	}
}

func TestEstimateTarget_DividesByDelta(t *testing.T) {
	t.Parallel()
	// Gaps 3 s, 3 s, 3 s — but each tick after the first carries 3 parts, so
	// the per-part cycle is 1 s.
	events := []PartEvent{ev(0, 3, ""), ev(3, 3, ""), ev(6, 3, ""), ev(9, 3, "")}
	if got := EstimateTarget(events); got != time.Second {
		t.Errorf("EstimateTarget = %v, want 1s", got)
	}
}
