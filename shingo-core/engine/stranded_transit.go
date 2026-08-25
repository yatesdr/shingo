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
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/fleet"
	"shingocore/service"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// inferredActor is the actor on the recovery_actions row and the bin audit. It
// is deliberately distinguishable from an operator: a placement nobody walked
// out and confirmed should be readable as such afterwards.
//
// Defined in service, where it is also ENFORCED — it is the only actor allowed
// to move a bin off a carrier node. One spelling, on the side that checks it.
const inferredActor = service.InferredActor

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

	// Branch A. The deck is empty; if the robot is PARKED somewhere we can
	// name, that is where it put the bin.
	if robot.Busy {
		// A WEAK GUARD, KEPT — and its old comment claimed more than the field
		// can deliver. `Busy` is the vendor's `procBusiness`
		// (fleet/seerrds/mappers.go), a TASK flag and not a motion flag: on
		// 2026-08-24 it was false for the whole of AMR-09's 2 m 18 s drive-off,
		// because the order carrying the bin had already been cancelled. So it
		// does not detect a parked robot and never did. It is kept because it
		// costs nothing and does exclude a robot the fleet is actively driving
		// under an order; on the sweep path the frozen drop sample is the real
		// guard, and this path has no freeze to fall back on.
		e.strandedAnomaly(binID, robotID, robot, true, "robot is under way, not parked")
		return
	}
	// PICKUP AGE, and this is branch A's MISSING PREMISE rather than a patch.
	//
	// Branch A never watched the deck empty. It reads one at-rest, empty-deck
	// sample and concludes the robot set the bin down where it is standing —
	// which is only sound while the pickup is recent enough that the robot has
	// not done other work since. Bin 37's cancel came 20 h 54 m after its
	// pickup, and the robot's position by then described a charging bay.
	//
	// MEASURED FROM THE `in_transit` ROW, not the terminal one, because
	// terminalWithin cannot bound this: on the event path the terminal row is
	// milliseconds old by construction, so that window always passes. The two
	// clauses bound different intervals and both are kept — see pickupWithin.
	//
	// Branch B and the carried-bin watch stay exempt: the jack is the jack.
	ord, _, haveOrder := e.lastClaimingOrder(binID)
	if !haveOrder || !e.pickupWithin(ord, e.strandedSweepWindow()) {
		// NO POSITION ON THIS NOTE, and the reason is the note's own sentence:
		// the robot's coordinates do not describe this bin any more, so
		// printing them would be handing an operator a pin to walk to while
		// telling them the pin means nothing. It also keeps the note CONSTANT,
		// which is what lets the log dedup suppress a decline the sweep repeats
		// every two seconds for as long as the terminal window lasts.
		e.strandedAnomaly(binID, robotID, fleet.RobotStatus{}, false,
			"the bin was picked up longer ago than "+e.strandedSweepWindow().String()+
				", so where this robot is standing now says nothing about where it left the bin")
		return
	}
	e.placeInferred(binID, robotID, observeDrop(robotID, robot, clock.Now().UTC()), false)
}

// pickupWithin reports whether the bin left its source recently enough for the
// robot's current position to still describe where it went.
//
// The `in_transit` history row is the pickup: it is written when the fleet
// first reports the order under way, and it is the only durable record of when
// the bin left the floor. FAIL CLOSED for the same reason terminalWithin does —
// an unreadable row, a missing one, or an order that never reached in_transit
// all report false, because this gates a write that moves a bin.
func (e *Engine) pickupWithin(ord *orders.Order, window time.Duration) bool {
	if ord == nil {
		return false
	}
	h, err := e.db.LatestOrderHistoryForStatus(ord.ID, protocol.StatusInTransit)
	if err != nil {
		e.logFn("engine: stranded transit: pickup row for order %d: %v", ord.ID, err)
		return false
	}
	if h == nil {
		return false
	}
	return clock.Now().UTC().Sub(h.CreatedAt) <= window
}

