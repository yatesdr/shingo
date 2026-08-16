package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// THE ENFORCEMENT MODE IS GONE, AND THE MARK REPLACED IT.
//
// There used to be a LaneEnforcementMode property on the lane's GROUP —
// none / mouth / gate_choreography — selecting whether Core gated the lane and
// how. It is deleted, along with its type, its constants, its reader, and the
// `lane_enforcement` key itself. Nothing migrates: the property was never set on
// any node at either plant (verified live 2026-08-08, zero rows both cores) and
// this branch has never been deployed to one. It exits without ever having had a
// writer.
//
// WHAT REPLACES IT IS THE WAITING POINT. A lane is gated if, and only if, it has
// a mark for its robots to dwell at (PropLaneGatePoint on the LANE). One fact,
// set by the person who knows the aisle, and the thing they set IS the thing that
// makes it true — rather than a switch that has to agree with a separate mark for
// anything to work.
//
// THE RULING IS SAFE BY CONSTRUCTION, and that is worth stating because "we
// deleted the enforcement switch" reads alarming. Collision safety never lived on
// the mode. Since the unification the physical questions — is a foreign dig
// holding this lane, is a robot inside it, is the target reachable — are asked on
// every lane-entry path with no mode consulted anywhere, and occupancy rows are
// written unconditionally. The mode only ever chose the WAITING ROOM: park before
// dispatch, or drive out and dwell at a point. So removing it cannot open a
// collision; it removes the ability to configure the two waiting rooms
// inconsistently.
//
// AND ENABLEMENT BECOMES PER-LANE AND INCREMENTAL. Nothing changes at deploy —
// no marks exist anywhere, so every lane keeps parking orders pre-dispatch. Each
// lane goes gated the day a human places its mark, and rollback is clearing it
// (robots already dwelling complete under the old rules). The global flip this
// was once sequenced around does not exist.

// laneGateReservedBy tags the mouth rows the gate writes, for forensics.
const laneGateReservedBy = "lanegate"

// THE HARDCODED LANE PRIORITY BOOST WAS DELETED HERE.
//
// It was `laneShareBasePriority = 30`, added to a lane-bound move's target slot
// depth and written into CreateOrderRequest.Priority, so co-admitted stores would
// enter a single-file lane deepest-first. RDS serves the LARGEST priority first.
//
// Why it went: it is left over from an earlier attempt at this, and priority is
// latent ability rather than something the system plans around. Lane entries are
// first-come-first-serve until they are not.
//
// Why deleting it is safe, stated without inventing a migration story: the value
// was only ever applied when the group's lane_enforcement selected it, and that
// property is set on no node at either plant (verified live 2026-08-08, zero rows
// both cores) — nor has this branch ever been deployed to one. The number never
// reached a fleet. The configuration that would have activated it does not exist.
//
// THE PRIORITY MECHANISM IS DELIBERATELY UNTOUCHED. CreateOrderRequest.Priority,
// the order's own priority column, and every path that carries a priority remain
// exactly as they were — that is dormant capability, not dead code. What was
// removed is Core INVENTING a number for lane-bound moves.
//
// Worth knowing when priority does start mattering — pre-clearing blockers ahead
// of a predicted changeover is the shape to expect: this boost wrote into
// the SAME field an operator boost uses, and under larger-wins a base of 30
// silently outranked the RDS team's conventional 10. That collision is why it was
// raised. It is now gone, and the field is clean for whoever needs it next.

// laneWaitPoint returns the map point a lane's robots dwell at while Core decides
// whether they may enter, or "" when the lane has none.
//
// It is the whole of the gate's configuration. A non-empty value means: ship
// lane-bound orders unsealed to this point and append their tail when the lane is
// safe. Empty means: decide before dispatch and park the order if the answer is
// no. Both are safe; the mark chooses which one the waiting happens in.
func (d *Dispatcher) laneWaitPoint(laneID int64) string {
	return d.db.GetNodeProperty(laneID, PropLaneGatePoint)
}

// laneIsGated reports whether Core stages robots at this lane rather than parking
// their orders before dispatch. Derived, never configured separately: the
// existence of the mark IS the answer.
func (d *Dispatcher) laneIsGated(laneID int64) bool {
	return d.laneWaitPoint(laneID) != ""
}

