package seerrds

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/rds"
)

// TestMapState_KnownStates verifies every documented RDS state maps to the
// expected canonical protocol status.
func TestMapState_KnownStates(t *testing.T) {
	cases := []struct {
		name        string
		vendorState string
		want        string
	}{
		{"created", string(rds.StateCreated), protocol.StatusDispatched},
		{"to_be_dispatched", string(rds.StateToBeDispatched), protocol.StatusDispatched},
		{"running", string(rds.StateRunning), protocol.StatusInTransit},
		{"waiting", string(rds.StateWaiting), protocol.StatusStaged},
		{"finished", string(rds.StateFinished), protocol.StatusDelivered},
		{"failed", string(rds.StateFailed), protocol.StatusFailed},
		{"stopped", string(rds.StateStopped), protocol.StatusCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MapState(tc.vendorState)
			if got != tc.want {
				t.Errorf("MapState(%q) = %q, want %q", tc.vendorState, got, tc.want)
			}
		})
	}
}

// TestMapState_Unknown verifies unrecognized vendor states fall back to
// "dispatched" rather than returning an empty/invalid status.
func TestMapState_Unknown(t *testing.T) {
	cases := []string{"", "BOGUS", "running", "finished"} // lower-case variants should not match
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			got := MapState(s)
			if got != protocol.StatusDispatched {
				t.Errorf("MapState(%q) = %q, want fallback %q", s, got, protocol.StatusDispatched)
			}
		})
	}
}

