//go:build sim

package simulator

import (
	"fmt"

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

// GetRobotsStatus synthesizes one robot per active order, with a stable
// SIM-ROBOT-N id (N = position among active orders in creation order) so the
// robots board renders something coherent in sim mode. Position is approximated
// by the order's first block location — the simulator itself doesn't track
// which block a robot is on (that's the driver's private bookkeeping).
func (s *SimulatorBackend) GetRobotsStatus() ([]fleet.RobotStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	robots := make([]fleet.RobotStatus, 0)
	n := 0
	for _, id := range s.orderSeq {
		o, ok := s.orders[id]
		if !ok || !isActiveRobotState(o.state) {
			continue
		}
		n++
		loc := ""
		if len(o.blocks) > 0 {
			loc = o.blocks[0].location
		}
		robots = append(robots, fleet.RobotStatus{
			VehicleID:      fmt.Sprintf("SIM-ROBOT-%d", n),
			Connected:      true,
			Available:      false,
			Busy:           true,
			BatteryLevel:   100,
			Model:          "SimBot",
			CurrentMap:     "sim",
			CurrentStation: loc,
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
