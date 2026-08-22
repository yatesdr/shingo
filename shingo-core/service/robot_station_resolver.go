package service

import (
	"strings"

	"shingocore/fleet"
	"shingocore/store/nodes"
)

// jackStateLoadedInPlace / jackStateUnloadedInPlace are the two AT-REST values
// of the deck's state machine (Robokit API Protocol §11027). The moving values
// (0 loading, 2 unloading) and 4/0xFF are deliberately not named: nothing here
// should act on a deck mid-travel.
const (
	jackStateLoadedInPlace   = 1
	jackStateUnloadedInPlace = 3
)

// liftLoadedThresholdM is the fallback proxy for a fleet that reports no
// jack_state. Measured at Springfield: a loaded deck reads 0.0601 m and an
// empty one reads ≈ -0.0001 m, so anything above half a centimetre is a bin.
//
// A PROXY, NOT THE SIGNAL. JackState is read first wherever it is present,
// because a height is a measurement of a mast and a state is the vendor's own
// answer to the question being asked.
const liftLoadedThresholdM = 0.005

// RobotCarryingBin reports whether a robot's deck has something on it, and
// whether that reading is trustworthy enough to act on.
//
// ok is false while the deck is MOVING (jack_state 0 or 2) or in its error
// states, because a bin halfway down is neither on the robot nor at the
// station, and either answer would be a guess. The stranded-bin inference falls
// to its ambiguous branch rather than picking one.
func RobotCarryingBin(r fleet.RobotStatus) (carrying, ok bool) {
	switch r.JackState {
	case jackStateLoadedInPlace:
		return true, true
	case jackStateUnloadedInPlace:
		return false, true
	case 0:
		// No jack_state on the wire at all — an older RDS, or a fleet whose
		// robots do not report one. Fall back to the height proxy rather than
		// reading the zero value as "loading".
		if !r.JackIsFull && !r.IsLoaded && r.LiftHeight == 0 {
			return false, false
		}
		return r.LiftHeight > liftLoadedThresholdM || r.JackIsFull || r.IsLoaded, true
	default:
		// Moving, stopped mid-travel, or failed.
		return false, false
	}
}

// ResolveRobotStation maps the RDS point a robot is sitting at to a Core node.
//
// The mapping is IDENTITY, and that is worth stating because it looks like it
// should need a table: Core sends the node's own dot-name as the RDS block
// Location (dispatch/complex_steps.go), so the point names RDS reports back are
// Core node names. A point that does not resolve is a real answer — charging
// stations, waypoints and parking spots are RDS points that are not Core nodes.
//
// LastStation is tried after CurrentStation because CurrentStation is empty
// while a robot is between points, which is most of the time it is moving. A
// robot that is parked reports both.
//
// Synthetic nodes are refused. _TRANSIT and the per-robot carrier nodes are
// bookkeeping, not places, and resolving a station onto one would move a bin to
// a location that does not exist on the floor.
func ResolveRobotStation(s *NodeService, r fleet.RobotStatus) (*nodes.Node, bool) {
	for _, name := range []string{r.CurrentStation, r.LastStation} {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		node, err := s.GetByName(name)
		if err != nil || node == nil {
			continue
		}
		if node.IsSynthetic {
			continue
		}
		return node, true
	}
	return nil, false
}
