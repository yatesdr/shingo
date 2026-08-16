package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// digHandoffReservedBy tags the mouth row a finished dig hands to the order
// collecting its bin, so a reader looking at a lane's holders can tell an
// ordinary outbound hold from one that came out of an excavation.
const digHandoffReservedBy = "dighandoff"

// FLIP 2 — the dug lane's dig claim drops when the last blocker LEAVES THE LANE,
// not when the compound terminates.
//
// ── AMENDED: THE EXCAVATION ENDS, AND THE CORRIDOR CHANGES HANDS ──────────
//
// Flip 2 as first built was right about transport and wrong about one shape. A
// SERVICE dig — one raised to clear a lane for somebody else, which owns no
// retrieve of its own — finished the moment its last blocker placed, dropped its
// claim, and left the bin it had just uncovered standing at an open lane mouth.
// Nothing then held that corridor, and the slots the dig had just emptied were
// the cheapest shuffle candidates in the group: the bin got re-buried by the
// traffic the excavation was run to get ahead of.
//
// The first fix for that was to make the dig KEEP its lane until the bin it
// uncovered was collected. It closed the re-burial window and opened a worse
// one. A dig holding for its target is a finished order that never terminates —
// a third non-terminal state, invisible to every stall checker, holding a
// corridor on behalf of a demand it has no way to ask about. On the lane-stress
// rig 2026-08-13 five of them held five lanes, and no live order wanted any of
// the five slots they were holding for: every demand had re-resolved onto a bin
// somewhere else while the digging went on. The holds were against a snapshot
// the plant had moved past, and together they were the wedge.
//
// So the dig does not hold and does not linger. IT HANDS THE CORRIDOR TO THE
// ORDER THAT IS ACTUALLY COMING FOR THE BIN — looked up now, from what the plant
// wants now — as that order's own outbound hold, and then finishes. Three things
// follow, and they are the point:
//
//	THE HOLD ENDS BY ITSELF. Its owner is a live order. Its per-visit release
//	drops it when the bin clears the lane and its terminalization drops it
//	whatever else happens, so no hold can outlive the work it was taken for.
//
//	A DEMAND THAT MOVED ON HOLDS NOTHING. No collector, no handoff, lane free.
//
//	THE DIG TERMINATES ON ITS ORDINARY PATH. No guard in the completion arm, no
//	carve-out in the reconciliation sweep, no re-drive to un-stick it later.
//
// The plain buried retrieve is untouched and needs none of this: it re-parents
// the demand, so the fetch is one of its own legs and legStillNeedsLane already
// sees it sitting in the lane. Only the service shape had no leg to see.
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

	// NOTHING OF THIS DIG'S OWN WORK IS IN THE LANE ANY MORE. If it uncovered a
	// bin, the corridor changes hands here rather than opening; if it did not, it
	// opens.
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
	if d.handOffDugLane(parent, laneID) {
		return // the corridor now belongs to the order collecting the bin
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
}