// enteredAtDispatch narrows a dispatch's endpoints to the ones its robot is
// actually being sent INTO, which is what Hold B records.
//
// ── WHY AN ENDPOINT IS NOT AN ENTRY ───────────────────────────────────────
//
// A create bound for a gated lane stops at that lane's MARK. The robot is next to
// the corridor, not in it, and the row is taken later by the append that lets it
// in (appendGateTail → takeLaneOccupancyByID). Recording it at dispatch would
// declare a robot present in a lane it is deliberately parked outside of and wall
// that lane for the length of the dwell — which is the opposite of what staging
// is for, and is the defect PLAN §R.54 measured: a dig leg dwelling at LS_D2's
// mark held LS_D2, refusing the only order that could break the cycle it had
// joined. 997 seconds; one row.
//
// ── WHY IT IS HERE AND NOT INSIDE TakeLaneOccupancy ───────────────────────
//
// Enforcing it at the door was tried and is WRONG, and the suite said so in six
// places at once. TakeLaneOccupancy is also the general "this order IS in this
// lane" primitive — fixtures state a robot's presence with it, and a robot that
// has been let THROUGH a gate is inside a gated lane like any other. A gated lane
// holding occupancy rows is not the bug; it is the entire mechanism, since one
// robot inside at a time is what the gate is for.
//
// What is conditional is not the LANE's ability to hold a row, it is whether THIS
// DISPATCH is the moment the robot goes in — and only the seam issuing the
// dispatch knows that. So the seams answer it, each in its own vocabulary: the
// gated create walks its spliced plan (d.planNodes(preWait), lane_gate_dispatch.go)
// because it has one, and the compound leg — which takes occupancy BEFORE its plan
// is spliced, so that a failed take can still hold the child at `pending` — asks
// the lane instead, through this.
//
// laneIsGated is the shared spelling, the same predicate admission asks in
// entryDeferredToGate. An unresolvable or unreadable lane yields no skip and the
// row is taken: over-restrictive, never under, which is the direction
// TakeLaneOccupancy already argues for by name.
func (d *Dispatcher) enteredAtDispatch(ns ...*nodes.Node) []*nodes.Node {
	entered := make([]*nodes.Node, 0, len(ns))
	for _, n := range ns {
		if n == nil {
			continue
		}
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			log.Printf("lanegate: resolve lane for node %d while deciding entry: %v (recording it as "+
				"entered — an unreadable lane must not silently lose a presence)", n.ID, err)
			entered = append(entered, n)
			continue
		}
		if lane != nil && d.laneIsGated(lane.ID) {
			d.dbg("lane occupancy: this dispatch stops at %s's mark, so no row is taken here; the "+
				"tail append takes it when the robot actually enters", lane.Name)
			continue
		}
		entered = append(entered, n)
	}
	return entered
}

// TWO QUESTIONS, ONE ANSWER — and the collapse is the ruling landing.
//
// takesMouthHold and sequencesLaneEntry lived here as separate predicates over
// the enforcement mode. They returned the same value for every mode, and were
// deliberately kept apart for one reason: the derivation that was coming might
// want to answer them independently.
//
// It does not. A lane with a mark gets the whole gate — mouth holds, entry
// sequencing, staging at the point — and a lane without one gets none of it and
// keeps parking orders pre-dispatch. There is no third shape left to express, so
// the two predicates are one derivation (laneIsGated) and the split retires
// having done its job: it survived exactly until the moment that decided it.
//
// The trap the original single predicate created is not rebuilt by this. That bug
// was a boolean standing in for a MODE, so a new arm silently inherited the wrong
// branch of a three-way choice. There is no three-way choice now — the mark is
// there or it is not — and the question a caller asks is the same question in
// every case.

// laneHold is a (lane, mode) an order must hold to work that lane: outbound when
// it picks from the lane, inbound when it drops into it.
type laneHold struct {
	laneID int64
	mode   reservations.Mode
}

// resolveOrderLaneHolds computes the mouth holds a plain order needs from its
// source and destination nodes: the source lane is outbound (the order picks
// there), the destination lane is inbound (the order drops there). A node that is
// not a direct lane slot (LaneForNode == nil) contributes no hold.
//
// ONLY MARKED LANES CONTRIBUTE, and the mark is on the LANE. The rule used to be
// written here as "only lanes whose group is configured for mouth enforcement —
// none/delegated groups contribute nothing", which described a three-way
// lane_enforcement property on the GROUP that no longer exists: the mode enum
// was deleted, the mark is the enablement, and `delegated` went with it. The code
// below has asked laneIsGated — does this lane carry a gate point — since then.
//
// The consequence it stated is unchanged and still worth stating: an unmarked
// plant yields zero holds, so the gate is a no-op at both plants today.
func (d *Dispatcher) resolveOrderLaneHolds(sourceNode, destNode *nodes.Node) ([]laneHold, error) {
	var holds []laneHold
	add := func(n *nodes.Node, mode reservations.Mode) error {
		if n == nil {
			return nil
		}
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			return err
		}
		if lane == nil || lane.ParentID == nil {
			return nil // not a lane slot, or a lane with no group — no hold
		}
		if !d.laneIsGated(lane.ID) {
			return nil // no mark, no gate: Core does not own this lane's mouth
		}
		holds = append(holds, laneHold{laneID: lane.ID, mode: mode})
		return nil
	}
	if err := add(sourceNode, reservations.ModeOutbound); err != nil {
		return nil, err
	}
	if err := add(destNode, reservations.ModeInbound); err != nil {
		return nil, err
	}
	return holds, nil
}

// acquireOrderLanes takes every hold the order needs, all-or-nothing across
// modes. It returns admitted=false on a mode conflict (the caller requeues the
// order in sourcing under WAITING_FOR_SLOT, per Rule 1); a non-nil error is a
// transient DB failure. With no holds (the common, unconfigured case) it admits
// immediately.
//
// AcquireLanes is per-mode all-or-nothing; an order picking from one lane and
// dropping into another needs one call per mode, so a conflict on the second
// mode rolls back the first via the order-scoped ReleaseLanesByOwner. All rows
// are owned by the order, so that release reclaims exactly this gate's takes.
func (d *Dispatcher) acquireOrderLanes(orderID int64, holds []laneHold) (admitted bool, err error) {
	if len(holds) == 0 {
		return true, nil
	}
	byMode := map[reservations.Mode][]int64{}
	for _, h := range holds {
		byMode[h.mode] = append(byMode[h.mode], h.laneID)
	}
	for mode, lanes := range byMode {
		if aErr := reservations.AcquireLanes(d.db.DB, orderID, mode, laneGateReservedBy, lanes...); aErr != nil {
			if errors.Is(aErr, reservations.ErrReservationConflict) {
				// Roll back any holds taken for an earlier mode so the acquire is
				// all-or-nothing across the order's lanes. The freed lanes are
				// discarded deliberately: nothing was ever admitted into them, so no
				// dweller is waiting on this release — the caller parks and the
				// ordinary triggers re-ask.
				_, _ = reservations.ReleaseLanesByOwner(d.db.DB, orderID)
				return false, nil
			}
			return false, aErr
		}
	}
	return true, nil
}

