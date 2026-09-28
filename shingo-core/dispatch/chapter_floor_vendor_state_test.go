package dispatch

import (
	"testing"

	"shingocore/rds"
)

// TestChapterFloorVendorTerminalIsTerminalPlusFailed pins isTerminalVendorState
// to the rds terminal set PLUS FAILED. The chapter floor asks whether the FLEET
// still has work outstanding on a mission, and a FAILED mission has none — even
// though MapState turns FAILED into Core's non-terminal faulted. The wider set
// is deliberate; this test is what keeps it the rds set plus exactly that one.
func TestChapterFloorVendorTerminalIsTerminalPlusFailed(t *testing.T) {
	t.Parallel()
	for _, st := range []rds.OrderState{
		rds.StateCreated, rds.StateToBeDispatched, rds.StateRunning, rds.StateFinished,
		rds.StateFailed, rds.StateStopped, rds.StateWaiting,
	} {
		want := st.IsTerminal() || st == rds.StateFailed
		if got := isTerminalVendorState(string(st)); got != want {
			t.Errorf("isTerminalVendorState(%s) = %v, want %v", st, got, want)
		}
	}
}