// placeInferred is THE placement gate, shared by both inference paths.
//
// One helper and not two call sites, because the two used to duplicate the
// resolve-then-RecoverTransitAnomaly shape and a rule added to one would have
// been forgotten by the other — which is exactly the failure mode the bin-type
// check below would have had.
//
// The order of the gate is the order of what can be known:
//
//  1. THE DECK IS AT REST AND EMPTY — the caller's precondition. The sweep has
//     a frozen sample of the tick it became true; the event path has one live
//     reading and no witness, which is why only it carries the pickup-age gate.
//  2. RESOLVE the reported point to a node (identity, then the scene alias).
//     A miss or an ambiguity declines, naming what the point actually is.
//  3. THE NODE'S BIN-TYPE CONFIG must admit this bin.
//  4. RecoverTransitAnomaly's own guards — occupied, synthetic, _TRANSIT —
//     stay the final authority. Nothing here bypasses them.
//
// INTENT IS RECORDED ON EVERY OUTCOME AND GATES NONE OF THEM. The order's
// delivery node goes into the placement log line, the decline note and the
// audit row, because "where was it supposed to go" is the first question asked
// afterwards. It is never a veto and never a substitute: an operator usually
// cancels BECAUSE the plan was wrong, so observation and intent disagree
// precisely when the observation is the only true answer. Bin 37's drop was
// 24.9 m from its order's destination.
//
// watchedUnload says whether this path saw the loaded→empty transition. It
// changes only the words — a path that did not watch must not say the deck
// emptied here.
func (e *Engine) placeInferred(binID int64, robotID string, obs dropObservation, watchedUnload bool) bool {
	bin, err := e.BinService().GetBin(binID)
	if err != nil || bin == nil {
		e.logFn("engine: stranded transit: read bin %d for placement: %v", binID, err)
		return false
	}
	intent := e.placementIntent(binID)

	node, point, resolved := service.ResolveReportedPoints(e.NodeService(), obs.CurrentStation, obs.LastStation)
	if !resolved {
		lead := "robot is not at a node we know"
		if watchedUnload {
			lead = "deck emptied somewhere we cannot name"
		}
		e.declineInferred(binID, robotID, obs, intent, lead+": "+
			service.DescribeUnresolvedPoints(e.NodeService(), obs.CurrentStation, obs.LastStation),
			watchedUnload)
		return false
	}
	if why := e.binTypeRefusal(node, bin); why != "" {
		e.declineInferred(binID, robotID, obs, intent, why, watchedUnload)
		return false
	}

	evidence := fmt.Sprintf("inferred from %s at %s, reported by %s; %s",
		point, obs.At.Format(time.RFC3339), robotID, intentPhrase(intent))
	if err := e.BinService().RecoverTransitAnomaly(binID, node.ID, inferredActor, evidence); err != nil {
		// The commonest refusal is an occupied node: something else is in that
		// slot, so the placement cannot be made right now. Fall to C rather than
		// forcing it — the empty-node guard is the reason this is safe to run
		// unattended at all. The frozen sample SURVIVES this: the next tick
		// retries the same observation against a slot that may since have freed.
		e.declineInferred(binID, robotID, obs, intent,
			fmt.Sprintf("could not place at %s: %v", node.Name, err), watchedUnload)
		return false
	}
	// FORGET THE NOTE ON EVERY PLACEMENT, not just the sweep's. The map that
	// suppresses a repeated log line is also a SILENCER when it outlives the
	// episode it describes: a bin placed by the fast path and later stranded
	// again in the same way would match its own stale note and say nothing at
	// all. Only the sweep used to forget, so only the sweep re-armed it.
	e.forgetStrandedNote(binID)
	e.logFn("engine: stranded transit: bin %d placed at %s (point %s; robot %s; deck read empty %s; %s)",
		binID, node.Name, point, robotID, obs.At.Format(time.RFC3339), intentPhrase(intent))
	return true
}