// releaseOrderLaneFor releases the order's mouth hold on the lane that contains
// node, if any — the early per-block handoff (§4): an outbound hold drops when
// the bin leaves the lane, an inbound hold drops when the bin lands. Owner-scoped
// and a no-op when no row exists, so it is byte-identical when the gate is off.
//
// A DIG CLAIM IS EXEMPT. This is a per-VISIT release and a dig's hold is
// per-RESHUFFLE: it has to outlive several legs, and the first leg's pickup is
// not the end of anything. Because laneOwnerFor routes a child's block progress
// to the compound parent, this used to arrive owned by the parent, match the
// parent's dig row, and delete it at leg one — leaving every later leg of the
// reshuffle with no durable claim on the lane at all.
//
// The exemption lives in ReleaseLaneHandoff's SQL rather than here, so there is
// no window between reading the mode and deleting the row. Everything else about
// the handoff is unchanged, which is the point: it is right for plain orders and
// was wrong only for digs.
func (d *Dispatcher) releaseOrderLaneFor(orderID int64, node *nodes.Node) error {
	if node == nil {
		return nil
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil {
		return err
	}
	if lane == nil {
		return nil
	}
	return reservations.ReleaseLaneHandoff(d.db.DB, orderID, lane.ID)
}

// TakeLaneOccupancy records that an order is INSIDE the lanes it was just
// dispatched into — Hold B, the second of the two lane holds.
//
// Called at dispatch rather than on any robot signal, and that is the design
// rather than a shortcut. Core cannot observe a robot entering a lane: every
// available signal is lagging, the earliest being a block completing AT a slot,
// which is already inside. It does not need to. Nothing enters a lane that Core
// did not send there, so Core's own dispatch decision IS the entry moment, and
// it is knowable exactly when it becomes true.
//
// Both endpoints are considered: a dig leg picks OUT of one lane and may place
// INTO another, and the robot is inside each while it is there. A node that is
// not a lane slot contributes nothing.
//
// NO LONGER ADVISORY. Step 6 removed the sibling-in-flight guard, and from that
// moment this row is the ONLY thing keeping two legs of one reshuffle out of one
// lane: admission reads it and holds the child. The doc that used to
// stand here — "it records; it does not arbitrate", true while the compound
// scheduler dispatched one child at a time — described a world that ended with
// that guard.
//
// So the error is returned rather than logged. The read side fails CLOSED (an
// unreadable lane is a busy lane, compound.go); a write side that logged and
// carried on meant a lane whose occupancy could not be RECORDED read as empty to
// the next leg — the same collision, arrived at from the other direction.
//
// Returns on the FIRST failure, deliberately leaving any rows already taken in
// place. A partial take is over-restrictive, never under: the extra row makes a
// lane look busier than it is, which costs a wait. Rolling it back would be the
// unsafe direction, and the caller holds the child rather than dispatching, so
// nothing is inside the lanes this reported.
func (d *Dispatcher) TakeLaneOccupancy(orderID int64, nodes ...*nodes.Node) error {
	for _, laneID := range d.lanesFor(nodes...) {
		if err := reservations.AcquireOccupancy(d.db.DB, orderID, laneID); err != nil {
			return fmt.Errorf("take occupancy for order %d on lane %d: %w", orderID, laneID, err)
		}
	}
	return nil
}

// takeLaneOccupancyByID is TakeLaneOccupancy for a caller that already knows the
// lane rather than a node in it — the gated append, whose lane comes off the
// wait step it is releasing (WaitLane) instead of off an endpoint column.
//
// Same semantics as the node-taking form: idempotent per (order, lane), and the
// error is RETURNED rather than logged, because a presence that could not be
// recorded reads as an empty lane to the next entrant.
func (d *Dispatcher) takeLaneOccupancyByID(orderID, laneID int64) error {
	if laneID == 0 {
		return nil // not a lane-owned wait — nothing to record
	}
	if err := reservations.AcquireOccupancy(d.db.DB, orderID, laneID); err != nil {
		return fmt.Errorf("take occupancy for order %d on lane %d: %w", orderID, laneID, err)
	}
	return nil
}

// ReleaseLaneOccupancy records that an order is out of every lane it occupied.
//
// Fired on DROPOFF completion. It is NO LONGER THE ONLY RELEASE, and the reason
// it used to be is now on file as wrong.
//
// ── WHAT THIS DOC USED TO ARGUE, AND WHY IT NO LONGER DOES ───────────────
//
// It said: "Fired on DROPOFF completion, not on pickup. After a pickup the robot
// is holding the bin and still physically in the lane; it is out once it has
// placed at the destination. Releasing at pickup would declare the lane free
// with a robot still standing in it, which is the whole failure this hold exists
// to prevent."
//
// That is true of ONE shape — a leg that picks and places inside the same lane —
// and it was applied to all of them. For every other shape the robot picks,
// drives out, and the row outlives its presence: five robots were refused at the
// mouth of an EMPTY lane on the lane-stress rig, 2026-08-10, by an order that had
// gone, with the holder itself queued behind its own stale row. So occupancy also
// releases on the exit now (HandleTransitForLaneGate), on the same
// EventBinEnteredTransit the mouth hold has used since §4.
//
// The argument above is kept verbatim rather than deleted because it is the case
// AGAINST what is there now, and it survives in one shape. It is preserved the
// same way in the test that pinned it —
// TestLaneOccupancy_EndsWhenTheRobotLeavesTheLane — which also carries the owner
// ruling, the window the early release opens, and the standing instruction: if
// two robots are ever seen in one lane, build the exit marker. Do not move the
// release back here. That reinstates the five-robot jam.
//
// THIS RELEASE STILL EARNS ITS PLACE. It is the right release for the drop end:
// an order that PLACES into a lane never hits the pickup path for it, and a
// store's only visit ends here.
//
// It releases ALL of the order's occupancy rather than the drop node's lane,
// because a dig leg's two endpoints are usually different lanes: it is the
// SOURCE lane the robot has finally left by placing elsewhere. "This leg has
// placed its bin and is out" is a statement about the leg, not about one node.
//
// Terminalization needs no separate arm: TerminalizeOrder's ReleaseByOrder is
// order-keyed and kind-agnostic, so a child that fails or is cancelled drops its
// occupancy in the same transaction that ends it.
func (d *Dispatcher) ReleaseLaneOccupancy(orderID int64) {
	if err := reservations.ReleaseAllOccupancy(d.db.DB, orderID); err != nil {
		log.Printf("lanegate: release occupancy for order %d: %v", orderID, err)
	}
}

// lanesFor maps nodes to the distinct lanes that contain them, skipping nodes
// that are not lane slots.
func (d *Dispatcher) lanesFor(ns ...*nodes.Node) []int64 {
	seen := make(map[int64]bool, len(ns))
	var out []int64
	for _, n := range ns {
		if n == nil {
			continue
		}
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			log.Printf("lanegate: resolve lane for node %d: %v", n.ID, err)
			continue
		}
		if lane == nil || seen[lane.ID] {
			continue
		}
		seen[lane.ID] = true
		out = append(out, lane.ID)
	}
	return out
}

