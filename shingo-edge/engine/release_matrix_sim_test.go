//go:build sim

package engine

// release_matrix_sim_test.go — door 11: the sim's auto-operator.
//
// The sim operator presses RELEASE the way a person would, through the same
// doors: the pair door for a two-robot node, the per-order trunk for the rest.
// These cells hold it to the matrix's outcome format, so a door change shows up
// here the same way it shows up for a person.

import (
	"context"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/config"
)

// simPress drives the sim operator's release worker for a leg, with the swap
// release delay cut to a millisecond.
func simPress(leg string) func(h *relHarness) error {
	return func(h *relHarness) error {
		op := &simOperator{
			e:         h.eng,
			ops:       config.SimOperatorsConfig{SwapRelease: time.Millisecond},
			clk:       clock.Real(),
			ctx:       context.Background(),
			pending:   make(map[int64]bool),
			releasing: make(map[int64]bool),
		}
		op.runRelease(h.leg(leg))
		return nil
	}
}

func TestReleaseMatrixSim(t *testing.T) {
	t.Parallel()
	S := protocol.StatusStaged
	runRelCells(t, []relCell{
		{name: "d11/two_robot pair",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0",
			build: pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S),
			act:   simPress("evac")},
		// The 2026-08-28 incident's shape: the sim pressed RELEASE on the side
		// the line was pulling from. fact-owners Lane G flips onto a ready
		// partner and releases.
		{name: "d11/sequential pulled side, partner ready",
			want: "ok | removal=in_transit | rel=removal | ingest=0 capred=0 | pull=SYN-PRESS-B",
			build: func(h *relHarness) {
				h.sequentialAB(protocol.ClaimRoleConsume, true)
				statuses(h, "removal", S)
			},
			act: simPress("removal"), probe: probes(pPull)},
	})
}