// declineInferred is branch C from the gate: the honest sentence, with
// everything the gate knew when it declined.
//
// The frozen coordinates come from the sample rather than from the robot, which
// is the whole of the pin-drift fix: the note now describes the moment the deck
// emptied and stops moving as the robot drives on.
//
// THE DROP INSTANT IS PRINTED ONLY WHEN THERE WAS A DROP TO INSTANT. On the
// sweep path the sample is frozen, so "deck read empty 21:02:23Z" is both true
// and the same bytes next pass. On the `_TRANSIT` path there is no freeze —
// the reading is taken fresh on every pass — so the same field would carry
// clock.Now(), and a note that changes every pass is a note neither dedup can
// suppress: not the log's (keyed on the text) and not MarkAnomalyWithNote's
// (`anomaly_note IS DISTINCT FROM`). The one field guaranteed to differ would
// have defeated the write-dedup that ships in this same change.
func (e *Engine) declineInferred(binID int64, robotID string, obs dropObservation, intent, why string, watchedUnload bool) {
	parts := []string{why, intentPhrase(intent)}
	if watchedUnload {
		parts = append(parts, "deck read empty "+obs.At.Format(time.RFC3339))
	}
	e.strandedAnomaly(binID, robotID, obs.status(), true, strings.Join(parts, "; "))
}

// placementIntent is where the last order that owned this bin was taking it.
//
// `orders.delivery_node`, and NOT `blocks_json`: neither call site holds the
// vendor block snapshot, and the stopped block names the same node this column
// does. It would be a second query for a corroborator already in hand.
func (e *Engine) placementIntent(binID int64) string {
	ord, _, ok := e.lastClaimingOrder(binID)
	if !ok || ord == nil {
		return ""
	}
	return ord.DeliveryNode
}

func intentPhrase(intent string) string {
	if intent == "" {
		return "no order on record names where it was going"
	}
	return "its order was taking it to " + intent
}

// binTypeRefusal reports why a node will not accept this bin, or "" when it
// will. THE MODE IS READ, NOT THE LIST, and that distinction is the whole rule.
//
// GetEffectiveBinTypes collapses three different meanings into an empty result:
// `all` returns nil, `inherit`/absent returns empty when no ancestor assigns
// anything, and `specific` with no assignments returns empty too. Only the
// third means "accepts nothing". A gate written as "empty means unrestricted"
// is fail-OPEN on that misconfiguration; a gate written as "empty means refuse"
// is fail-CLOSED on every node at Springfield, where node_bin_types is empty
// and 41 of 52 physical nodes carry no mode row at all.
//
// ON THE INFERENCE PATH ONLY — deliberately not inside RecoverTransitAnomaly,
// even though that would be one place instead of two. That method is also the
// operator's "I found it, it's at X" door, and refusing to record a physical
// fact a human observed because a config row says the node should not hold that
// type would be wrong. The inference is guessing and should be conservative;
// the operator is asserting and should be believed.
//
// Nothing in dispatch changes: GetEffectiveBinTypes and binTypeAllowed keep
// their current readings.
func (e *Engine) binTypeRefusal(node *nodes.Node, bin *bins.Bin) string {
	mode := e.NodeService().GetNodeProperty(node.ID, "bin_type_mode")
	if mode == "all" {
		return ""
	}
	types, err := e.NodeService().GetEffectiveBinTypes(node.ID)
	if err != nil {
		// FAIL CLOSED. This gate exists to stop a bin being recorded somewhere
		// it cannot be, and a list that could not be read is not an empty list.
		return fmt.Sprintf("%s's bin-type list could not be read (%v)", node.Name, err)
	}
	if len(types) == 0 {
		if mode == "specific" {
			return fmt.Sprintf("%s is set to accept specific bin types and has none assigned, "+
				"so it accepts nothing — that is a configuration gap, not a fact about the bin",
				node.Name)
		}
		return ""
	}
	codes := make([]string, 0, len(types))
	for _, t := range types {
		if t.ID == bin.BinTypeID {
			return ""
		}
		codes = append(codes, t.Code)
	}
	return fmt.Sprintf("%s does not accept bin type %s (it accepts %s)",
		node.Name, bin.BinTypeCode, strings.Join(codes, ", "))
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
func carrierNodeName(robotID string) string { return bins.CarrierNodePrefix + robotID }

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
	// THE WITNESS STARTS HERE. This process has just read the deck LOADED, so
	// the next empty reading is a transition it watched — which is the whole
	// difference between an observation and a state Core woke up to.
	e.markDeckLoaded(binID)
	e.logFn("engine: stranded transit: bin %d rides %s (deck loaded)", binID, robotID)
}