// handOffDugLane gives laneID to the order coming to collect the bin this dig
// uncovered, and reports whether it did. False means the lane is the caller's to
// release: this dig uncovered nothing, or nobody is coming for what it uncovered.
//
// ── IT IS ASKED IN TWO HALVES, AND BOTH ARE PHYSICAL ──────────────────────
//
// IS ANYTHING STANDING THERE (DigStillOwesItsTarget) — the same predicate, the
// same one spelling, now with one reader instead of three. A dig whose target
// slot is empty uncovered nothing worth protecting: either the excavation was
// for a slot rather than a bin (a dweller clearing somewhere to drop), or the bin
// has already gone. Nothing to hand over.
//
// IS ANYBODY COMING (CollectorForDigTarget) — asked NOW, through the episode
// that raised the dig, because that is the only tie a buried demand has to the
// bin it is waiting for. This is the half that was missing: without it a corridor
// is held for a demand that walked away, and the release is keyed on an event
// that will never happen.
//
// ── EVERY DOUBT FALLS THE SAME WAY, AND IT IS NOT THE USUAL WAY ───────────
//
// Towards RELEASING. That is the opposite of the rest of this file, and it is
// deliberate: everything above is deciding whether a dig is still WORKING, where
// an early release drives another order into a live excavation. This decides who
// owns an already-finished corridor, where the failure is a lane shut with
// nothing running in it — no robot to finish, no leg to terminate, nothing whose
// completion re-asks. A missed handoff costs the exposure window the ordinary
// mouth gate covers on the demand's next dispatch; a wrong hold costs the lane.
func (d *Dispatcher) handOffDugLane(parent *orders.Order, laneID int64) bool {
	if parent == nil {
		return false
	}

	// ── GATE 2: THE SELF-HANDOFF (§R.91) ──────────────────────────────────
	//
	// A RE-PARENTED DEMAND IS ITS OWN COLLECTOR, and the two questions below
	// cannot ask that. They are written for a FOLDER: "is anything standing at
	// the target" reads DigTargetNode, which only createServiceDigParent writes,
	// and "is anybody coming" looks through the episode for a DIFFERENT order.
	// A demand that took its own excavation records no target and is not somebody
	// else, so both answer no and the corridor is released outright.
	//
	// That is the naked-target window. The excavation ends with the demand's bin
	// standing at an open lane mouth and the demand not yet dispatched, and the
	// slots the dig just emptied are the cheapest shuffle candidates in the group
	// — so the next order wanting one re-buries the bin the dig was run to
	// expose, in the gap between the resume and the demand's own dispatch.
	//
	// WHY THE MODE IS OUTBOUND and not "keep the dig": what has to be excluded is
	// precisely a DROP into that lane. Outbound says that in the vocabulary that
	// already exists — it excludes inbound and dig, and shares with other outbound
	// holders, who can only take bins OUT and so cannot re-bury anything. It also
	// means the demand's own dispatch needs no special case: AcquireLanes for its
	// source lane asks for outbound, finds its own row, and is idempotent.
	//
	// AND IT IS WRITTEN MOUTH-AWARE, so stage 2 cannot break it (§R.96). Today the
	// parent's only row on this lane is the dig row, which this converts. After
	// universal mouth acquisition the parent may arrive already holding the lane —
	// as the SAME row, because a demand digging a lane it holds upgrades rather
	// than doubles (AcquireLanesFor) — so this still finds exactly one row to
	// convert. HandOffLaneToPicker's own picker-already-holds arm covers the
	// remaining shape: it returns true and keeps what is there.
	//
	// SCOPED TO A PARENT THAT STILL OWES ITS OWN WORK. A plain retrieve's fetch is
	// one of its own legs — the bin leaves the lane inside the compound — so there
	// is nothing standing at the mouth to protect and legStillNeedsLane already
	// covered it. That is the same reason DigTargetNode is not written on one.
	if parent.DigTargetNode == "" {
		if !IsCoordinated(parent) {
			return false // its fetch was one of its own legs; nothing is left standing
		}
		handed, hErr := reservations.HandOffLaneToPicker(
			d.db.DB, laneID, parent.ID, parent.ID, digHandoffReservedBy)
		if hErr != nil {
			log.Printf("dig lock: could not convert dig %d's own claim on lane %d to its outbound "+
				"hold (%v) — leaving the claim in place; the compound's own teardown releases it",
				parent.ID, laneID, hErr)
			return true // the row may or may not have moved: do not release on top of a failed write
		}
		if !handed {
			return false
		}
		log.Printf("dig lock: demand %d finished its own excavation of lane %d and kept the corridor "+
			"as an OUTBOUND hold. Nothing may drop into it until the demand has its bin, and the "+
			"hold ends with the demand however it ends", parent.ID, laneID)
		return true
	}

	standing, sErr := d.db.DigStillOwesItsTarget(parent)
	if sErr != nil {
		// The predicate has already chosen the disposition and returned it; this
		// only says so. A misconfigured target releases the lane and is LOUD,
		// because a hold nothing in the world can end is worse than an early one.
		log.Printf("dig lock: %v", sErr)
	}
	if !standing {
		return false
	}

	laneName := fmt.Sprintf("%d", laneID)
	if lane, err := d.db.GetNode(laneID); err == nil && lane != nil {
		laneName = lane.Name
	}

	picker, err := d.db.CollectorForDigTarget(parent)
	if err != nil {
		log.Printf("dig lock: could not look up who is collecting %s after dig %d cleared %s: %v "+
			"(releasing the lane — a corridor held on an unanswered question has no releaser)",
			parent.DigTargetNode, parent.ID, laneName, err)
		return false
	}
	if picker == nil {
		d.recordExcavationWithNobodyComing(parent, laneName)
		return false
	}

	handed, hErr := reservations.HandOffLaneToPicker(d.db.DB, laneID, parent.ID, picker.ID, digHandoffReservedBy)
	if hErr != nil {
		log.Printf("dig lock: could not hand lane %s from dig %d to order %d (%v) — leaving the dig's "+
			"claim in place; the compound's own teardown releases it", laneName, parent.ID, picker.ID, hErr)
		return true // the row may or may not have moved: do not release on top of a failed write
	}
	if !handed {
		return false
	}
	log.Printf("dig lock: dig %d cleared %s and handed it to order %d, which is collecting the bin at "+
		"%s. The corridor is now that order's outbound hold: nothing may drop into %s until it has "+
		"its bin, and the hold ends with that order however it ends",
		parent.ID, laneName, picker.ID, parent.DigTargetNode, laneName)
	return true
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