// TestIsTerminalState verifies terminality matches rds.OrderState.IsTerminal.
func TestIsTerminalState(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{string(rds.StateFinished), true},
		{string(rds.StateFailed), true},
		{string(rds.StateStopped), true},
		{string(rds.StateCreated), false},
		{string(rds.StateToBeDispatched), false},
		{string(rds.StateRunning), false},
		{string(rds.StateWaiting), false},
		{"", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			if got := IsTerminalState(tc.state); got != tc.want {
				t.Errorf("IsTerminalState(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestMapOrderSnapshot_FullyPopulated verifies every field, including nested
// block and message slices, is copied verbatim into the fleet snapshot.
func TestMapOrderSnapshot_FullyPopulated(t *testing.T) {
	d := &rds.OrderDetail{
		ID:           "order-xyz",
		Vehicle:      "AMB-07",
		State:        rds.StateRunning,
		CreateTime:   1700000000000,
		TerminalTime: 1700000100000,
		Blocks: []rds.BlockDetail{
			{BlockID: "b1", Location: "LINE-1", State: rds.StateFinished},
			{BlockID: "b2", Location: "DEST", State: rds.StateRunning},
		},
		Errors: []rds.OrderMessage{
			{Code: 101, Desc: "boom", Times: 2, Timestamp: 1700000001000},
		},
		Warnings: []rds.OrderMessage{
			{Code: 202, Desc: "slow", Times: 1, Timestamp: 1700000002000},
		},
		Notices: []rds.OrderMessage{
			{Code: 303, Desc: "fyi", Times: 3, Timestamp: 1700000003000},
		},
	}

	got := mapOrderSnapshot(d)
	if got == nil {
		t.Fatal("mapOrderSnapshot returned nil")
	}

	if got.VendorOrderID != "order-xyz" {
		t.Errorf("VendorOrderID = %q, want order-xyz", got.VendorOrderID)
	}
	if got.State != string(rds.StateRunning) {
		t.Errorf("State = %q, want RUNNING", got.State)
	}
	if got.Vehicle != "AMB-07" {
		t.Errorf("Vehicle = %q, want AMB-07", got.Vehicle)
	}
	if got.CreateTime != 1700000000000 {
		t.Errorf("CreateTime = %d, want 1700000000000", got.CreateTime)
	}
	if got.TerminalTime != 1700000100000 {
		t.Errorf("TerminalTime = %d, want 1700000100000", got.TerminalTime)
	}

	if len(got.Blocks) != 2 {
		t.Fatalf("Blocks len = %d, want 2", len(got.Blocks))
	}
	wantBlock0 := fleet.BlockSnapshot{BlockID: "b1", Location: "LINE-1", State: string(rds.StateFinished)}
	if got.Blocks[0] != wantBlock0 {
		t.Errorf("Blocks[0] = %+v, want %+v", got.Blocks[0], wantBlock0)
	}
	wantBlock1 := fleet.BlockSnapshot{BlockID: "b2", Location: "DEST", State: string(rds.StateRunning)}
	if got.Blocks[1] != wantBlock1 {
		t.Errorf("Blocks[1] = %+v, want %+v", got.Blocks[1], wantBlock1)
	}

	if len(got.Errors) != 1 || got.Errors[0].Code != 101 || got.Errors[0].Desc != "boom" ||
		got.Errors[0].Times != 2 || got.Errors[0].Timestamp != 1700000001000 {
		t.Errorf("Errors = %+v, want single msg {101, boom, 2, 1700000001000}", got.Errors)
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Code != 202 || got.Warnings[0].Desc != "slow" {
		t.Errorf("Warnings = %+v, want single msg {202, slow, ...}", got.Warnings)
	}
	if len(got.Notices) != 1 || got.Notices[0].Code != 303 || got.Notices[0].Desc != "fyi" {
		t.Errorf("Notices = %+v, want single msg {303, fyi, ...}", got.Notices)
	}
}

// TestMapOrderSnapshot_EmptyDetail verifies a zero-valued detail produces a
// non-nil snapshot with empty slices (not nil pointer, not panic).
func TestMapOrderSnapshot_EmptyDetail(t *testing.T) {
	d := &rds.OrderDetail{}
	got := mapOrderSnapshot(d)
	if got == nil {
		t.Fatal("mapOrderSnapshot(empty) returned nil")
	}
	if got.VendorOrderID != "" {
		t.Errorf("VendorOrderID = %q, want empty", got.VendorOrderID)
	}
	if got.State != "" {
		t.Errorf("State = %q, want empty", got.State)
	}
	if len(got.Blocks) != 0 {
		t.Errorf("Blocks len = %d, want 0", len(got.Blocks))
	}
	if len(got.Errors) != 0 || len(got.Warnings) != 0 || len(got.Notices) != 0 {
		t.Errorf("message slices should be empty, got errs=%d warns=%d notices=%d",
			len(got.Errors), len(got.Warnings), len(got.Notices))
	}
}

// TestMapRobotStatus_AllFields verifies every mapped field reaches the output
// struct with the correct value. Connected is derived (connection_status != 0).
func TestMapRobotStatus_AllFields(t *testing.T) {
	in := rds.RobotStatus{
		VehicleID:        "AMB-01",
		ConnectionStatus: 1, // nonzero -> Connected = true
		Dispatchable:     true,
		ProcBusiness:     true,
		IsError:          true,
		NetworkDelay:     42,
		BasicInfo: rds.RobotBasicInfo{
			IP:          "10.0.0.5",
			Model:       "JS-200",
			Version:     "v3.1.0",
			CurrentMap:  "warehouse_A",
			CtrlTemp:    35.5,
			CtrlHumi:    40.0,
			CtrlVoltage: 48.1,
		},
		RbkReport: rds.RbkReport{
			X:              1.1,
			Y:              2.2,
			Angle:          0.75,
			BatteryLevel:   0.87,
			Charging:       true,
			Blocked:        true,
			Emergency:      true,
			CurrentStation: "STN-3",
			LastStation:    "STN-2",
			Odo:            1234.5,
			TodayOdo:       67.8,
			Time:           600_000,
			TotalTime:      90_000_000,
			Voltage:        47.9,
			Current:        -2.1,
			Jack: rds.JackReport{
				JackLoadTimes: 12,
				JackHeight:    0.150,
				JackErrorCode: 0,
			},
		},
	}

	got := mapRobotStatus(in)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"VehicleID", got.VehicleID, "AMB-01"},
		{"Connected", got.Connected, true},
		{"Available", got.Available, true},
		{"Busy", got.Busy, true},
		{"Emergency", got.Emergency, true},
		{"Blocked", got.Blocked, true},
		{"IsError", got.IsError, true},
		{"BatteryLevel", got.BatteryLevel, 87.0},
		{"Charging", got.Charging, true},
		{"CurrentMap", got.CurrentMap, "warehouse_A"},
		{"Model", got.Model, "JS-200"},
		{"IP", got.IP, "10.0.0.5"},
		{"X", got.X, 1.1},
		{"Y", got.Y, 2.2},
		{"Angle", got.Angle, 0.75},
		{"NetworkDelay", got.NetworkDelay, 42},
		{"CurrentStation", got.CurrentStation, "STN-3"},
		{"LastStation", got.LastStation, "STN-2"},
		{"OdoTotal", got.OdoTotal, 1234.5},
		{"OdoToday", got.OdoToday, 67.8},
		{"SessionMs", got.SessionMs, int64(600_000)},
		{"TotalMs", got.TotalMs, int64(90_000_000)},
		{"LiftCount", got.LiftCount, 12},
		{"LiftHeight", got.LiftHeight, 0.150},
		{"LiftError", got.LiftError, 0},
		{"BatteryV", got.BatteryV, 47.9},
		{"BatteryA", got.BatteryA, -2.1},
		{"CtrlTemp", got.CtrlTemp, 35.5},
		{"CtrlHumi", got.CtrlHumi, 40.0},
		{"CtrlVoltage", got.CtrlVoltage, 48.1},
		{"Version", got.Version, "v3.1.0"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestMapRobotStatus_DisconnectedZeroStatus verifies that connection_status == 0
// surfaces as Connected = false — the State() computed status then derives
// "offline" downstream.
func TestMapRobotStatus_DisconnectedZeroStatus(t *testing.T) {
	in := rds.RobotStatus{
		VehicleID:        "AMB-OFF",
		ConnectionStatus: 0, // disconnected
		Dispatchable:     false,
	}
	got := mapRobotStatus(in)
	if got.Connected {
		t.Error("Connected = true, want false when ConnectionStatus == 0")
	}
	if got.Available {
		t.Error("Available = true, want false when Dispatchable == false")
	}
	if got.State() != "offline" {
		t.Errorf("State() = %q, want offline", got.State())
	}
}

// TestMapRobotStatus_Zero verifies that a zero rds.RobotStatus produces a
// fully-zero fleet.RobotStatus (no panics, no surprise defaults).
func TestMapRobotStatus_Zero(t *testing.T) {
	got := mapRobotStatus(rds.RobotStatus{})
	var want fleet.RobotStatus
	if got != want {
		t.Errorf("mapRobotStatus(zero) = %+v, want zero value", got)
	}
}