// laneHoldRead is one lane's mouth rows as they came back, error included. The
// error is carried rather than handled so the classification below can see the
// difference between "read it, no dig" and "could not read it".
type laneHoldRead struct {
	rows []reservations.MouthHold
	err  error
}

// classifyLaneHoldCause turns those reads into the engineer-facing cause.
//
// Pure, and split out from the gathering for one reason: the interesting arm is
// the one where a read FAILED, and there is no way to make a SELECT fail for one
// lane in a shared test database without breaking every other test using the
// table. A pure classifier can be handed the failure directly. (The gathering
// half is a loop and a call; the decision is what had the bug.)
//
// Precedence is deliberate and is the fix:
//
//	a readable dig anywhere  -> lane-held-dig        (definite, and it wins)
//	otherwise any failed read -> lane-held-unreadable (we cannot rule a dig out)
//	otherwise                 -> lane-held-traffic    (definite)
//
// It used to `continue` past a failed read and fall through to traffic, so a
// dig-held lane whose row could not be read was filed as ordinary traffic
// contention. Nothing was unsafe — admission had already refused, and this only
// labels that refusal — but the label is what an engineer reads when a lane
// stalls, and a dig stall and a traffic stall are investigated differently. It
// is the §17.5/§17.8 family: not an alarm that fails to fire, an alarm that
// fires with the wrong name on it, which costs the next reader more than silence
// would.
//
// A definite dig still wins over an unreadable sibling lane, because a dig
// SEEN is a stronger fact than a lane not seen; reporting unreadable there
// would hide an answer we actually have.
func classifyLaneHoldCause(orderID int64, reads []laneHoldRead) QueueCause {
	unreadable := false
	for _, r := range reads {
		if r.err != nil {
			unreadable = true
			continue
		}
		for _, row := range r.rows {
			if row.OrderID != orderID && row.Mode == reservations.ModeDig {
				return CauseLaneHeldDig
			}
		}
	}
	if unreadable {
		return CauseLaneHeldUnreadable
	}
	return CauseLaneHeldTraffic
}

// causeForLaneHolds classifies a lane conflict for the engineer-facing queue
// cause (§6). The operator sees the same "Waiting for a slot at ‹lane›" in every
// case; this is the tag underneath it.
func (d *Dispatcher) causeForLaneHolds(orderID int64, holds []laneHold) QueueCause {
	reads := make([]laneHoldRead, 0, len(holds))
	for _, h := range holds {
		rows, err := reservations.ActiveMouthRows(d.db.DB, h.laneID)
		if err != nil {
			log.Printf("lanegate: mouth rows for lane %d unreadable while labelling order %d's wait: %v",
				h.laneID, orderID, err)
		}
		reads = append(reads, laneHoldRead{rows: rows, err: err})
	}
	return classifyLaneHoldCause(orderID, reads)
}