// carrierNode returns the robot's carrier node, creating it on first use.
func (e *Engine) carrierNode(robotID string) (*nodes.Node, error) {
	name := carrierNodeName(robotID)
	if node, err := e.db.GetNodeByName(name); err == nil && node != nil {
		return node, nil
	}
	// NO node_type_id, deliberately, and matching `_TRANSIT` is the reason: v15
	// creates that node as `(name, is_synthetic, enabled)` and nothing else, so
	// the two bookkeeping nodes have the same shape. Inventing a `carrier` node
	// type would give the carriers a classification `_TRANSIT` does not have,
	// for a row nothing groups or filters by type — and every predicate that
	// matters keys on is_synthetic, which is set.
	//
	// The row is temporary besides: DeleteCarrierNodeIfEmpty retires it as soon
	// as the deck is clear, so a carrier node exists only while a bin is on it.
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
	e.retireEmptyCarrierNodes()

	carried, err := e.db.ListBinsOnCarrierNodes()
	if err != nil {
		e.logFn("engine: carried bins: %v", err)
		return
	}
	for _, bin := range carried {
		robotID := strings.TrimPrefix(bin.NodeName, bins.CarrierNodePrefix)
		if robotID == "" || robotID == bin.NodeName {
			continue
		}
		robot, ok := e.GetCachedRobotStatus(robotID)
		if !ok {
			continue
		}
		e.placeCarriedBinIfSettled(bin, robotID, robot)
	}
	e.pruneDropObservations(carried)
}

// retireEmptyCarrierNodes removes carrier nodes nothing is riding any more,
// BEFORE the walk so a node emptied by a recovery order does not linger a whole
// tick.
//
// By shape, not by name. The per-name retire at the end of the walk only covers
// bins this watch placed itself; a recovery order's unload ends in the ordinary
// arrival handling, which knows nothing about carrier nodes, and would
// otherwise leave permanent furniture on a table operators read.
//
// THE BULK DELETE IS ONE STATEMENT, AND ONE PINNED ROW DEFEATS ALL OF IT.
// Neither `corrections.node_id` nor `cms_transactions.node_id` declares an
// ON DELETE, so a single carrier node referenced by one of those rows fails the
// whole DELETE — and the error used to be debug-level, so five stale carrier
// nodes could sit on the node list forever while the log said nothing. The
// per-node fallback retires the ones that CAN go, and the error is now visible.
func (e *Engine) retireEmptyCarrierNodes() {
	n, err := e.db.RetireEmptyCarrierNodes(bins.CarrierNodePrefix)
	if err == nil {
		if n > 0 {
			e.dbg("engine: carried bins: retired %d empty carrier node(s)", n)
		}
		return
	}
	e.logFn("engine: carried bins: bulk retire of empty carrier nodes failed (%v) — "+
		"retrying one at a time, because one row pinned by a foreign key must not "+
		"leave every other carrier node on the list", err)
	all, lerr := e.db.ListNodes()
	if lerr != nil {
		e.logFn("engine: carried bins: list nodes for per-node retire: %v", lerr)
		return
	}
	for _, node := range all {
		if !node.IsSynthetic || !strings.HasPrefix(node.Name, bins.CarrierNodePrefix) {
			continue
		}
		// The query declines while any bin is still on it, so this is safe to
		// call against every carrier node including the occupied ones.
		if derr := e.db.DeleteCarrierNodeIfEmpty(node.Name); derr != nil {
			e.logFn("engine: carried bins: retire %s: %v", node.Name, derr)
		}
	}
}

