//go:build sim

package simulator

import "testing"

// T2.4: one synthetic robot per active order, stable SIM-ROBOT-N ids; CREATED
// orders (no robot yet) don't count. (RobotLister is sim-build only — see
// parity.go for why; in non-sim builds the simulator is not a RobotLister.)
func TestRobotListerSynthesizesPerActiveOrder(t *testing.T) {
	s := New()
	v1 := mkTransport(t, s, "o1")
	v2 := mkTransport(t, s, "o2")
	mkTransport(t, s, "o3") // stays CREATED → no robot

	s.DriveState(v1, "RUNNING")
	s.DriveState(v2, "RUNNING")

	robots, err := s.GetRobotsStatus()
	if err != nil {
		t.Fatalf("GetRobotsStatus: %v", err)
	}
	if len(robots) != 2 {
		t.Fatalf("want 2 robots for 2 active orders, got %d", len(robots))
	}
	if robots[0].VehicleID != "SIM-ROBOT-1" || robots[1].VehicleID != "SIM-ROBOT-2" {
		t.Fatalf("want stable SIM-ROBOT-1/2, got %s/%s", robots[0].VehicleID, robots[1].VehicleID)
	}
	if !robots[0].Connected || !robots[0].Busy {
		t.Fatalf("synthetic robot should be connected+busy: %+v", robots[0])
	}
	if robots[0].State() != "busy" {
		t.Fatalf("want computed state busy, got %s", robots[0].State())
	}
}

// T2.4: the control ops satisfy RobotLister without erroring (no real robot).
func TestRobotControlOpsAreNoOps(t *testing.T) {
	s := New()
	if err := s.SetAvailability("SIM-ROBOT-1", true); err != nil {
		t.Fatalf("SetAvailability: %v", err)
	}
	if err := s.RetryFailed("SIM-ROBOT-1"); err != nil {
		t.Fatalf("RetryFailed: %v", err)
	}
	if err := s.ForceComplete("SIM-ROBOT-1"); err != nil {
		t.Fatalf("ForceComplete: %v", err)
	}
}
