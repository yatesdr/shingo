// stranded_transit.go — where did the bin go.
//
// A bin the robot has picked up sits at `_TRANSIT` with claimed_by set. Cancel
// or fail the order and TerminalizeOrderWithReason releases the claim without
// touching node_id (store/orders.go), so the bin is now at `_TRANSIT`,
// unclaimed, and nobody knows where it physically is. The only exit was an
// operator finding it and pressing "I found it, it's at X".
//
// Most of that job is the FINDING. RDS can usually tell us: the robot is still
// in the cache, its deck reports whether it is carrying anything, and the point
// it is standing at is a Core node name. Three branches:
//
//	A — the deck is empty at a station we can name  → place the bin there
//	B — the deck is still loaded                    → the bin rides the robot (unit 13)
//	C — anything else                               → anomaly, with the robot's last position
//
// Nothing here bypasses a guard. Branch A goes through the same
// RecoverTransitAnomaly an operator's button calls, including its empty-node
// check, and falls to C when that refuses.

package engine

import (
	"fmt"
	"strings"

	"shingocore/fleet"
	"shingocore/service"
)

// inferredActor is the actor on the recovery_actions row and the bin audit. It
// is deliberately distinguishable from an operator: a placement nobody walked
// out and confirmed should be readable as such afterwards.
const inferredActor = "system:inferred"

// inferStrandedTransitBin runs the A/B/C decision for every bin the terminating
// order left at _TRANSIT.
//
// Called from the terminal handlers (the fast path) and from the reconciliation
// sweep (the guarantee). Best-effort throughout: this improves an operator's
// starting position and must never be able to fail a terminal transition, so
// every branch logs and returns rather than propagating.
func (e *Engine) inferStrandedTransitBin(orderID int64) {
	order, err := e.db.GetOrder(orderID)
	if err != nil || order == nil {
		e.logFn("engine: stranded transit: order %d: %v", orderID, err)
		return
	}
	// orders.bin_id, not claimed_by and not the order_bins junction: by the time
	// any terminal handler runs, TerminalizeOrderWithReason has cleared the
	// claim and deleted the junction rows. The order's own bin_id is what
	// survives its own terminalisation, and a compound's legs each carry theirs.
	if order.BinID == nil {
		return
	}
	bin, err := e.BinService().GetBin(*order.BinID)
	if err != nil || bin == nil {
		return
	}
	// Only a bin actually stranded: parked at _TRANSIT with nobody holding it.
	// A bin that reached its destination, or that another order has already
	// picked up, is not this function's business.
	if bin.NodeName != transitNodeName || bin.ClaimedBy != nil {
		return
	}
	robot, haveRobot := e.GetCachedRobotStatus(order.RobotID)
	e.placeStrandedBin(bin.ID, order.RobotID, robot, haveRobot)
}

// transitNodeName is the synthetic node a picked-up bin parks at.
const transitNodeName = "_TRANSIT"

// placeStrandedBin is the decision for one bin.
func (e *Engine) placeStrandedBin(binID int64, robotID string, robot fleet.RobotStatus, haveRobot bool) {
	if !haveRobot {
		// No robot in the cache — an order that never dispatched, or a fleet
		// Core has not heard from. There is nothing to infer from.
		e.strandedAnomaly(binID, robotID, robot, false, "no robot telemetry")
		return
	}

	carrying, certain := service.RobotCarryingBin(robot)
	if !certain {
		// The deck is mid-travel or in an error state. A bin halfway down is
		// neither on the robot nor at the station, and either answer would be
		// a guess an operator would then have to un-do.
		e.strandedAnomaly(binID, robotID, robot, true, "jack state not at rest")
		return
	}
	if carrying {
		// Branch B. Until unit 13 gives the bin somewhere to live, a loaded
		// deck is an anomaly that says so — which is still better than the
		// bare "lost" it produced before.
		e.strandedAnomaly(binID, robotID, robot, true, "bin still on the robot's deck")
		return
	}

	// Branch A. The deck is empty; if the robot is standing somewhere we can
	// name, that is where it put the bin.
	node, ok := service.ResolveRobotStation(e.NodeService(), robot)
	if !ok {
		e.strandedAnomaly(binID, robotID, robot, true, "robot is not at a node we know")
		return
	}
	if err := e.BinService().RecoverTransitAnomaly(binID, node.ID, inferredActor); err != nil {
		// The commonest refusal is an occupied node: the robot has moved on and
		// something else is in that slot, so the inference is stale. Fall to C
		// rather than forcing it — the empty-node guard is the reason this is
		// safe to run unattended at all.
		e.strandedAnomaly(binID, robotID, robot, true,
			fmt.Sprintf("could not place at %s: %v", node.Name, err))
		return
	}
	e.logFn("engine: stranded transit: bin %d inferred at %s (robot %s, deck empty)",
		binID, node.Name, robotID)
}

// strandedAnomaly is branch C: leave the bin at _TRANSIT, stamp it, and record
// where the robot last was so the operator gets a map pin instead of a search.
func (e *Engine) strandedAnomaly(binID int64, robotID string, robot fleet.RobotStatus, haveRobot bool, why string) {
	note := strandedNote(robotID, robot, haveRobot, why)
	if err := e.db.MarkBinAnomalyWithNote(binID, note); err != nil {
		e.logFn("engine: stranded transit: mark bin %d anomalous: %v", binID, err)
		return
	}
	e.logFn("engine: stranded transit: bin %d left at _TRANSIT — %s", binID, note)
}

// strandedNote renders the robot's last known position as one line an operator
// can read and walk to.
//
// Coordinates lead because they are the map pin; the station names follow
// because either may be the answer and neither is reliably present. Ordered by
// CORE POLL TIME implicitly — this is the cache's latest sample, and the robot's
// own clock is never consulted, because robot clocks at Springfield are off by
// days (FINDING-seer-jackunload-vs-block-completion-2026-08-12.md).
func strandedNote(robotID string, robot fleet.RobotStatus, haveRobot bool, why string) string {
	parts := []string{why}
	if robotID != "" {
		parts = append(parts, "robot "+robotID)
	}
	if haveRobot {
		parts = append(parts, fmt.Sprintf("at x=%.2f y=%.2f angle=%.2f", robot.X, robot.Y, robot.Angle))
		if robot.CurrentStation != "" {
			parts = append(parts, "station "+robot.CurrentStation)
		}
		if robot.LastStation != "" && robot.LastStation != robot.CurrentStation {
			parts = append(parts, "last station "+robot.LastStation)
		}
		parts = append(parts, fmt.Sprintf("jack_state=%d height=%.4f", robot.JackState, robot.LiftHeight))
	}
	return strings.Join(parts, "; ")
}