// placeCarriedBinIfSettled is the per-bin body of the watch: place the bin as
// soon as the deck it is riding reports empty at rest, at the station it
// reported WHEN IT EMPTIED.
func (e *Engine) placeCarriedBinIfSettled(bin *bins.Bin, robotID string, robot fleet.RobotStatus) {
	carrying, certain := service.RobotCarryingBin(robot)
	if !certain || carrying {
		// Still loaded, or the deck is mid-travel. Leave it riding; the next
		// poll asks again — and record that this process has now SEEN the deck
		// loaded, which is what makes the eventual empty reading an observation
		// of a transition rather than a state Core woke up to.
		e.markDeckLoaded(bin.ID)
		return
	}

	// FROZEN FIRST, BEFORE EVERY GATE THAT CAN FAIL. The reading is what decays;
	// the gates are what may need another tick. A freeze taken after the
	// stand-down check or the Busy check would be a freeze the robot had already
	// driven away from.
	window := e.strandedSweepWindow()
	obs, verdict := e.freezeDrop(bin.ID, observeDrop(robotID, robot, clock.Now().UTC()), window)
	switch verdict {
	case dropUnwitnessed:
		// See freezeDrop: an already-empty deck this process never saw loaded is
		// a Core that restarted after the unload, and it is not evidence of
		// anything. The honest answer is the anomaly and the operator button.
		//
		// NO POSITION, for the same reason the sentence gives: the reading does
		// not describe this bin. It also keeps the note constant, so a decline
		// the sweep repeats every two seconds forever prints one line and not
		// 43,200 a day.
		e.strandedAnomaly(bin.ID, robotID, fleet.RobotStatus{}, false,
			"the deck was already empty when Core first looked — Core restarted after the "+
				"unload, so the drop was not observed and this robot's position does not "+
				"describe where the bin is")
		return
	case dropGapped:
		// A DIFFERENT SENTENCE, because it is a different fact. Core did not
		// restart: it was running and heard nothing about this robot for longer
		// than a deck reading stays worth anything — the fleet went unreachable,
		// or this AMR dropped off the network. The unload happened somewhere
		// inside that silence, and so could a person lifting the bin off by
		// hand. Telling an operator Core restarted when it did not would send
		// them to check the wrong thing.
		//
		// Positionless for the same reason as the restart case, and constant for
		// the same reason.
		e.strandedAnomaly(bin.ID, robotID, fleet.RobotStatus{}, false,
			"the deck last read loaded more than "+deckWitnessRecency.String()+
				" before it read empty — Core heard nothing about this robot in between, so "+
				"the drop was not observed and this robot's position does not describe "+
				"where the bin is")
		return
	case dropExpired:
		// The drop WAS watched, and the sentence says so — this is the only one
		// of the three that keeps its coordinates, because they are a true
		// record of where the deck emptied. What has run out is the right to act
		// on them: every retry since has failed, and hours later an operator may
		// have moved the bin themselves. The sample is kept rather than dropped
		// so that this stays the answer instead of a fresh reading becoming one.
		e.declineInferred(bin.ID, robotID, obs, e.placementIntent(bin.ID),
			"the drop was observed more than "+window.String()+" ago and could not be placed "+
				"in that time, so it is no longer safe to record it from that reading", true)
		return
	}

	// ── THE CARRIER-NODE GUARD, AND WHY IT IS HERE AND NOT ON THE MOVE ──
	//
	// A recovery order (carried_bin_recovery.go) asks this robot to unload at a
	// chosen destination. While that order is running the deck will report empty
	// the instant the bin is set down — and this watch would then place the bin
	// at whatever station resolves for the tick, racing the order's own arrival
	// handling. Two placements, and the second one wins by accident.
	//
	// The order is the better answer whenever there is one: it has a destination
	// somebody chose, a slot reservation behind it, and the ordinary arrival
	// path to record where the bin actually landed. So this watch stands down
	// for the duration.
	//
	// Deliberately NOT done by widening BinService's actor guard on
	// RecoverTransitAnomaly. That guard stops a HUMAN asserting a location for a
	// bin that is riding a robot, and the jack watch is its sanctioned
	// exception; adding a third actor would re-open the same question a fourth
	// time. What is needed here is not "who may move this bin" but "is something
	// already moving it", and that is a liveness question, answered where the
	// race is.
	//
	// A FAILED READ STANDS DOWN TOO. The error used to be discarded, so a list
	// that could not be read meant "no live order" and this watch went on to
	// place the bin — racing the arrival of the very order it was written to
	// yield to. The two answers do not cost the same: standing down wrongly
	// costs one tick, and the next poll asks again, while proceeding wrongly is
	// the double placement.
	live, err := e.liveRecoveryOrderForBin(bin.ID)
	if err != nil {
		e.logFn("engine: carried bins: bin %d — cannot tell whether a recovery order is live (%v); "+
			"standing down this tick rather than racing one", bin.ID, err)
		return
	}
	if live != nil {
		e.dbg("engine: carried bins: bin %d left to recovery order %d (%s)",
			bin.ID, live.ID, live.Status)
		return
	}
	if robot.Busy {
		// Kept, and weak — see the same check in placeStrandedBin. `Busy` is the
		// vendor's task flag, not a motion flag, and it was false throughout the
		// drive-off that produced this whole fix. On THIS path the freeze above
		// is the real guard: the answer was taken before the robot moved, so a
		// robot that is driving now costs a tick, not a wrong station.
		return
	}
	if !e.placeInferred(bin.ID, robotID, obs, true) {
		return
	}
	e.forgetDrop(bin.ID)
	// The bin has left the deck; if it was the last one, the carrier node has
	// served its purpose. Removed here rather than left to accumulate — a
	// lazily-created node per vehicle would otherwise be permanent furniture on
	// a table operators read. The query declines while any bin is still on it,
	// so a robot carrying two is unaffected.
	if err := e.db.DeleteCarrierNodeIfEmpty(bin.NodeName); err != nil {
		e.dbg("engine: carried bins: retire %s: %v", bin.NodeName, err)
	}
}

