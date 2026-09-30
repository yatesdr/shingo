package service

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestCountWaits pins the board's sentence for a sequential position the line
// has moved onto before its bin arrived (L3, owner 2026-09-30), and that it
// says nothing in every other shape.
func TestCountWaits(t *testing.T) {
	t.Parallel()
	bin := int64(4201)
	seq := &processes.NodeClaim{SwapMode: protocol.SwapModeSequential, PairedCoreNode: "SEQ-A"}
	cases := []struct {
		name  string
		claim *processes.NodeClaim
		rt    *processes.RuntimeState
		want  string
	}{
		{"the line is on it and no bin is bound", seq, &processes.RuntimeState{ActivePull: true},
			"The line is on SEQ-B; its parts wait for SEQ-B's bin."},
		{"a bin is bound", seq, &processes.RuntimeState{ActivePull: true, ActiveBinID: &bin}, ""},
		{"the line is on the partner", seq, &processes.RuntimeState{}, ""},
		{"not sequential", &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, PairedCoreNode: "SEQ-A"},
			&processes.RuntimeState{ActivePull: true}, ""},
		{"not paired", &processes.NodeClaim{SwapMode: protocol.SwapModeSequential},
			&processes.RuntimeState{ActivePull: true}, ""},
		{"no claim", nil, &processes.RuntimeState{ActivePull: true}, ""},
		{"no runtime", seq, nil, ""},
	}
	for _, c := range cases {
		if got := countWaits("SEQ-B", c.claim, c.rt); got != c.want {
			t.Errorf("%s: countWaits = %q, want %q", c.name, got, c.want)
		}
	}
}
