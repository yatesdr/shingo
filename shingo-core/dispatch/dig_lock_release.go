package dispatch

import (
	"encoding/json"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// digHandoffReservedBy tags the mouth row a finished dig hands to the order
// collecting its bin, so a reader looking at a lane's holders can tell an
// ordinary outbound hold from one that came out of an excavation.
const digHandoffReservedBy = reservations.ByDigHandoff

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

	// ── THE HOLDER ITSELF IS IN THE POPULATION (§R.101) ───────────────────
	//
	// This walked the owner's LEGS, which was the whole population while only a
	// dig could hold a lane. §R.101 made every demand a lane holder: a demand that
	// resolves onto an open or shallow bin locks that lane and has no legs at all,
	// so a legs-only walk finds nothing outstanding and drops the lock on the first
	// unrelated exit event — before the demand has been anywhere near its bin.
	//
	// One lock lifecycle, one owner, and the owner is the demand from resolve to
	// collection. Its own row answers the same two questions a leg's does, through
	// the same predicate: its bin is still sitting in the lane (it has not picked
	// yet) or it is dwelling in the lane holding it.
	//
	// The two shapes §R.101a names fall straight out. An OPEN OR SHALLOW resolve
	// has no legs, so the owner's own row is the only thing holding the lane and it
	// clears on the pickup. A BURIED resolve re-parents (§R.91), so the legs hold it
	// through the excavation and the parent's own row holds it afterwards until the
	// target is collected — which is "clears after it achieves the target", with no
	// handoff anywhere, because the collector was the holder all along.
	//
	// FAIL CLOSED, like every read in this file: an unreadable holder keeps the lane.
	holder, hErr := d.db.GetOrder(digOwner)
	if hErr != nil {
		log.Printf("dig lock: could not read lane %d's holder %d while deciding whether it is finished "+
			"with (%v) — keeping the claim", laneID, digOwner, hErr)
		return
	}
	population := open
	if holder != nil && !protocol.IsTerminal(holder.Status) {
		population = append([]*orders.Order{holder}, open...)
	}
	for _, leg := range population {
		if protocol.IsTerminal(leg.Status) {
			continue
		}
		needs, why := d.legStillNeedsLane(leg, laneID)
		if !needs && holder != nil && leg.ID == holder.ID {
			// The holder gets the second question too — see holderStillOwesTheLane.
			needs, why = d.holderStillOwesTheLane(leg, laneID)
		}
		if needs {
			d.dbg("dig lock: order %d keeps lane %d — %d %s", digOwner, laneID, leg.ID, why)
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
	if holder == nil {
		log.Printf("dig lock: lane %d is held by order %d, which does not exist — releasing", laneID, digOwner)
		d.laneLock.Unlock(laneID, digOwner)
		d.EvaluateLaneReleases(laneID)
		return
	}
	parent := holder
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

// handOffDugLane keeps the corridor a demand just excavated, as that demand's own
// OUTBOUND hold, and reports whether it did. False means the lane is the caller's
// to release: this demand's fetch was one of its own legs, so nothing is left
// standing at the mouth to protect.
//
// ── IT LOOKS LIKE A STUB AND IT IS NOT. READ THE PIN FIRST ────────────────
//
// This is §R.91's gate-2 self-handoff. It has been boarded for deletion once
// already and was live when anybody actually checked, so before removing it read
// dig_self_handoff_docker_test.go, which says what breaks without it.
//
// ── WHAT IT PROTECTS: THE NAKED-TARGET WINDOW ─────────────────────────────
//
// Releasing outright ends the excavation with the demand's bin standing at an
// open lane mouth and the demand not yet dispatched, while the slots the dig just
// emptied are the cheapest shuffle candidates in the group — so the next order
// wanting one re-buries the bin the dig was run to expose.
//
// ── THE REMAINING DOUBT FALLS TOWARDS RELEASING ───────────────────────────
//
// That is the opposite of the rest of this file, and it is deliberate: everything
// above is deciding whether a dig is still WORKING, where an early release drives
// another order into a live excavation. This decides who owns an already-finished
// corridor, where the failure is a lane shut with nothing running in it. A missed
// handoff costs the exposure window the ordinary mouth gate covers on the
// demand's next dispatch; a wrong hold costs the lane.
func (d *Dispatcher) handOffDugLane(parent *orders.Order, laneID int64) bool {
	if parent == nil {
		return false
	}

	// ── GATE 2: THE SELF-HANDOFF (§R.91) ──────────────────────────────────
	//
	// A RE-PARENTED DEMAND IS ITS OWN COLLECTOR. The excavation ends with the
	// demand's bin standing at an open lane mouth and the demand not yet
	// dispatched, and the slots the dig just emptied are the cheapest shuffle
	// candidates in the group — so the next order wanting one re-buries the bin
	// the dig was run to expose, in the gap between the resume and the demand's
	// own dispatch.
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
	// covered it.
	if !IsCoordinated(parent) {
		return false // its fetch was one of its own legs; nothing is left standing
	}

	// ── GATE 3: THE HOLDER HAS ALREADY COLLECTED ──────────────────────────
	//
	// THE NAKED TARGET IS THE WHOLE JUSTIFICATION ABOVE, AND IT IS A STATE, NOT A
	// SHAPE. Gate 2 protects a bin standing at an open mouth with its demand not
	// yet dispatched. Once the demand HAS dispatched and lifted, that bin is on a
	// robot: there is nothing at the mouth to re-bury, and converting the dig row
	// hands the corridor to an order that has already left it.
	//
	// What that costs is not theoretical. The row then excludes every inbound
	// comer for the whole transport leg — minutes of driving that has nothing to
	// do with this lane — and it becomes PERMANENT whenever the holder cannot
	// terminate. The single-position swap race supplies exactly that: the holder
	// waits at the station for its evac partner, the partner needs to drop INTO
	// this lane, and the partner is refused by this row. Neither can finish.
	// Observed on the sim 2026-08-28 (main 1a6b6d23): order 142 held Lane_15
	// outbound with its bin already at _TRANSIT while sibling 143 sat in
	// `sourcing` under `lane-held-traffic`, and Lane_15's only reservation was
	// that one row — a corridor shut with nothing inside it.
	//
	// ── WHY NOT ASK legStillNeedsLane, WHICH IS RIGHT THERE ───────────────
	//
	// Because it is INERT here, and believing otherwise already cost one revision.
	// Control only reaches this function because that predicate returned false,
	// and being claim-keyed it returns false for BOTH arrivals — the naked target
	// (no claim yet) and the collected demand (claim lifted out). It cannot tell
	// them apart, so no amount of asking it again will.
	//
	// The discriminator is DISPATCH STATE. swapLegCommittedToFleet is that
	// question already asked and already argued, for this same mistake in the
	// mirror direction, and its `reshuffling` arm rules a mid-dig holder
	// NOT-committed — which is the answer this gate needs too, since a demand
	// still digging has not collected anything. It is inherited rather than
	// re-decided so the two cannot drift apart.
	//
	// The drop-back shape — a plan that picks from this lane and later drops back
	// into it — is NOT released here: holderStillOwesTheLane answers that from the
	// remaining steps before the caller ever reaches this function.
	if swapLegCommittedToFleet(parent) {
		log.Printf("dig lock: demand %d has already collected from lane %d (status %s) — releasing the "+
			"corridor rather than converting its dig row. Nothing is standing at the mouth to protect, "+
			"and a hold taken here would outlast the demand's own visit",
			parent.ID, laneID, parent.Status)
		return false
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

// holderStillOwesTheLane is the holder's half of the question, and it exists
// because legStillNeedsLane asks the RETRIEVE half of it.
//
// "Is my bin still sitting in the lane" is the whole question for a dig leg and
// for an order coming to COLLECT. It is blind to the other direction: an order
// that dug a lane open in order to DROP into it is carrying its bin in the
// gripper, so no bin of its is anywhere in the lane — and it needs the corridor
// more than anyone, because it is about to drive down it.
//
// §R.104's acceptance arm made that shape ordinary: a staged store digs its own
// destination lane open and is appended INTO it the moment the chapter closes.
// Releasing there would open the corridor in the gap between the last blocker
// leaving and the robot arriving, which is the re-burial window the lock exists
// to shut, entered from the inbound side.
//
// ASKED ONLY OF THE HOLDER, not of legs. A dig leg's business in a lane is its
// bin, and legStillNeedsLane is right about it and mutation-pinned; widening that
// predicate would answer a question the legs are not being asked.
//
// FAIL CLOSED, like every read in this file.
func (d *Dispatcher) holderStillOwesTheLane(holder *orders.Order, laneID int64) (bool, string) {
	if holder.DeliveryNode == "" {
		return false, ""
	}
	dest, err := d.db.GetNodeByDotName(holder.DeliveryNode)
	if err != nil {
		return true, "has a destination that could not be resolved"
	}
	if dest == nil {
		return false, ""
	}
	lane, err := d.db.LaneForNode(dest.ID)
	if err != nil {
		return true, "has a destination whose lane could not be resolved"
	}
	if lane != nil && lane.ID == laneID {
		return true, "still owes this lane a drop — its bin is in the gripper, not in the corridor"
	}

	// ── AND DeliveryNode IS ONLY THE LAST STEP (the whole-visit shape) ────
	//
	// A plan may pick from this lane and later drop BACK into it, with its final
	// destination somewhere else entirely. resolveOrderLaneHolds and
	// resolvePlanLaneHolds both dedupe that to ONE row with dig as the stronger
	// mode, on the stated rule that "an order that both picks from and drops into
	// a lane owns it for the whole visit" — and mouth holds are taken once at
	// dispatch, not per step, so that single row is the drop-back's only
	// protection. Reachable through the operator bin-move door.
	//
	// NOTHING ELSE IN THE WALK CAN SEE IT. The drop-back bin is in the GRIPPER, so
	// legStillNeedsLane finds no bin of the holder's in the lane; DeliveryNode
	// names the final destination, which is not this lane. Read the plan or miss
	// it — and missing it means opening the corridor in the gap before the robot
	// drives back down it, the re-burial window entered from the inbound side.
	if owes, why := d.holderOwesLaneALaterDrop(holder, laneID); owes {
		return true, why
	}
	return false, ""
}

// holderOwesLaneALaterDrop reports whether any step still AHEAD of the holder
// drops into laneID.
//
// The position is entryStepIndex — the same answer binForStep uses for "where is
// this order in its plan", rather than a second spelling of it. Steps before it
// are done and say nothing about what the corridor is still owed.
//
// TWO ERROR DIRECTIONS, AND THEY DIFFER ON PURPOSE:
//
//	a DATABASE read that does not answer fails CLOSED, like every other read in
//	this file — an unresolvable node must not shorten a claim.
//
//	an UNPARSEABLE PLAN does not. It is a malformed column rather than a database
//	that is not answering, it will read the same on every retry, and the order
//	carrying it cannot be dispatched by anyone — so holding its corridor forever
//	is the wedge this guard exists to prevent, arrived at from the other side.
//	wantedBin rules the same way on the same column for the same reason.
func (d *Dispatcher) holderOwesLaneALaterDrop(holder *orders.Order, laneID int64) (bool, string) {
	if holder == nil || holder.StepsJSON == "" {
		return false, ""
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(holder.StepsJSON), &steps); err != nil {
		return false, ""
	}
	// POSITION WHEN IT IS KNOWABLE, THE WHOLE PLAN WHEN IT IS NOT.
	//
	// entryStepIndex answers from the wait the order is parked at, so it has no
	// answer for a dispatched order whose plan contains no wait at all — and a
	// pick-then-drop-back plan is exactly that shape. An unlocatable order falls
	// back to reading every step, which is not a guess: the hold was taken ONCE at
	// dispatch under the rule that an order picking from and dropping into a lane
	// owns it for the whole visit, so "does this plan drop here at all" IS the
	// question the row was created to answer.
	from := 0
	if idx, ok := entryStepIndex(holder, steps); ok && idx > 0 && idx < len(steps) {
		from = idx
	}
	for _, step := range steps[from:] {
		if step.Action != protocol.ActionDropoff || step.Node == "" {
			continue
		}
		node, err := d.db.GetNodeByDotName(step.Node)
		if err != nil {
			return true, "has a later drop-off whose node could not be resolved"
		}
		if node == nil {
			continue
		}
		lane, err := d.db.LaneForNode(node.ID)
		if err != nil {
			return true, "has a later drop-off whose lane could not be resolved"
		}
		if lane != nil && lane.ID == laneID {
			return true, "still owes this lane a drop at a LATER step — it picks from this lane and " +
				"drops back into it, and it owns the corridor for the whole visit"
		}
	}
	return false, ""
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
// ── AND IT ASKS ABOUT EVERY BIN THE ORDER HOLDS, NOT ONLY bin_id (§R.101) ─
//
// `leg.BinID` is the whole answer for a dig leg, which moves one bin. It is not
// the whole answer for a DEMAND, and §R.101 made a demand a lane holder: a
// coordinated order claims its bins through the order_bins junction and may hold
// several, so a bin_id-only read would say "nothing of mine is in this lane"
// while one of its other claimed bins sits there — and drop the lock under it.
//
// So the claim is the key. It is the same key findFallbackBinAtSource calls
// canonical for "this order's bin(s)", it is owner-scoped, and it degrades to
// exactly the old behaviour for a leg: one claimed bin, the one in bin_id.
func (d *Dispatcher) legStillNeedsLane(leg *orders.Order, laneID int64) (bool, string) {
	if d.holdsOccupancyThroughDwell(leg.ID, laneID) {
		return true, "is dwelling in it, holding a blocker that has not left"
	}
	claimed, err := d.db.ListBinsByClaim(leg.ID)
	if err != nil {
		return true, "holds bins whose positions could not be read"
	}
	if leg.BinID != nil {
		// bin_id may name a bin the junction does not (a plain order's claim is
		// written before the junction row, and a re-entry reuses it), so both are
		// consulted. Fail closed on a read that does not answer.
		bin, bErr := d.db.GetBin(*leg.BinID)
		if bErr != nil {
			return true, "carries a bin whose position could not be read"
		}
		if bin != nil {
			claimed = append(claimed, bin)
		}
	}
	for _, bin := range claimed {
		if bin == nil || bin.NodeID == nil {
			continue // in transit and not dwelling: the robot is driving out
		}
		lane, lErr := d.db.LaneForNode(*bin.NodeID)
		if lErr != nil {
			return true, "carries a bin whose lane could not be resolved"
		}
		if lane != nil && lane.ID == laneID {
			return true, "has a bin still sitting in it"
		}
	}
	return false, ""
}