// sweepStrandedBins is the reconciliation half: ask the world, not an event.
//
// Two populations. Every bin already riding a deck gets its jack re-checked,
// which is the same work the 2-second poll does. And every bin at _TRANSIT with
// no claim whose order ended RECENTLY gets the A/B/C decision re-run against
// that order's robot.
//
// THE RESTART PROMISE IS DELIBERATELY NARROWED, and this comment used to make
// it: "repeated here so a Core that restarted between the unload and the poll
// still places the bin". It no longer does, and must not. A bin found already
// on a carrier node with an already-empty deck was unloaded at some unknown
// point while Core was down, and during that window an operator may have taken
// it off the deck by hand — so the robot's position describes the robot, not
// the bin. What the sweep still recovers across a restart is the case where the
// deck is STILL LOADED: it re-reads the jack, records the witness, and places
// the bin when that deck later empties under this process's eyes. See
// freezeDrop for the rule and for why agreement-with-intent is not a safe
// substitute for having watched.
//
// IT DOES NOT BACKFILL HISTORY, and that is the design. An earlier cut of this
// swept every unclaimed _TRANSIT bin regardless of age, which read as a feature
// ("the first sweep clears the backlog") and was a defect: the inference is
// computed from the robot's CURRENT telemetry, so it only answers "where did
// this bin go" while the robot has not moved on. For a bin stranded days ago the
// robot has run hundreds of jobs since and its position is unrelated to the bin;
// an empty node at that position would take the placement and invent a bin the
// floor would then be dispatched to fetch. Older bins stay anomalies for an
// operator to resolve, exactly as before any of this shipped. Declining costs
// nothing; guessing costs a phantom.
//
// The window does not apply to carrier bins. The jack is the jack: a deck that
// reports empty has set its bin down, however long the bin has been riding.
func (e *Engine) sweepStrandedBins() {
	e.sweepCarriedBins()

	stranded, err := e.BinService().ListAnomalies()
	if err != nil {
		e.logFn("engine: stranded sweep: list anomalies: %v", err)
		return
	}
	window := e.strandedSweepWindow()
	declined := 0
	for _, bin := range stranded {
		if bin.NodeName != transitNodeName {
			// ListAnomalies is "unclaimed at _TRANSIT" since the carrier-node
			// fix, so this should no longer match. Kept as a cheap assertion:
			// the carrier bins are handled above and are not anomalies.
			continue
		}
		ord, robotID, ok := e.lastClaimingOrder(bin.ID)
		if !ok {
			// No order ever claimed it, or the order is gone. Stamp it so the
			// operator at least sees it, with the honest reason.
			e.strandedAnomaly(bin.ID, "", fleet.RobotStatus{}, false, "no claiming order on record")
			continue
		}
		if endedAt, fresh := e.terminalWithin(ord, window); !fresh {
			// Too old to infer from, or not terminal, or unreadable. Left as
			// the anomaly it already is — no stamp rewrite, because a later
			// sweep has nothing new to say about a bin it has declined.
			declined++
			e.dbg("engine: stranded sweep: bin %d declined — order %d ended %v (window %s)",
				bin.ID, ord.ID, endedAt, window)
			continue
		}
		robot, haveRobot := e.GetCachedRobotStatus(robotID)
		e.dbg("engine: stranded sweep: bin %d from order %d robot %q", bin.ID, ord.ID, robotID)
		e.placeStrandedBin(bin.ID, robotID, robot, haveRobot)
	}
	if declined > 0 {
		e.logFn("engine: stranded sweep: %d bin(s) left as anomalies — older than %s, "+
			"robot telemetry no longer describes where they went", declined, window)
	}
}

