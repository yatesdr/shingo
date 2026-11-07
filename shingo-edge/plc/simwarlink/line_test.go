package simwarlink

import "testing"

// TestLinePause_TheLineDrainsAndTheCounterTicksUntilEmpty: paused, nothing
// loads and the loads in flight count out one a tick; once the line is empty
// the counter stands. Resumed, loading goes on and the counter waits for the
// line to refill.
func TestLinePause_TheLineDrainsAndTheCounterTicksUntilEmpty(t *testing.T) {
	l := NewLine(3, 1)
	l.SetIdentity(1001)
	for range 5 {
		l.Tick()
	}
	l.Pause()
	for c := int64(3); c <= 5; c++ {
		l.Tick()
		if st := l.State(); st.Loads != 5 || st.Counter != c || !st.Paused {
			t.Fatalf("paused, draining: %+v; want 5 loads (none since the pause), counter %d, paused", st, c)
		}
	}
	for range 2 {
		l.Tick()
	}
	if st := l.State(); st.Loads != 5 || st.Counter != 5 || st.InFlight != 0 {
		t.Fatalf("paused and empty: %+v; want the counter standing at 5, nothing in flight", st)
	}
	l.Resume()
	for k := int64(6); k <= 8; k++ {
		l.Tick()
		if st := l.State(); st.Loads != k || st.Counter != 5 || st.Paused {
			t.Fatalf("resumed, refilling: %+v; want %d loads, counter 5, not paused", st, k)
		}
	}
	l.Tick()
	if st := l.State(); st.Loads != 9 || st.Counter != 6 || st.InFlight != 3 || st.ByIdentity[1001] != 9 {
		t.Fatalf("refilled: %+v; want 9 loads under 1001, counter 6, 3 in flight", st)
	}
}

// TestLinePause_ADrainChangeLandsAndLoadingStaysPaused: a drain change made
// while loading is paused lands once the line is empty, and the line stays
// paused until resumed; the loads after it are booked to the new identity.
func TestLinePause_ADrainChangeLandsAndLoadingStaysPaused(t *testing.T) {
	l := NewLine(2, 1)
	l.SetIdentity(1001)
	for range 3 {
		l.Tick()
	}
	l.Pause()
	l.Change(1002)
	for range 4 {
		l.Tick()
	}
	if st := l.State(); st.Identity != 1002 || st.Counter != 0 || st.Loads != 3 || !st.Paused {
		t.Fatalf("after the drain and the change: %+v; want identity 1002, counter 0, 3 loads, still paused", st)
	}
	l.Resume()
	l.Tick()
	if st := l.State(); st.Loads != 4 || st.ByIdentity[1002] != 1 {
		t.Fatalf("resumed: %+v; want the 4th load booked to 1002", st)
	}
}