// ownsDig reports whether an order may pass a dig hold on a lane: it either IS
// the dig owner, or it is a leg of that dig.
//
// THE ONE DIG EXEMPTION. It absorbed isOwnDigLeg, which was the same predicate
// minus the owner arm and which admission and the retrieve classifier used
// while this one served the plain entry path. Two answers to one question is
// what the convergence ended; this is the answer that survived, because the
// arm it has and the other lacked is load-bearing and the arm it lacks is not
// reachable.
//
// ── THE OWNER ARM, WHICH isOwnDigLeg COULD NOT EXPRESS ────────────────────
//
// laneOwnerFor returns the order itself when it has no parent, and isOwnDigLeg
// required owner != order.ID, so a dig's own OWNER failed it. On the entry path
// that case is routine rather than exotic: in expose mode the lane lock is
// transferred to the complex parent (compound.go), and ResumeCompound puts that
// parent back through the scanner to re-resolve its own pickup — so the order
// asking is the dig owner, entering the lane its own dig holds, and that pickup
// is what releases the lock. Refusing it would not be a wait, it would be a
// wedge. Pinned by TestAcquireLanesForOrder_OwnDigAdmitsTheDigOwner.
//
// ── WHAT isOwnDigLeg's NARROWING PROTECTED, AND WHY LOSING IT IS SAFE ─────
//
// Carried forward verbatim from its doc, because it is a real future hazard
// rather than a historical note. The narrowing to CHILDREN existed so that a
// GATE-STAGED digger could not be released into the lane its own dig is still
// working: laneOwnerFor would return its own id, match, and open the gate.
//
// It cannot happen today, and not by luck — both planners divert a buried
// retrieve to Reshuffling before it ever reaches the fleet
// (complex_reshuffle.go planBuriedReshuffleAtIntake / handleComplexBuriedOnReplay,
// planning_service.go planBuriedReshuffle), so the digger carries no vendor
// order and is never gate-staged. GIVE THE DIGGER A PRE-POSITION AND THIS IS
// THE LINE TO REVISIT: the exemption would then need to be claim-scoped, or the
// release path would need the narrowing back on its own terms.
//
// The other line that makes this wrong if it moves is unchanged:
// reservations.AcquireLanes' dig rule. If it ever admits two claims on one
// lane, "the dig" stops being singular and parent identity stops identifying
// it. A lane carries exactly one dig claim because admitMouth refuses a second
// outright.
//
// Order-id only, deliberately: laneOwnerFor already answers the parent question,
// so nothing here needs the order struct and the common path costs no extra read.
//
// ── IT IS NOW A CALLER, NOT A SIBLING ─────────────────────────────────────
//
// The two comparisons below used to be written out here, and that made this
// one of three places answering "does a dig hold exclude this order" — the
// other two being store.ListChildNodesUnlocked (which exempted nobody) and
// dig planning (which did not ask). They disagreed, and the disagreement
// arrested the ring: see store/reservations/dig_exclusion.go.
//
// The self arm is no longer special-cased ahead of the parent read. It saved
// one GetOrder on a path that has just done a reservations round trip for
// DigOwner, and buying that back would mean writing half the predicate out
// again right underneath a comment saying not to.
func (d *Dispatcher) ownsDig(orderID, digOwner int64) bool {
	return !d.digAsker(orderID).ExcludedBy(digOwner)
}

// digAsker resolves an order ID into the (self, lane owner) pair the dig-lock
// predicate takes, reading the order to find its parent.
//
// Callers that already hold the order struct use digAskerFor and skip the
// read. The two are one rule: this one loads and delegates.
func (d *Dispatcher) digAsker(orderID int64) reservations.DigAsker {
	o, err := d.db.GetOrder(orderID)
	if err != nil || o == nil {
		// Same disposition laneOwnerFor takes on an unreadable order: treat it
		// as its own owner. It narrows the exemption to the order itself, which
		// is the conservative direction — a missed exemption is a wait, an
		// invented one is entry into a lane somebody else is excavating.
		return reservations.AskerFor(orderID, orderID)
	}
	return digAskerFor(o)
}

// digAskerFor builds the dig-lock asker from an order already in hand.
//
// THE RULE IS "parent, or self when there is none", and it is written once. It
// matches laneOwnerFor because it is the same question about lane ownership —
// laneOwnerFor exists for callers who have only an id and must pay a read for
// the answer, which most resolution sites do not.
func digAskerFor(o *orders.Order) reservations.DigAsker {
	if o == nil {
		return reservations.Anyone
	}
	owner := o.ID
	if o.ParentOrderID != nil {
		owner = *o.ParentOrderID
	}
	return reservations.AskerFor(o.ID, owner)
}

// digRefusalFor WAS HERE AND IS NOW admission (admission.go).
//
// It answered the DIG question for an order about to work a set of nodes,
// REGARDLESS OF ENFORCEMENT MODE — the correction that closed the floor defect
// where a plain retrieve walked into a corridor another reshuffle owned. That
// correction is intact; only its implementation moved.
//
// It went because it was a second spelling of admission's first arm, and the
// two had drifted: this one exempted the dig's own OWNER (ownsDig) and
// admission's did not (isOwnDigLeg). One question with two answers is the thing
// the convergence exists to end. The surviving answer is ownsDig's — see the
// note on that function for which arm is load-bearing and why.
//
// AcquireLanesForOrder now asks the same question through the same function,
// declaring skipsForPlainEntry: the dig only, which is exactly what this asked.

