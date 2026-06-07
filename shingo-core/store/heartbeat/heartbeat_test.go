package heartbeat

import (
	"testing"
	"time"
)

var base = time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

// evAt builds part events at the given second-offsets from base.
func evAt(offsetsSec ...int) []PartEvent {
	out := make([]PartEvent, len(offsetsSec))
	for i, s := range offsetsSec {
		out[i] = PartEvent{CellID: "STN-A", RecordedAt: base.Add(time.Duration(s) * time.Second), Delta: 1}
	}
	return out
}

func TestComputeCellState(t *testing.T) {
	target := 20 * time.Second
	th := DefaultThresholds()

	if got := ComputeCellState(nil, target, base, th); got.State != StateNoData {
		t.Errorf("empty → %q, want no-data", got.State)
	}

	ev := evAt(0, 20, 40)
	// 10s since last (0.5× target) → running; current cycle = 20s.
	if got := ComputeCellState(ev, target, base.Add(50*time.Second), th); got.State != StateRunning {
		t.Errorf("10s since last → %q, want running", got.State)
	} else if got.CurrentCycleMS != 20000 {
		t.Errorf("current cycle = %dms, want 20000", got.CurrentCycleMS)
	}
	// 50s since last (2.5×) → slowed.
	if got := ComputeCellState(ev, target, base.Add(90*time.Second), th); got.State != StateSlowed {
		t.Errorf("2.5× → %q, want slowed", got.State)
	}
	// 5min since last (15×) → micro-stop, with an active stop.
	if got := ComputeCellState(ev, target, base.Add(340*time.Second), th); got.State != StateMicroStop {
		t.Errorf("15× → %q, want micro-stop", got.State)
	} else if got.StopActive == nil {
		t.Error("micro-stop should carry an active stop")
	}
	// 20min since last (60×) → stopped.
	if got := ComputeCellState(ev, target, base.Add(40*time.Second+20*time.Minute), th); got.State != StateStopped {
		t.Errorf("60× → %q, want stopped", got.State)
	}
}

func TestComputeStops(t *testing.T) {
	target := 20 * time.Second
	th := DefaultThresholds()
	// Cycles at 0,20,40, then a 300s gap (micro-stop: >60s, ≤600s), then a
	// 700s gap (stopped: >600s).
	ev := evAt(0, 20, 40, 340, 1040)
	stops := ComputeStops(ev, target, th)
	if len(stops) != 2 {
		t.Fatalf("stops = %d, want 2", len(stops))
	}
	if stops[0].Kind != StateMicroStop {
		t.Errorf("first stop kind = %q, want micro-stop", stops[0].Kind)
	}
	if stops[0].DurationMS != 300000 {
		t.Errorf("first stop duration = %dms, want 300000", stops[0].DurationMS)
	}
	if stops[1].Kind != StateStopped {
		t.Errorf("second stop kind = %q, want stopped", stops[1].Kind)
	}
	// Normal cadence → no stops.
	if got := ComputeStops(evAt(0, 20, 40, 60), target, th); len(got) != 0 {
		t.Errorf("steady cadence → %d stops, want 0", len(got))
	}
}

func TestComputeMetrics(t *testing.T) {
	target := 20 * time.Second
	th := DefaultThresholds()
	// 4 parts over a 600s window with one 300s micro-stop gap.
	ev := evAt(0, 20, 40, 340)
	m := ComputeMetrics(ev, base, base.Add(600*time.Second), target, th)
	if m.Parts != 4 {
		t.Errorf("parts = %d, want 4", m.Parts)
	}
	if m.StopCount != 1 {
		t.Errorf("stop count = %d, want 1", m.StopCount)
	}
	if m.TotalDowntimeMS != 300000 {
		t.Errorf("downtime = %dms, want 300000", m.TotalDowntimeMS)
	}
	// run = 600s - 300s = 300s = 5min; MTBF = 5/1 = 5min, MTTR = 5/1 = 5min.
	if m.MTBFMinutes != 5 {
		t.Errorf("MTBF = %v, want 5", m.MTBFMinutes)
	}
	if m.MTTRMinutes != 5 {
		t.Errorf("MTTR = %v, want 5", m.MTTRMinutes)
	}
	// expected at target = 600/20 = 30; lost = 30 - 4 = 26.
	if m.PartsLost != 26 {
		t.Errorf("parts lost = %d, want 26", m.PartsLost)
	}
	if m.LongestStopMS != 300000 {
		t.Errorf("longest stop = %dms, want 300000", m.LongestStopMS)
	}
}
