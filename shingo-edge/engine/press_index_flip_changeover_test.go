package engine

import (
	"strings"
	"testing"

	"shingoedge/store/processes"
)

// THE FLIP STOPPED AT THE CHANGEOVER BOUNDARY.
//
// IndexRobotSupplies moves the supermarket trip between the two robots, and
// BuildTwoRobotPressIndexSwapSteps reads it. buildPressIndexChangeoverSwap never
// did. So a press configured with the flip ran R2-supplies all shift and then
// inverted the two robots' roles the moment a changeover started — same cell,
// same hardware, opposite choreography, decided by which builder happened to be
// running.
func TestPressIndexChangeover_HonoursTheFlip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		secondPaired string
		backPosition string
	}{
		{"two position", "", "INDEX-B"},
		{"three position", "INDEX-C", "INDEX-C"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			to := &processes.NodeClaim{
				CoreNodeName:  "PRESS",
				InboundSource: "MARKET-EMPTIES",
				PayloadCode:   "PART-B",
			}

			flipped := buildPressIndexChangeoverSwap(flipClaim("consume", tc.secondPaired, true), to, false)
			r1 := legTrace(flipped.Roles.evac.steps)
			r2 := legTrace(flipped.Roles.supply.steps)

			if strings.Contains(r1, "pickup@MARKET-EMPTIES") {
				t.Errorf("flipped: R1 still fetches from the supermarket.\nR1: %s", r1)
			}
			if !strings.Contains(r2, "pickup@MARKET-EMPTIES") {
				t.Errorf("flipped: R2 does not fetch from the supermarket, so the flip did not "+
					"reach the changeover builder at all.\nR2: %s", r2)
			}
			if !strings.HasSuffix(r2, "dropoff@"+tc.backPosition) {
				t.Errorf("flipped: R2's fresh carrier must land at the back position %s.\nR2: %s",
					tc.backPosition, r2)
			}

			unflipped := buildPressIndexChangeoverSwap(flipClaim("consume", tc.secondPaired, false), to, false)
			u1 := legTrace(unflipped.Roles.evac.steps)
			u2 := legTrace(unflipped.Roles.supply.steps)
			if !strings.Contains(u1, "pickup@MARKET-EMPTIES") {
				t.Errorf("unflipped: R1 owns the supermarket trip.\nR1: %s", u1)
			}
			if strings.Contains(u2, "pickup@MARKET-EMPTIES") {
				t.Errorf("unflipped: R2 must not fetch.\nR2: %s", u2)
			}

			// THE FLIP MOVES THE FETCH AND NOTHING ELSE. The press pickup and
			// the press dropoff stay put in both states — that is what makes it
			// a rearrangement rather than a different choreography.
			if !strings.Contains(r1, "pickup@PRESS") || !strings.Contains(u1, "pickup@PRESS") {
				t.Errorf("the press pickup moved with the flip.\nflipped R1: %s\nunflipped R1: %s", r1, u1)
			}
			if !strings.Contains(r2, "dropoff@PRESS") || !strings.Contains(u2, "dropoff@PRESS") {
				t.Errorf("the press dropoff moved with the flip.\nflipped R2: %s\nunflipped R2: %s", r2, u2)
			}
		})
	}
}
