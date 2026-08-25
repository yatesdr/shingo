//go:build sim

package simulator

import (
	"testing"
	"time"

	"shingocore/config"
)

// ── THE SIM FLEET IS THE DRIVER'S POOL ────────────────────────────────────
//
// This test used to assert one synthetic robot per ACTIVE ORDER, named
// SIM-ROBOT-N. That contract was wrong three ways and this is the replacement.
//
//   - the names did not match orders.robot_id, which carries the driver's
//     AMR-nn, so anything joining a board row to a database row found nothing;
//   - a robot existed only while its order did, so the fleet emptied whenever
//     the plant went quiet and a bin left on a deck belonged to a robot the
//     board said did not exist;
//   - every row was Available:false, which fails the recovery dispatchability
//     gate unconditionally.
//
// (RobotLister is sim-build only — see parity.go for why; in non-sim builds the
// simulator is not a RobotLister.)
func TestRobotListerReportsTheDriverPool(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, FleetSize: 3}
	d, s, m, _ := newTestDriver(t, cfg, 1)
	s.driver = d // what NewDriverFromConfig does in production

	// Before a tick there is no snapshot, and an empty fleet is the honest
	// answer rather than an invented one.
	robots, err := s.GetRobotsStatus()
	if err != nil {
		t.Fatalf("GetRobotsStatus: %v", err)
	}
	if len(robots) != 0 {
		t.Fatalf("want no robots before the driver has stepped, got %d", len(robots))
	}

	mkTransport(t, s, "o1")
	mkTransport(t, s, "o2")
	// Enough ticks for both orders to leave CREATED and take a robot, not
	// enough for either to finish.
	runTicks(d, m, 6)

	robots, err = s.GetRobotsStatus()
	if err != nil {
		t.Fatalf("GetRobotsStatus: %v", err)
	}
	// THE WHOLE POOL, not just the working part. A fleet that shrinks when the
	// plant goes quiet answers "no" to every "is there a robot that could take
	// this".
	if len(robots) != cfg.FleetSize {
		t.Fatalf("want all %d pool members, got %d: %+v", cfg.FleetSize, len(robots), robots)
	}

	byID := map[string]bool{}
	busy, free := 0, 0
	for _, r := range robots {
		byID[r.VehicleID] = true
		if !r.Connected {
			t.Errorf("%s is not connected — a simulated robot is always reachable", r.VehicleID)
		}
		if r.Busy == r.Available {
			t.Errorf("%s reports Busy=%v Available=%v — a robot is one or the other",
				r.VehicleID, r.Busy, r.Available)
		}
		if r.Busy {
			busy++
		} else {
			free++
		}
	}
	// THE NAMES ARE THE DRIVER'S DURABLE ONES, which is what lands in
	// orders.robot_id and on the waybill.
	for _, want := range []string{"AMR-01", "AMR-02", "AMR-03"} {
		if !byID[want] {
			t.Errorf("pool member %s is missing from the fleet: %+v", want, robots)
		}
	}
	if busy != 2 {
		t.Errorf("two orders are running, so two robots are busy; got %d", busy)
	}
	if free != 1 {
		t.Errorf("the third pool member is idle and must report Available; got %d free", free)
	}
}

// A ROBOT OUTLIVES ITS ORDER. The old shape minted robots from active orders,
// so a robot vanished the moment its order went terminal — and a bin left
// riding that robot's deck belonged to a robot nothing could look up. Recovery
// reads that lookup.
func TestRobotListerKeepsRobotsAfterTheirOrdersFinish(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 2 * time.Second, FleetSize: 2}
	d, s, m, _ := newTestDriver(t, cfg, 1)
	s.driver = d

	mkTransport(t, s, "o1")
	runTicks(d, m, 40) // long enough for the order to finish

	robots, err := s.GetRobotsStatus()
	if err != nil {
		t.Fatalf("GetRobotsStatus: %v", err)
	}
	if len(robots) != cfg.FleetSize {
		t.Fatalf("the fleet must not shrink when the plant goes quiet; got %d of %d: %+v",
			len(robots), cfg.FleetSize, robots)
	}
	for _, r := range robots {
		if !r.Available {
			t.Errorf("%s is still busy after every order finished: %+v", r.VehicleID, r)
		}
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
