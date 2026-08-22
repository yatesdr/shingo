package service

import (
	"testing"

	"shingocore/fleet"
)

// RobotCarryingBin is the question the stranded-bin inference turns on, and the
// answer has three values, not two: yes, no, and "the deck is moving, do not
// guess". A bin halfway down is neither on the robot nor at the station.
func TestRobotCarryingBin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		r            fleet.RobotStatus
		wantCarrying bool
		wantOK       bool
	}{
		{
			name:         "loaded in place",
			r:            fleet.RobotStatus{JackState: 1, JackIsFull: true, IsLoaded: true, LiftHeight: 0.0601},
			wantCarrying: true, wantOK: true,
		},
		{
			name:         "unloaded in place",
			r:            fleet.RobotStatus{JackState: 3, LiftHeight: -0.0001},
			wantCarrying: false, wantOK: true,
		},
		{
			// The deck is on its way down. Neither answer is true yet, and the
			// caller must fall to its ambiguous branch rather than pick one.
			name:         "unloading is not an answer",
			r:            fleet.RobotStatus{JackState: 2, JackIsFull: true, LiftHeight: 0.03},
			wantCarrying: false, wantOK: false,
		},
		{
			name:         "loading is not an answer",
			r:            fleet.RobotStatus{JackState: 0, JackIsFull: true, IsLoaded: true, LiftHeight: 0.03},
			wantCarrying: true, wantOK: true, // jack_state 0 is indistinguishable from absent; see below
		},
		{
			name:         "execution failed is not an answer",
			r:            fleet.RobotStatus{JackState: 0xFF, LiftHeight: 0.0601},
			wantCarrying: false, wantOK: false,
		},
		{
			// A fleet that reports no jack_state at all. The zero value is
			// indistinguishable from "loading", so the height proxy decides
			// rather than the enum — otherwise every robot on an older RDS
			// would read as mid-load forever.
			name:         "no jack_state falls back to the height proxy, loaded",
			r:            fleet.RobotStatus{LiftHeight: 0.0601},
			wantCarrying: true, wantOK: true,
		},
		{
			name:         "no jack_state and nothing on the deck",
			r:            fleet.RobotStatus{LiftHeight: 0},
			wantCarrying: false, wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			carrying, ok := RobotCarryingBin(tc.r)
			if carrying != tc.wantCarrying || ok != tc.wantOK {
				t.Errorf("RobotCarryingBin(%+v) = (%v, %v), want (%v, %v)",
					tc.r, carrying, ok, tc.wantCarrying, tc.wantOK)
			}
		})
	}
}