// AcquireLanesForOrder takes the mouth holds a plain order needs before it
// dispatches — outbound on its source lane, inbound on its destination lane, for
// lanes whose group is configured for mouth enforcement. The scanner calls it
// just before the fleet commit; on a conflict it returns admitted=false plus the
// operator cause and the contended lane's name, and the scanner parks the order
// in sourcing under WAITING_FOR_SLOT holding its soft reservations (Rule 1).
//
// admitted=true with empty cause/lane means there was nothing to gate (no
// mouth-enforced lane on the order's path), so an unconfigured plant is a no-op
// and behavior is byte-identical. A non-nil error is a transient DB failure.
//
// IT DELEGATES THE PHYSICAL QUESTIONS AND KEEPS THE MOUTH. That split is the
// whole of its relationship with admission, and each half has its own reason.
//
// The physical questions — dig exclusion, presence, reachability — are ordinary
// reads with no transaction to hold, so they go through admit() like every other
// entry path. Answering them here in a second spelling is exactly what the
// convergence ended; digRefusalFor above is the tombstone of the last one, and
// the drift it had accumulated is why one function now owns the answer.
//
// The MOUTH stays, because admitMouth is not a decision this site could lift
// out. It runs under pg_advisory_xact_lock inside the acquire's own transaction
// (mouth.go), so moving it would either drop the lock or hold the transaction
// open across a decision made outside it. It is the acquisition, not a judgement
// about one — which is why admission.go's boundary map files it under IS NOT A
// DECISION AT ALL rather than under a missing delegate.
//
// So this site is both a caller of admission and the place the acquire happens,
// in that order, and that is not a cycle: admit() runs before the acquire and
// admission never calls back into here.
//
// The audit of where a plain order gets each admission question answered is
// beside that boundary map. It has one empty cell — reachability for a held-bin
// order — declared as skipsForPlainEntry rather than left to be inferred from
// missing code, and the caller says which of the two it is via EntryKind.
func (d *Dispatcher) AcquireLanesForOrder(order *orders.Order, sourceNode, destNode *nodes.Node, kind EntryKind) (admitted bool, cause QueueCause, laneName string, err error) {
	if order == nil {
		// A caller bug, not a refusal with a cause: there is no order to park and
		// no queue row to write one onto. Same disposition as admission's own
		// nil-order arm, for the same reason.
		return false, "", "", fmt.Errorf("lane gate: no order to acquire lanes for")
	}
	// THE PHYSICAL QUESTIONS FIRST, through admission — one function, one answer
	// per question. The skip set comes from the CALLER's kind (EntryFreshBin /
	// EntryHeldBin), because the one surviving skip — reachability — is justified
	// by the finder having answered it, and only the fresh caller went through the
	// finder. See skipsForEntry.
	//
	// Asked ahead of the mouth holds because a dig EXCLUDES everything: there is
	// no point taking holds on a lane that is not open to this order at all, and
	// the refusal carries its own cause and its own lane name.
	v, err := d.admit(admissionSituation{
		order:      order,
		sourceNode: sourceNode,
		destNode:   destNode,
		skip:       skipsForEntry(kind),
	})
	if err != nil {
		return false, "", "", err
	}
	if !v.Admitted() {
		return false, v.Cause(), v.Lane(), nil
	}

	holds, err := d.resolveOrderLaneHolds(sourceNode, destNode)
	if err != nil {
		return false, "", "", err
	}
	if len(holds) == 0 {
		return true, "", "", nil // nothing gated by the MOUTH — the dig is answered above
	}
	admitted, err = d.acquireOrderLanes(order.ID, holds)
	if err != nil {
		return false, "", "", err
	}
	if admitted {
		return true, "", "", nil
	}
	return false, d.causeForLaneHolds(order.ID, holds), d.laneDisplayName(holds), nil
}

// BuriedForHeldBin builds the BuriedError for an order whose HELD bin has become
// unreachable, so the caller can plan the dig that unburies it.
//
// It exists because a refusal is not an answer. Admission can now tell a held-bin
// order that its bin is walled, but a refusal with nothing to clear it is a
// permanent park — the wedge shape this stream keeps refusing to build. The fresh
// path never had that problem: the finder returns OutcomeReshuffle carrying a
// BuriedError, and the scanner turns it into PlanBuriedReshuffle. This is the same
// fact, reconstructed for the caller that did not go through the finder.
//
// The bin's CURRENT slot, not the order's remembered source_node: pickupSlotNow
// is the one definition of where a held bin actually sits, shared with the
// classifier and the gate rebind, and a dig planned against an abandoned slot
// would dig out the wrong thing.
func (d *Dispatcher) BuriedForHeldBin(order *orders.Order) (*BuriedError, error) {
	if order == nil || order.BinID == nil {
		return nil, fmt.Errorf("buried-for-held-bin: order holds no bin")
	}
	slot, err := d.db.GetNodeByDotName(order.SourceNode)
	if err != nil || slot == nil {
		return nil, fmt.Errorf("buried-for-held-bin: source node %q: %v", order.SourceNode, err)
	}
	lane, err := d.db.LaneForNode(slot.ID)
	if err != nil {
		return nil, err
	}
	if lane == nil {
		return nil, fmt.Errorf("buried-for-held-bin: %q is not a lane slot", slot.Name)
	}
	at, _, err := d.pickupSlotNow(order, lane)
	if err != nil {
		return nil, err
	}
	bin, err := d.db.GetBin(*order.BinID)
	if err != nil || bin == nil {
		return nil, fmt.Errorf("buried-for-held-bin: bin %d: %v", *order.BinID, err)
	}
	return &BuriedError{Bin: bin, Slot: at, LaneID: lane.ID}, nil
}

// laneDisplayName returns a human name for the first contended lane, for the
// "Waiting for a slot at ‹lane›" queue sentence.
func (d *Dispatcher) laneDisplayName(holds []laneHold) string {
	if len(holds) == 0 {
		return ""
	}
	if n, err := d.db.GetNode(holds[0].laneID); err == nil && n != nil {
		return n.Name
	}
	return ""
}

// ReleaseLanesForOrder drops all of an order's mouth holds. Used on a fleet-
// dispatch failure rollback: the robot never committed, so the hold taken by
// AcquireLanesForOrder must not linger and block the lane. Owner-scoped; a no-op
// when the order holds none.
func (d *Dispatcher) ReleaseLanesForOrder(orderID int64) error {
	_, err := reservations.ReleaseLanesByOwner(d.db.DB, orderID)
	return err
}

