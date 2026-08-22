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
	"shingocore/store/nodes"
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
		// Branch B: the bin is still on the deck, so it is not lost and it is
		// not at a station. It rides the robot until the deck reports empty.
		e.parkOnCarrier(binID, robotID, robot)
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

// carrierNodeName is the per-robot synthetic node a bin lives on while it is
// still on that robot's deck.
//
// Synthetic, so every bin-finding query in Core already excludes it — the
// finders filter `claimed_by IS NULL AND is_synthetic = false`, so a bin here
// cannot be re-claimed or sourced from without touching a single predicate
// (docs/bin-transit-state.md). It is never offered as a destination for the
// same reason.
//
// It is deliberately NOT _TRANSIT. _TRANSIT plus no claim IS the anomaly
// definition, and a bin whose location we know perfectly well — it is on that
// robot — is not an anomaly. Putting it there would be reporting a lost bin
// that is not lost.
func carrierNodeName(robotID string) string { return "_ROBOT:" + robotID }

// parkOnCarrier is branch B: move the bin onto the robot's own node.
//
// Created lazily on first use. A plant's robots come and go, and a table of
// carrier nodes maintained ahead of need would be one more thing to keep in
// step with the fleet.
func (e *Engine) parkOnCarrier(binID int64, robotID string, robot fleet.RobotStatus) {
	if robotID == "" {
		e.strandedAnomaly(binID, robotID, robot, true, "bin on a deck, but the order names no robot")
		return
	}
	node, err := e.carrierNode(robotID)
	if err != nil {
		e.logFn("engine: stranded transit: carrier node for %s: %v", robotID, err)
		e.strandedAnomaly(binID, robotID, robot, true, "bin still on the robot's deck")
		return
	}
	// Through BinService.Move, not the store: the service owns the occupancy
	// fail-close and the orphaned-order guard, and the forbidigo rule exists
	// because bypassing it is exactly the mistake this code would make. A
	// synthetic destination skips the occupancy check by design — many bins may
	// not ride one deck, but a carrier node has no slot to contend for — and
	// the orphan guard passes because the order is already terminal.
	bin, err := e.BinService().GetBin(binID)
	if err != nil || bin == nil {
		e.logFn("engine: stranded transit: read bin %d: %v", binID, err)
		return
	}
	if _, err := e.BinService().Move(bin, node.ID); err != nil {
		e.logFn("engine: stranded transit: park bin %d on %s: %v", binID, node.Name, err)
		e.strandedAnomaly(binID, robotID, robot, true, "bin still on the robot's deck")
		return
	}
	if err := e.db.RecordRecoveryAction("transit_bin_on_robot", "bin", binID,
		"inferred: still on "+robotID+"'s deck", inferredActor); err != nil {
		e.logFn("engine: stranded transit: audit carrier park for bin %d: %v", binID, err)
	}
	e.logFn("engine: stranded transit: bin %d rides %s (deck loaded)", binID, robotID)
}

// carrierNode returns the robot's carrier node, creating it on first use.
func (e *Engine) carrierNode(robotID string) (*nodes.Node, error) {
	name := carrierNodeName(robotID)
	if node, err := e.db.GetNodeByName(name); err == nil && node != nil {
		return node, nil
	}
	node := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true}
	if err := e.db.CreateNode(node); err != nil {
		// A concurrent sweep may have created it between the read and the
		// write; re-read rather than failing the placement.
		if existing, rerr := e.db.GetNodeByName(name); rerr == nil && existing != nil {
			return existing, nil
		}
		return nil, err
	}
	return node, nil
}

