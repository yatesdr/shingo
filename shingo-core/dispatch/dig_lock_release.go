package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// FLIP 2 — the dug lane's dig claim drops when the last blocker LEAVES THE LANE,
// not when the compound terminates.
//
// ── AMENDED (§R.76): A SERVICE DIG HOLDS UNTIL ITS TARGET BIN IS COLLECTED ─
//
// Flip 2 as first built was right about transport and wrong about one shape. A
// SERVICE dig — one raised to clear a lane for somebody else, which owns no
// retrieve of its own — finished the moment its last blocker placed, dropped its
// claim, and left the bin it had just uncovered standing at an open lane mouth.
// Nothing then held that corridor. What kept the next order from taking the very
// slots the dig had emptied was the CLAIM on the target bin, checked by
// SlotsBlockedByHardClaims, and a claim is a thing that can be cancelled. Cancel
// it and on the next pass those slots are ordinary shuffle candidates: the bin
// gets re-buried by the traffic the excavation was run to get ahead of.
//
// So the claim now spans the excavation AND the retrieval it was raised for. The
// release asks one more question — is the target bin still standing there — and
// that question is deliberately PHYSICAL rather than about any order's status,
// because the point is to stop depending on an order that may not survive.
//
// The plain buried retrieve is untouched and needs nothing: it re-parents the
// demand, so the fetch is one of its own legs and legStillNeedsLane already sees
// it sitting in the lane. Only the service shape had no leg to see.
//
// ── WHAT IT COSTS TODAY ───────────────────────────────────────────────────
//
// Hold A is exclusive: a dig claims its lane for every other order for as long as
// the claim stands. It stood until the compound reached a terminal status, and a
// compound's last act is usually a DRIVE — the retrieve leg carrying the target
// bin to a line, which can be minutes away and is not in the lane at all. So the
// corridor was held shut for the length of a journey that had already left it.
//
// The dig's actual need for the lane ends when the last thing it has to lift out
// of that lane is out. Everything after that is transport.
//
// ── THE PREDICATE, AND THE CASE IT WAS NOT WRITTEN FOR ────────────────────
//
// "Any non-terminal leg still picks from this lane" was the shape proposed for
// this (R.64's C2). Under the outbound dwell that predicate has a hole big enough
// to drive the whole mechanism through: THE EVENT THAT WOULD FIRE IT IS THE
// DWELL'S OWN LIFT. A dwelling leg has picked — its bin is in transit — and is
// standing in the lane holding it. Read naively, the last blocker has "left", and
// the claim would drop with a loaded robot parked in the corridor and its
// remaining legs, which are exempt from their own parent's dig lock by design,
// free to queue in behind it.
//
// So the predicate is restated in terms of the LANE rather than the lift: the dig
// still needs the lane while any of its open legs has a bin still sitting in it,
// OR while any of them is dwelling in it. A dweller's bin is off the floor and
// its robot is not — and "off the floor" was never the question.
//
// ── FAIL CLOSED, ALWAYS ───────────────────────────────────────────────────
//
// Every read here fails towards KEEPING the claim. A lane held one leg too long
// costs a wait; a lane released one leg too early is another order entering a
// corridor a dig is still working, which is the re-burial the claim exists to
// prevent and the family F-19 came from.

