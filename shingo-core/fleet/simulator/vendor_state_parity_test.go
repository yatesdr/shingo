package simulator

import (
	"testing"

	"shingocore/fleet/seerrds"
	"shingocore/rds"
)

// allRDSStates is every vendor order state rds/ defines.
var allRDSStates = []rds.OrderState{
	rds.StateCreated, rds.StateToBeDispatched, rds.StateRunning, rds.StateFinished,
	rds.StateFailed, rds.StateStopped, rds.StateWaiting,
}

// TestSimulatorVendorStateMatchesSEER pins the simulator's vendor vocabulary to
// the real adapter's (CW#25). The simulator stands in for SEER RDS in every sim
// run and in the dispatch tests, so a mapping or terminal set that drifts from
// seerrds is a sim that certifies behaviour the plant does not have.
//
// The one deliberate difference is the UNKNOWN-state default: seerrds logs and
// answers "dispatched" (a real fleet may grow a state); the simulator answers
// "unknown" (it only ever emits states it defines, so anything else is a sim
// bug worth seeing). Known states must agree exactly.
func TestSimulatorVendorStateMatchesSEER(t *testing.T) {
	t.Parallel()
	sim := New()
	for _, st := range allRDSStates {
		if got, want := sim.MapState(string(st)), seerrds.MapState(string(st)); got != want {
			t.Errorf("MapState(%s): simulator %q, seerrds %q", st, got, want)
		}
		if got, want := sim.IsTerminalState(string(st)), seerrds.IsTerminalState(string(st)); got != want {
			t.Errorf("IsTerminalState(%s): simulator %v, seerrds %v", st, got, want)
		}
	}
	if got := sim.MapState("NOT-A-STATE"); got != "unknown" {
		t.Errorf("simulator MapState(unknown) = %q, want \"unknown\" (its documented default)", got)
	}
}

// TestEvictableTerminalIsTerminalPlusFailed pins the eviction set as the
// dispatch terminal set PLUS FAILED, on purpose: FAILED maps to Core's faulted,
// a non-terminal grace state, so dispatch must not call it terminal — but a
// failed sim order is as dead as a finished one and its memory can go.
func TestEvictableTerminalIsTerminalPlusFailed(t *testing.T) {
	t.Parallel()
	for _, st := range allRDSStates {
		want := st.IsTerminal() || st == rds.StateFailed
		if got := isEvictableTerminal(string(st)); got != want {
			t.Errorf("isEvictableTerminal(%s) = %v, want %v (rds IsTerminal ∪ FAILED)", st, got, want)
		}
	}
}