// laneOwnerFor resolves the order that OWNS a lane mouth row for a block: the
// order itself for a plain order, or its complex parent for a compound child
// (children never own rows, §2). So a child's block progress releases the
// parent-owned hold.
func (d *Dispatcher) laneOwnerFor(orderID int64) int64 {
	o, err := d.db.GetOrder(orderID)
	if err != nil || o == nil || o.ParentOrderID == nil {
		return orderID
	}
	return *o.ParentOrderID
}

// HandleTransitForLaneGate releases BOTH of the owner's holds on the lane a
// picked bin just LEFT (§4 pickup / early handoff). Fired on
// EventBinEnteredTransit, routed to the compound parent for a child. A no-op when
// the from-node is not a lane or no rows are held — byte-identical when the gate
// is off.
//
// ── OCCUPANCY LEAVES HERE TOO, AND IT USED NOT TO ─────────────────────────
//
// The mouth hold has released on this signal since §4. Occupancy did not: its
// only release was the DROPOFF completing (wiring_block_completed.go), on the
// stated reasoning that "after a pickup the robot is still in the lane holding
// the bin". True for a pickup whose dropoff is in the SAME lane — and false for
// every other shape, where the robot picks, drives out, and the row it leaves
// behind declares it present in a corridor it has left.
//
// The cost of that was measured on the lane-stress rig, 2026-08-10: a robot
// entered a lane, picked, and drove to its next gate point. Its occupancy row
// stayed. Four other robots queued at the mouth of that lane refused with
// lane-occupied, by an order that had gone — and the one holding the stale row
// was itself queued behind it, self-exempting, so nothing in the set could move.
// Five robots, an EMPTY lane, indefinitely.
//
// Waiting for the dropoff is not an option worth having: the next drop can be a
// line delivery ten or twenty minutes away, and the corridor is falsely occupied
// for all of it.
//
// ── WHAT THIS TRADES, STATED PLAINLY ──────────────────────────────────────
//
// The bin entering transit means the robot has LIFTED it and is driving out — it
// is not yet through the mouth. So this releases while the robot is still
// physically inside, and a second order can be admitted into a lane the first is
// still leaving. That window is the deliberate trade: an owner ruling took it
// because the alternative is a corridor held for the length of a line run, and
// because the mouth hold has been releasing on this exact signal all along.
//
// IF IT BITES, THE FIX IS A REAL EXIT EVENT, NOT A LONGER HOLD. The gate already
// splices a wait block at the gate point on the way IN; the symmetric move is a
// marker at the mouth on the way OUT, whose completion is the exit. That makes
// the moment observable instead of inferred, and it is the first thing to reach
// for if two robots are ever seen in one lane. Do not simply move this back to
// the dropoff — that reinstates the five-robot jam.
//
// PER-LANE, NOT PER-ORDER. ReleaseAllOccupancy would drop the order's presence in
// every corridor it is in, and leaving one says nothing about the other.
func (d *Dispatcher) HandleTransitForLaneGate(orderID, fromNodeID int64) {
	if fromNodeID == 0 {
		return
	}
	owner := d.laneOwnerFor(orderID)
	node, err := d.db.GetNode(fromNodeID)
	if err != nil || node == nil {
		return
	}
	if err := d.releaseOrderLaneFor(owner, node); err != nil {
		log.Printf("lanegate: release hold for order %d (owner %d) on transit from node %d: %v",
			orderID, owner, fromNodeID, err)
	}
	// ── THE TWO ARGUMENTS DIFFER ON PURPOSE. DO NOT "FIX" THEM TO MATCH. ──
	//
	// The mouth hold above is PARENT-owned: a compound's legs share one inbound
	// row taken by the parent, so it is released by `owner`. Occupancy is
	// ORDER-owned: every writer keys it on the order that is physically inside —
	// TakeLaneOccupancy(next.ID) at the compound leg's dispatch (compound.go),
	// takeLaneOccupancyByID(order.ID) at the gated append, commitToFleet's take
	// for a complex order — and ReleaseLaneOccupancy on the dropoff releases it
	// the same way. So the exit release must use `orderID` too.
	//
	// IT DID NOT, AND THAT MADE THIS ENTIRE FIX A NO-OP FOR DIG LEGS. The delete
	// is `WHERE order_id=$1`, so releasing a child's row under its PARENT's id
	// matched nothing: the row survived to the dropoff exactly as it had before
	// the exit release existed, and the five-robot jam stayed reachable on the
	// compound-leg population — which is most of the lane traffic a dig produces.
	// It read as correct because the routing was copied from the line above it,
	// where it IS correct, and because every test order was parentless, so
	// owner == orderID and the two spellings were indistinguishable.
	d.releaseOccupancyOnExit(orderID, node)
}