// maybeReleaseDigOnLastBlockerOut releases laneID's dig claim if the dig holding
// it has nothing left to do in that lane.
//
// Called from the two places a robot actually leaves a lane: the ordinary exit
// (a bin entering transit, for a leg that drives straight out) and the dwell's
// release (the tail append, which is when a dweller starts driving). Both are
// outside the lane's evaluator mutex — waking a lane from inside it is a
// self-deadlock, which is why the dwell path defers this to after its pass.
func (d *Dispatcher) maybeReleaseDigOnLastBlockerOut(laneID int64) {
	if d.laneLock == nil || laneID == 0 {
		return
	}
	digOwner, err := d.laneLock.DigOwner(laneID)
	if err != nil || digOwner == 0 {
		return // no dig holds this lane, or the row could not be read: keep whatever is there
	}
	children, err := d.db.ListChildOrders(digOwner)
	if err != nil {
		log.Printf("dig lock: could not list compound %d's legs while deciding whether lane %d is "+
			"finished with (%v) — keeping the claim", digOwner, laneID, err)
		return
	}
	open, _ := compoundGenerations(children)
	for _, leg := range open {
		if protocol.IsTerminal(leg.Status) {
			continue
		}
		needs, why := d.legStillNeedsLane(leg, laneID)
		if needs {
			d.dbg("dig lock: compound %d keeps lane %d — leg %d %s", digOwner, laneID, leg.ID, why)
			return
		}
	}

	// NOTHING OF THIS DIG'S OWN WORK IS IN THE LANE ANY MORE — but a service dig
	// still owes the bin it uncovered, and that debt outlives every one of its
	// legs. Ask before releasing.
	//
	// FAIL CLOSED ON AN UNREADABLE PARENT, like every other read in this file. The
	// compound's own teardown (unlockLaneForCompound) is the backstop, so a lane
	// kept here is kept for a while, not forever.
	parent, pErr := d.db.GetOrder(digOwner)
	if pErr != nil {
		log.Printf("dig lock: could not read compound %d while deciding whether lane %d may be "+
			"released (%v) — keeping the claim", digOwner, laneID, pErr)
		return
	}
	owes, oErr := d.db.DigStillOwesItsTarget(parent)
	if oErr != nil {
		// The predicate has already chosen the disposition and returned it; this
		// only says so. A misconfigured target releases the lane and is LOUD,
		// because a hold nothing in the world can end is worse than an early one.
		log.Printf("dig lock: %v", oErr)
	}
	if owes {
		// LAW 8: owner, cause and releaser, on the parent itself. Without this the
		// board shows a reshuffle in `reshuffling` with every child confirmed and
		// no explanation — which is indistinguishable from the stall this is not.
		lane, lErr := d.db.GetNode(laneID)
		laneName := fmt.Sprintf("%d", laneID)
		if lErr == nil && lane != nil {
			laneName = lane.Name
		}
		d.setQueueReason(parent, protocol.QueueStorageRearranging, CauseReshuffleHoldsTarget,
			QueueParams{Lane: laneName, HoldingTarget: parent.DigTargetNode})
		d.dbg("dig lock: compound %d keeps lane %s — its blockers are out, but the bin it uncovered "+
			"at %s has not been collected yet", digOwner, laneName, parent.DigTargetNode)
		return
	}

	// Release and wake: a corridor that just became enterable is exactly the
	// condition a parked order or a dwelling robot is waiting on, and the release
	// is the last thing that happens — every event that could have re-asked has
	// already fired while it was held.
	d.laneLock.Unlock(laneID, digOwner)
	log.Printf("dig lock: compound %d released lane %d — its last blocker is out of the lane, and what "+
		"remains is transport", digOwner, laneID)
	d.EvaluateLaneReleases(laneID)
	d.RedriveHeldCompoundLegs(laneID)
	// AND THE DWELLERS IN THE REST OF THE GROUP, because this release is THEIR
	// releaser. A leg parked under `dig-holds-parking` is standing in a different
	// lane naming THIS one, so evaluating only the lane that just freed re-asks
	// everyone except the population the cause was invented for.
	d.EvaluateDwellersSharingGroupWith(laneID)

	// AND FINISH THE PARENT, IF THIS RELEASE IS WHAT IT WAS WAITING FOR.
	//
	// A service dig that held for its target has ALREADY been through
	// AdvanceCompoundOrder's completion arm and been turned away, and the event
	// that would ordinarily bring it back — its last child reaching a terminal
	// status — fired while it was still holding and does not fire twice. Without
	// this the parent sits in `reshuffling` forever with its lane already
	// released: not a wedge, because it holds nothing, but a permanent row every
	// census and every stall checker has to learn to ignore, which is the exact
	// shape abandonHealParent exists to avoid creating.
	//
	// Narrow on purpose. Only a parent that named a target reaches this, so the
	// ordinary dig's teardown is bit-for-bit what it was. AdvanceCompoundOrder is
	// the same call every child terminal event makes and it no-ops on a compound
	// whose children are still running, so the guard is about not changing
	// untouched paths rather than about safety.
	if parent.DigTargetNode != "" && parent.Status == protocol.StatusReshuffling {
		if err := d.AdvanceCompoundOrder(digOwner); err != nil {
			log.Printf("dig lock: compound %d released lane %d but could not be finished: %v "+
				"(the reconciliation floor will re-drive it — it owes nothing now)", digOwner, laneID, err)
		}
	}
}

// legStillNeedsLane reports whether one open leg of a dig still has business
// INSIDE laneID, and says which of the two reasons it is so the caller can log
// something a reader can act on.
//
// Two questions, and the second is the one the dwell added:
//
//	the leg's BIN is still in the lane — it has not been lifted yet, so the leg
//	has not done its work at all. Covers a pending leg, a dispatched one still
//	driving in, and the retrieve tail whose target is sitting there waiting to be
//	picked.
//
//	the leg is DWELLING in the lane — it has lifted, so its bin is nowhere on the
//	floor, and its robot is standing in the corridor holding it while Core chooses
//	a destination. This is the case "any leg still picks from this lane" reads as
//	finished and is not.
//
// A leg whose bin cannot be read counts as still needing the lane, which is the
// fail-closed direction: an unreadable bin must not shorten a claim.
func (d *Dispatcher) legStillNeedsLane(leg *orders.Order, laneID int64) (bool, string) {
	if d.holdsOccupancyThroughDwell(leg.ID, laneID) {
		return true, "is dwelling in it, holding a blocker that has not left"
	}
	if leg.BinID == nil {
		return false, ""
	}
	bin, err := d.db.GetBin(*leg.BinID)
	if err != nil {
		return true, "carries a bin whose position could not be read"
	}
	if bin == nil || bin.NodeID == nil {
		return false, "" // in transit and not dwelling: the robot is driving out
	}
	lane, err := d.db.LaneForNode(*bin.NodeID)
	if err != nil {
		return true, "carries a bin whose lane could not be resolved"
	}
	if lane != nil && lane.ID == laneID {
		return true, "has a bin still sitting in it"
	}
	return false, ""
}
