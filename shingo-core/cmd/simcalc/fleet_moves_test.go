package main

import (
	"testing"

	"shingo/protocol"
	"shingocore/plantspec"
)

// TestFleetMovesPerSwap_CostsTheClaim: simcalc costs a claim, not a mode name.
// A sequential cell's backfill is made while its removal moves, so its peak is
// two robots; an index press is two, or three when Robot 3 brings its back.
func TestFleetMovesPerSwap_CostsTheClaim(t *testing.T) {
	t.Parallel()
	press := func(r protocol.RefillBy) plantspec.Claim {
		return plantspec.Claim{SwapMode: string(protocol.SwapModeTwoRobotPressIndex), RefillBy: r}
	}
	for _, c := range []struct {
		name      string
		claim     plantspec.Claim
		crossings float64
		robots    int
	}{
		{"sequential", plantspec.Claim{SwapMode: string(protocol.SwapModeSequential)}, 2, 2},
		{"index press, Robot 1", press(""), 2, 2},
		{"index press, Robot 2", press(protocol.RefillByIndexRobot), 2, 2},
		{"index press, Robot 3", press(protocol.RefillByOwnRobot), 2, 3},
		{"two robot", plantspec.Claim{SwapMode: string(protocol.SwapModeTwoRobot)}, 2, 2},
	} {
		if x, r := fleetMovesPerSwap(c.claim); x != c.crossings || r != c.robots {
			t.Errorf("%s: %v crossings and %d robots, want %v and %d", c.name, x, r, c.crossings, c.robots)
		}
	}
}