// strandedSweepWindow is the config age limit, with the shipped default when
// config is absent (tests construct an Engine without one).
func (e *Engine) strandedSweepWindow() time.Duration {
	if e.cfg == nil || e.cfg.RDS.StrandedSweepWindow <= 0 {
		return defaultStrandedSweepWindow
	}
	return e.cfg.RDS.StrandedSweepWindow
}

// defaultStrandedSweepWindow mirrors config's shipped value for the nil-config
// path. The config comment carries the reasoning.
const defaultStrandedSweepWindow = 2 * time.Hour

// terminalWithin reports whether the order ended recently enough for its
// robot's current position to still mean something, and when it ended.
//
// The end time is the TERMINAL HISTORY ROW, not orders.updated_at. updated_at is
// rewritten by UpdateOrderVendor, which runs after every vendor status change
// including ones the lifecycle rejected on an already-terminal order — so a
// stale order can carry a fresh updated_at, which is the wrong direction to be
// wrong in here. The history row is when the order actually ended.
//
// FAIL CLOSED. An unreadable row, a missing one, or a non-terminal order all
// report false: this gates a write that moves a bin, and declining leaves an
// anomaly an operator resolves while guessing puts a phantom bin on the floor.
func (e *Engine) terminalWithin(ord *orders.Order, window time.Duration) (time.Time, bool) {
	if ord == nil || !protocol.IsTerminal(ord.Status) {
		return time.Time{}, false
	}
	h, err := e.db.LatestOrderHistoryForStatus(ord.ID, ord.Status)
	if err != nil {
		e.logFn("engine: stranded sweep: terminal row for order %d: %v", ord.ID, err)
		return time.Time{}, false
	}
	if h == nil {
		return time.Time{}, false
	}
	return h.CreatedAt, clock.Now().UTC().Sub(h.CreatedAt) <= window
}