// releaseOccupancyOnExit drops ONE ORDER's occupancy on the lane containing node,
// and re-evaluates that lane because a corridor that just emptied is exactly the
// condition a dweller at its mark is waiting on.
//
// orderID is the order that HOLDS the row, never its compound parent — see the
// note at the call site for why that distinction is load-bearing and why the
// mouth release one line above it legitimately uses the other one.
//
// The evaluation is the half that makes this a fix rather than a bookkeeping
// correction: without it the row goes but nobody re-asks, and on a quiet lane the
// next firing is the 60-second floor. Same reasoning, same shape, as the dig
// lock's release (unlockLaneForCompound).
func (d *Dispatcher) releaseOccupancyOnExit(orderID int64, node *nodes.Node) {
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil || lane == nil {
		return
	}
	// ── THE LIFT IS NOT AN EXIT FOR A DWELLER ─────────────────────────────
	//
	// This whole path rests on one inference: a bin entering transit means the
	// robot has it up and is driving OUT. That is true of every leg that knows
	// where it is going, and false of a dig leg under the outbound dwell — it
	// lifts and then STAYS, standing in the shallowest slot of the lane it is
	// digging while Core chooses a destination.
	//
	// Releasing here would drop the row while the robot is still in the corridor,
	// and the next sibling leg would be admitted in behind it. `ModeDig` does not
	// contain that, which is the correction this fix exists for: a sibling leg is
	// exempt from its own parent's dig lock BY DESIGN (ownsDig routes the leg's
	// question to its parent), so the lock excludes everyone EXCEPT the exact
	// population that queues behind a dwelling robot. Two robots nose to tail in
	// one single-file lane, the deeper one's exit behind the shallower one's — and
	// in-lane stacking is an unproven property of the fleet, not a thing to
	// discover in production.
	//
	// WHAT IS GIVEN UP IS ONLY THE DWELL'S OWN OVERLAP. The row still drops when
	// the robot actually drives out, which is the moment its tail is appended
	// (releaseDwellingDigLeg → releaseOccupancyForExitFromLane), so the next leg
	// still enters during the drive-out — where the pipelining gain of retiring
	// the sibling-in-flight guard actually lives. The part surrendered is the part
	// that did not exist before the dwell.
	if d.holdsOccupancyThroughDwell(orderID, lane.ID) {
		d.dbg("lane occupancy: order %d lifted in lane %s and is DWELLING there — the row is held until "+
			"its tail is appended and it drives out", orderID, lane.Name)
		// FLIP 2 IS STILL ASKED, AND ITS ANSWER HERE IS "NO" — which is the whole
		// reason its predicate had to be restated.
		//
		// The two facts are independent: occupancy says whether THIS ROBOT is in the
		// corridor, the dig claim says whether THE EXCAVATION still has work in it.
		// A lift is the natural moment to ask the second, and asking it only on the
		// paths where the first says "gone" would make the dweller invisible to it —
		// so the arm that keeps the claim for a dwelling leg would be a guard nothing
		// could ever exercise, which is a guard nobody can trust.
		d.maybeReleaseDigOnLastBlockerOut(lane.ID)
		return
	}
	d.releaseOccupancyForExitFromLane(orderID, lane)
}

// releaseOccupancyForExitFromLane is the release proper: drop the row and wake
// the lane, because a corridor that just emptied is exactly the condition a
// dweller at its mark is waiting on.
//
// Split from the caller above so the DWELL's own exit — which happens at the
// append rather than at the lift — goes through the same two steps rather than a
// second spelling of them.
func (d *Dispatcher) releaseOccupancyForExitFromLane(orderID int64, lane *nodes.Node) {
	if err := reservations.ReleaseOccupancyForLane(d.db.DB, orderID, lane.ID); err != nil {
		log.Printf("lanegate: release occupancy for order %d on lane %d: %v", orderID, lane.ID, err)
		return
	}
	// AND THE DIG'S CLAIM GOES WITH THE LAST BLOCKER — flip 2. A robot leaving is
	// the moment to ask whether the excavation still has anything IN this lane, as
	// opposed to a transport leg still driving somewhere else. It is asked before
	// the wake below rather than after, so a lane that just became enterable is
	// evaluated once, in a state that includes both facts.
	d.maybeReleaseDigOnLastBlockerOut(lane.ID)
	d.EvaluateLaneReleases(lane.ID)
	d.RedriveHeldCompoundLegs(lane.ID)
}

// holdsOccupancyThroughDwell reports whether this order is currently DWELLING in
// laneID — parked at an outbound wait that names it.
//
// It reads the order's own durable plan, which is the same source every other
// dwell decision reads (IsGateStaged, the candidate walk, the floor). A leg that
// has already been released has indexed past its wait, so this goes false at the
// moment the append lands, which is exactly when the release becomes correct.
func (d *Dispatcher) holdsOccupancyThroughDwell(orderID, laneID int64) bool {
	order, err := d.db.GetOrder(orderID)
	if err != nil || order == nil || !IsGateStaged(order) {
		return false
	}
	var steps []resolvedStep
	if json.Unmarshal([]byte(order.StepsJSON), &steps) != nil {
		return false
	}
	if !waitGatesAnAppend(steps, order.WaitIndex) {
		return false
	}
	w, ok := waitAt(steps, order.WaitIndex)
	return ok && w.WaitLane == laneID
}

// ReleaseInboundLaneForOrder releases the owner's mouth hold on the lane an order
// just DROPPED into (§4 dropoff / early handoff). Fired from the store block-
// completion handler BEFORE the delivery-node early-return, routed to the compound
// parent for a child. A no-op when the drop node is not a lane or no mouth row is
// held.
func (d *Dispatcher) ReleaseInboundLaneForOrder(orderID int64, dropNodeName string) {
	if dropNodeName == "" {
		return
	}
	owner := d.laneOwnerFor(orderID)
	node, err := d.db.GetNodeByDotName(dropNodeName)
	if err != nil || node == nil {
		return
	}
	if err := d.releaseOrderLaneFor(owner, node); err != nil {
		log.Printf("lanegate: release hold for order %d (owner %d) on dropoff at %s: %v",
			orderID, owner, dropNodeName, err)
	}
}