// sweepCarriedBins is the watch half of branch B: a bin riding a deck is placed
// as soon as that deck reports empty.
//
// Driven by the robot poll Core already makes every 2 seconds, so it adds no
// RDS traffic. There is no jack-unload EVENT to subscribe to — the jack is
// sampled state — which is why this is a watch and not a notification.
func (e *Engine) sweepCarriedBins() {
	carried, err := e.db.ListBinsOnCarrierNodes()
	if err != nil {
		e.logFn("engine: carried bins: %v", err)
		return
	}
	for _, bin := range carried {
		robotID := strings.TrimPrefix(bin.NodeName, "_ROBOT:")
		if robotID == "" || robotID == bin.NodeName {
			continue
		}
		robot, ok := e.GetCachedRobotStatus(robotID)
		if !ok {
			continue
		}
		carrying, certain := service.RobotCarryingBin(robot)
		if !certain || carrying {
			// Still loaded, or the deck is mid-travel. Leave it riding; the
			// next poll asks again.
			continue
		}
		// The deck is empty. Apply branch A at this tick's station.
		node, resolved := service.ResolveRobotStation(e.NodeService(), robot)
		if !resolved {
			e.strandedAnomaly(bin.ID, robotID, robot, true,
				"deck emptied somewhere we cannot name")
			continue
		}
		if err := e.BinService().RecoverTransitAnomaly(bin.ID, node.ID, inferredActor); err != nil {
			e.strandedAnomaly(bin.ID, robotID, robot, true,
				fmt.Sprintf("deck emptied but could not place at %s: %v", node.Name, err))
			continue
		}
		e.logFn("engine: carried bin %d placed at %s after %s unloaded", bin.ID, node.Name, robotID)
	}
}

// sweepStrandedBins is the reconciliation half: ask the world, not an event.
//
// Two populations. Every bin at _TRANSIT with no claim — the anomaly by
// definition — gets the A/B/C decision re-run against its last claiming order's
// robot. Every bin already riding a deck gets its jack re-checked, which is the
// same work the 2-second poll does and is repeated here so a Core that restarted
// between the unload and the poll still places the bin.
//
// IT ALSO CLEARS THE BACKLOG. Every bin stranded before this shipped is in the
// first population, so the first sweep after deploy is the one the floor will
// notice: bins that have been "lost" for days get placed or get a map pin.
func (e *Engine) sweepStrandedBins() {
	e.sweepCarriedBins()

	stranded, err := e.BinService().ListAnomalies()
	if err != nil {
		e.logFn("engine: stranded sweep: list anomalies: %v", err)
		return
	}
	for _, bin := range stranded {
		if bin.NodeName != transitNodeName {
			// ListAnomalies is "unclaimed at a synthetic node", which now also
			// matches the carrier nodes. Those are handled above and are not
			// anomalies.
			continue
		}
		orderID, robotID, ok := e.lastClaimingRobot(bin.ID)
		if !ok {
			// No order ever claimed it, or the order is gone. Stamp it so the
			// operator at least sees it, with the honest reason.
			e.strandedAnomaly(bin.ID, "", fleet.RobotStatus{}, false, "no claiming order on record")
			continue
		}
		robot, haveRobot := e.GetCachedRobotStatus(robotID)
		e.dbg("engine: stranded sweep: bin %d from order %d robot %q", bin.ID, orderID, robotID)
		e.placeStrandedBin(bin.ID, robotID, robot, haveRobot)
	}
}

// lastClaimingRobot finds the order that most recently owned this bin, and the
// robot it was on.
//
// orders.bin_id survives terminalisation (bins.claimed_by and the order_bins
// junction do not), so the order's own column is the only durable link back from
// a stranded bin to the robot that was carrying it.
func (e *Engine) lastClaimingRobot(binID int64) (orderID int64, robotID string, ok bool) {
	ords, err := e.db.ListOrdersByBin(binID, 1)
	if err != nil || len(ords) == 0 {
		return 0, "", false
	}
	return ords[0].ID, ords[0].RobotID, true
}

// strandedAnomaly is branch C: leave the bin at _TRANSIT, stamp it, and record
// where the robot last was so the operator gets a map pin instead of a search.
func (e *Engine) strandedAnomaly(binID int64, robotID string, robot fleet.RobotStatus, haveRobot bool, why string) {
	note := strandedNote(robotID, robot, haveRobot, why)
	if err := e.BinService().MarkAnomalyWithPosition(binID, note); err != nil {
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
