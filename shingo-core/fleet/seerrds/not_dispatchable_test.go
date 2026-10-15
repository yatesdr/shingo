package seerrds

import (
	"testing"

	"shingocore/rds"
)

// TestNotDispatchableWhy_ThroughTheMapper pins the robot pop-up's sentence
// from the wire: RDS's undispatchable_reason and lock_info, through
// mapRobotStatus, to fleet.RobotStatus.NotDispatchableWhy. The first case is
// Hopkinsville's AMR-13 on 2026-10-15 as RDS reported it: set dispatchable,
// still not, because something local held control.
func TestNotDispatchableWhy_ThroughTheMapper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rds.RobotStatus
		want string
	}{
		{"control held elsewhere", rds.RobotStatus{
			ConnectionStatus:     1,
			UndispatchableReason: rds.UndispatchableReason{Unlock: 1},
			LockInfo:             rds.LockInfo{Locked: true, NickName: "this", IP: "127.0.0.1"},
		}, "the fleet manager does not hold control (held by this@127.0.0.1)"},
		{"no lock held at all", rds.RobotStatus{
			ConnectionStatus:     1,
			UndispatchableReason: rds.UndispatchableReason{Unlock: 1},
		}, "the fleet manager does not hold control"},
		{"several reasons, in one sentence", rds.RobotStatus{
			ConnectionStatus:     1,
			UndispatchableReason: rds.UndispatchableReason{UnconfirmedReloc: true, LowBattery: true, DispatchableStatus: 1},
		}, "relocalization not confirmed; low battery; taken out of dispatch"},
		{"wrong map", rds.RobotStatus{
			ConnectionStatus:     1,
			UndispatchableReason: rds.UndispatchableReason{CurrentMapInvalid: true},
		}, "on a map the fleet manager is not using"},
		{"no reason given", rds.RobotStatus{ConnectionStatus: 1}, "the fleet manager gives no reason"},
		{"dispatchable", rds.RobotStatus{
			ConnectionStatus: 1, Dispatchable: true,
			LockInfo: rds.LockInfo{Locked: true, NickName: "core", IP: "192.0.2.10"},
		}, ""},
	}
	for _, c := range cases {
		if got := mapRobotStatus(c.in).NotDispatchableWhy(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
