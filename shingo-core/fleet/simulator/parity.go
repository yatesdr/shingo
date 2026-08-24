//go:build sim

package simulator

import (
	"shingocore/fleet"
)

// Interface parity (brief T2.4 / D5) — SIM BUILDS ONLY. Production type-asserts
// the fleet backend to optional interfaces; the simulator implements RobotLister
// so the sim-mode robots board renders instead of 501ing.
//
// This file is //go:build sim on purpose. When it was untagged the simulator
// advertised these interfaces in EVERY build, which broke the docker test suite
// two ways: the "RobotLister unsupported -> 501" handler tests started returning
// 200, and — worse — the engine's auto SceneSync treated the simulator as a
// SceneSyncer and DELETED every DB node missing from its (empty) synthetic
// scene (bins_node_id_fkey violations cascading into node-not-found failures).
//
// SceneSyncer is deliberately NOT implemented at all. SceneSync (engine_scene_
// sync.go) treats GetSceneAreas as the authoritative scene and reaps DB nodes
// not in it; a synthetic sim scene would wipe the seeded topology in the dev
// runtime too. The seed tool owns the nodes, not a robot scene — so SceneSync
// reports "unsupported" and the robot-map stays empty (acceptable, brief §8).
var _ fleet.RobotLister = (*SimulatorBackend)(nil)

// isActiveRobotState reports whether an order currently has a robot assigned to
// it (moving or dwelling at a wait point) — the orders that synthesize a robot.
func isActiveRobotState(state string) bool {
	return state == "RUNNING" || state == "WAITING"
}

// GetRobotsStatus reports THE DRIVER'S FLEET — the durable, named pool the
// simulator actually assigns work from.
//
// IT USED TO MINT ONE ROBOT PER ACTIVE ORDER, named SIM-ROBOT-N by position in
// creation order, with Available:false and Busy:true on every row. That was
// wrong in three ways at once, and each one broke something:
//
//   - THE NAMES DID NOT MATCH. The driver has kept a durable named pool
//     (AMR-01..NN, exclusive FIFO) since G16, and that is what lands in
//     orders.robot_id and on every waybill. A board keyed on SIM-ROBOT-1 and a
//     database keyed on AMR-03 describe two different fleets, so anything that
//     joins them — the carried-bin recovery's robot lookup above all — found
//     nothing.
//   - ROBOTS VANISHED. A robot existed only while its order was active, so the
//     moment an order went terminal its robot left the fleet. A bin left riding
//     that robot's deck belongs to a robot the board says does not exist.
//   - NOTHING WAS EVER DISPATCHABLE. Available:false on every row fails the
//     recovery gate unconditionally: robotCanTakeARecoveryOrder refuses a robot
//     the plant has taken out of the dispatch pool, and every simulated robot
//     looked taken out.
//
// So the pool is the answer: in-use robots by their real name and Busy, free
// robots Available. The driver publishes it once per tick (see Driver.Fleet).
//
// TIER 3 OF THE RECOVERY DESTINATION FALLBACK STAYS UNREACHABLE IN SIM, and
// that is not fixed here. It resolves "the node the robot is parked at", and
// the driver has no position model — CurrentStation below is the order's first
// block location for a busy robot and empty for a free one, neither of which is
// where the robot is. A sim run exercises tiers 1 and 2; tier 3 needs a
// position model that does not exist.
func (s *SimulatorBackend) GetRobotsStatus() ([]fleet.RobotStatus, error) {
	d := s.typedDriver()
	if d == nil {
		// No driver: nothing is assigning work, so there is no fleet. Honest,
		// and it keeps the non-driver test backends from inventing robots.
		return []fleet.RobotStatus{}, nil
	}
	robots := make([]fleet.RobotStatus, 0)
	for _, r := range d.Fleet() {
		robots = append(robots, fleet.RobotStatus{
			VehicleID:    r.ID,
			Connected:    true,
			Available:    !r.Busy,
			Busy:         r.Busy,
			BatteryLevel: 100,
			Model:        "SimBot",
			CurrentMap:   "sim",
			// Localization is chosen, not defaulted. The zero value of
			// RelocStatus is 0 = FAILED, which would make every simulated
			// robot read as a localization failure and hand the confidence
			// collector a fleet that is permanently lost. SUCCESS + a
			// healthy 0.95 is the honest sim equivalent.
			//
			// The simulator has no position model, so these rows all land at
			// (0,0). That is fine for exercising the write path — clause 4
			// (on-task and stationary) is what fires in sim — but sim rows
			// carry no spatial signal and no segment statistic should be
			// read from them.
			// Empty, not nil-by-omission: the simulator has no advanced-area
			// model, and "this robot is in no special area" is the honest
			// sim answer rather than "we never looked". Written explicitly
			// so a reader does not have to infer it from the zero value.
			AreaIDs:     []string{},
			Confidence:  0.95,
			RelocStatus: 1,
			// One map, named, and the SAME hash on every simulated robot.
			// The whole point of carrying a per-robot map hash is that a
			// real fleet can be split across maps; a sim fleet never is, and
			// stamping a constant here means the roll-up's map-mismatch
			// quarantine sees a unanimous fleet and quarantines nothing —
			// which is the correct sim behaviour and is now asserted rather
			// than arrived at by every robot sharing the empty string.
			MapMD5:         "sim-map-md5",
			CurrentStation: r.At,
		})
	}
	return robots, nil
}

// SetAvailability is a no-op for the simulator (no real robot to pause).
func (s *SimulatorBackend) SetAvailability(vehicleID string, available bool) error { return nil }

// RetryFailed is a no-op for the simulator.
func (s *SimulatorBackend) RetryFailed(vehicleID string) error { return nil }

// ForceComplete is a no-op for the simulator (orders complete via the driver).
func (s *SimulatorBackend) ForceComplete(vehicleID string) error { return nil }