// lastClaimingOrder finds the order that most recently owned this bin, and the
// robot it was on.
//
// orders.bin_id survives terminalisation (bins.claimed_by and the order_bins
// junction do not), so the order's own column is the only durable link back from
// a stranded bin to the robot that was carrying it.
func (e *Engine) lastClaimingOrder(binID int64) (ord *orders.Order, robotID string, ok bool) {
	ords, err := e.db.ListOrdersByBin(binID, 1)
	if err != nil || len(ords) == 0 {
		return nil, "", false
	}
	return ords[0], ords[0].RobotID, true
}

// strandedAnomaly is branch C: leave the bin at _TRANSIT, stamp it, and record
// where the robot last was so the operator gets a map pin instead of a search.
func (e *Engine) strandedAnomaly(binID int64, robotID string, robot fleet.RobotStatus, haveRobot bool, why string) {
	note := strandedNote(robotID, robot, haveRobot, why)
	if err := e.BinService().MarkAnomalyWithPosition(binID, note); err != nil {
		e.logFn("engine: stranded transit: mark bin %d anomalous: %v", binID, err)
		return
	}
	// THE WRITE REPEATS; THE LOG LINE MUST NOT — AND NOW THE DEDUP IS PERFECT
	// BY CONSTRUCTION.
	//
	// The sweep runs every two seconds and re-marks every stranded bin on every
	// tick. That used to produce a note that MOVED, because it carried the
	// robot's latest position: bin 5's note drifted from AP102 to a park point
	// 12.3 m away over 69 ticks, and the dedup below could not suppress a line
	// that genuinely changed. Now the carried-bin note renders from the FROZEN
	// drop sample, so the same episode produces the same bytes every tick and
	// this map suppresses every repeat rather than most of them.
	//
	// The map is still keyed on the NOTE and not on the bin, because that is
	// what makes a bin whose situation genuinely changes — a different reason, a
	// different station on the event path — say so. What is suppressed is the
	// identical line, repeated. The WRITE is deduped one layer down
	// (bins.MarkAnomalyWithNote), so an unchanged note no longer churns
	// bins.updated_at every two seconds either.
	e.strandedNotesMu.Lock()
	if e.strandedNotes == nil {
		e.strandedNotes = map[int64]string{}
	}
	unchanged := e.strandedNotes[binID] == note
	e.strandedNotes[binID] = note
	e.strandedNotesMu.Unlock()
	if unchanged {
		return
	}
	e.logFn("engine: stranded transit: bin %d left at _TRANSIT — %s", binID, note)
}

// ForgetStrandedNote re-arms the log for a bin an OPERATOR has just resolved.
//
// For the two doors in www — "I found it, it's at X" (apiClearTransitAnomaly)
// and the manual bin move. Both clear the anomaly in the database and neither
// could reach this map, so a bin recovered by hand and later stranded again in
// exactly the same way was re-flagged correctly and announced not at all:
// strandedAnomaly saw its own stale entry and suppressed the line. For a bin
// that strands the same way twice, "until the note changes" is never.
//
// THE HANDLER CALLS IT, NOT BinService. RecoverTransitAnomaly is the shared
// door — the inference goes through it too — and it holds no Engine and should
// not grow one for a log silencer.
//
// ON RecoveryService, WHICH THE HANDLERS ALREADY REACH, so the narrow
// ServiceAccess interface does not have to widen to carry it. That width is a
// tripwire (www/engine_iface_width_test.go) and this is not the change that
// should trip it: re-arming a log after an operator recovery is the same kind
// of thing as the other verbs on this service, and it belongs beside them.
func (s *RecoveryService) ForgetStrandedNote(binID int64) { s.engine.forgetStrandedNote(binID) }

// forgetStrandedNote drops a bin's last-logged anomaly note, so the next
// stranding of that bin logs again rather than being suppressed as a repeat.
//
// Called when the bin is placed. Without it the map is a slow leak AND a
// silencer: a bin recovered and later stranded again in the same way would
// match its own stale note and say nothing.
func (e *Engine) forgetStrandedNote(binID int64) {
	e.strandedNotesMu.Lock()
	delete(e.strandedNotes, binID)
	e.strandedNotesMu.Unlock()
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
