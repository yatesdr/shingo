package dispatch

import (
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
	if err != nil {
		return // the row could not be read: keep whatever is there
	}
	if digOwner == 0 {
		// NO DIG — but there may be a converted handoff row that nothing else
		// will ever look at again. This is the only walk that visits a lane on
		// an ordinary exit event, so it is the only place that can notice.
		d.maybeReleaseStrandedHandoff(laneID)
		return
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
	// CHANGE OF OWNER, because the collector was the holder all along.
	//
	// That last clause used to read "with no handoff anywhere", and it was read as
	// a statement about reachability rather than about ownership. handOffDugLane
	// does run in this shape — it converts the holder's own dig row to the holder's
	// own outbound row. What does not happen is the corridor changing hands.
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

// maybeReleaseStrandedHandoff gives the converted naked-target row the releaser
// it never had.
//
// ── THE ROW LEAVES THE MACHINERY THE MOMENT IT IS CREATED ─────────────────
//
// handOffDugLane converts the dig row to an OUTBOUND row so the uncovered bin
// cannot be re-buried before its demand dispatches. That conversion also takes
// the row OUT OF THIS WALK: the walk opens on DigOwner, which reads mode='dig',
// and returns the moment there is none. From then on the row's only releasers
// are its owner's own block progress (releaseOrderLaneFor) and its
// terminalization.
//
// For a demand that dispatches shortly that is exactly right, and the row lives
// seconds. For one that goes back to the scanner and RE-RESOLVES ONTO A BIN
// SOMEWHERE ELSE, nothing looks at the row again and the corridor stays shut for
// the rest of that order's life. Flip 2's own header records five of those
// wedging five lanes on the lane-stress rig 2026-08-13 — "no live order wanted
// any of the five slots they were holding for" — and the same shape wedged the
// sim on 2026-08-28: order 202 held Lane_03 as `dighandoff` while its own route
// had re-resolved to SMN_001 → SMN_008, and the evac that needed a slot in
// Lane_03 queued behind it until three cells had stopped.
//
// It was survivable before only by accident. A second blocker-out pass used to
// read "the dig row is already gone" as permission to release, and ReleaseLane
// is mode-blind, so it deleted the fresh handoff row — the race graphite-finch
// found and 9466f086 closed. That bug was sweeping these rows away constantly.
// Closing it was right; it exposed the hole underneath, which is this one.
//
// ── WHAT DECIDES IT, AND WHY IT IS NOT A ROUTE READ ───────────────────────
//
//	RESHUFFLING           the gap the row exists for. The dig is still finishing
//	                      and the demand has not been handed back to the scanner.
//	                      KEEP — this is gate 2 doing its job.
//
//	DISPATCHED OR BEYOND  its own per-visit release and its terminalization own
//	                      the row now, exactly as HandOffLaneToPicker's doc says.
//	                      KEEP.
//
//	A BIN OF ITS OWN      legStillNeedsLane — it is coming for something still in
//	  STILL IN THE LANE   here, or dwelling in it. KEEP.
//
//	PRE-DISPATCH WITH     it has not re-resolved yet and has not chosen. This is
//	  NO CLAIM ANYWHERE   still the gap. KEEP. ("No claim → release" would be
//	                      wrong for exactly this reason: a naked target holds no
//	                      claim BY DEFINITION, so that rule would drop the row one
//	                      scanner tick after the parent left `reshuffling` and
//	                      undo gate 2 across most of its window.)
//
//	ANYTHING ELSE         RELEASE. Two shapes reach here and both are stranded:
//	                      an order that went back through the scanner and resolved
//	                      onto a bin SOMEWHERE ELSE, and one that has COLLECTED
//	                      and driven off with its bin.
//
// ── THE COLLECTED ARM IS GATE 3, ASKED A SECOND TIME ──────────────────────
//
// It was tempting — and wrong, and the sim said so — to treat "dispatched or
// beyond" as KEEP on the grounds that the demand's own per-visit release owns
// the row from then on. That is what HandOffLaneToPicker's doc claims, and it
// holds only while the lane is ON THE DEMAND'S ROUTE: releaseOrderLaneFor fires
// on block progress AT A NODE IN THIS LANE, so an order whose route never
// returns here produces no such progress and the row lives to terminalization.
//
// Measured (run C, 2026-08-28): order 260 sat `in_transit`, "Waiting for partner
// robot", bin already at _TRANSIT, holding Lane_15 as `dighandoff` — while its
// own route was SMN_023 → ALN_001, which never touches Lane_15. Gate 3 had
// already ruled that shape at conversion time ("a demand that has already
// collected releases its corridor"); a row converted while the demand was still
// a naked target simply never got asked again. So this asks it.
//
// No step list is read: later visits belong to the lane's own state.
//
// FAIL CLOSED on every read, like the rest of this file. A row kept one pass too
// long costs a wait; a row released while its demand is walking to the bin is
// the re-burial this whole seam exists to prevent.
func (d *Dispatcher) maybeReleaseStrandedHandoff(laneID int64) {
	rows, err := reservations.ActiveMouthRows(d.db.DB, laneID)
	if err != nil {
		return
	}
	for _, h := range rows {
		if h.ReservedBy != digHandoffReservedBy {
			continue
		}
		holder, hErr := d.db.GetOrder(h.OrderID)
		if hErr != nil {
			return // unreadable holder keeps the lane
		}
		if holder == nil {
			d.releaseStrandedHandoff(laneID, h.OrderID, "its owner no longer exists")
			continue
		}
		if holder.Status == StatusReshuffling {
			continue // the gap the row exists for: the dig is still finishing
		}
		if needs, _ := d.legStillNeedsLane(holder, laneID); needs {
			continue // a bin of its own is still in here, or it is dwelling in it
		}
		claimed, cErr := d.db.ListBinsByClaim(h.OrderID)
		if cErr != nil {
			return // unreadable claims keep the lane
		}
		if len(claimed) == 0 && !swapLegCommittedToFleet(holder) {
			continue // pre-dispatch and undecided: it has not chosen yet
		}
		d.releaseStrandedHandoff(laneID, h.OrderID, strandedWhy(holder))
	}
}

// strandedWhy names which of the two stranded shapes this is, so the log says
// what happened rather than that something did.
func strandedWhy(holder *orders.Order) string {
	if swapLegCommittedToFleet(holder) {
		return "its owner has already collected and driven off with its bin, so nothing is coming " +
			"for what this corridor was protecting (gate 3's rule, asked a second time)"
	}
	return "its owner went back through the scanner and resolved onto a bin somewhere else"
}

// releaseStrandedHandoff drops one stranded handoff row and wakes the lane, the
// same three wakes the ordinary release arm performs.
func (d *Dispatcher) releaseStrandedHandoff(laneID, owner int64, why string) {
	if err := reservations.ReleaseLaneHandoff(d.db.DB, owner, laneID); err != nil {
		log.Printf("dig lock: could not release order %d's stranded handoff row on lane %d: %v",
			owner, laneID, err)
		return
	}
	log.Printf("dig lock: released order %d's stranded handoff hold on lane %d — %s",
		owner, laneID, why)
	d.EvaluateLaneReleases(laneID)
	d.RedriveHeldCompoundLegs(laneID)
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
	// SCOPED TO A COORDINATED PARENT. A plain retrieve's fetch is one of its own
	// legs — the bin leaves the lane inside the compound — so there is nothing
	// standing at the mouth to protect and legStillNeedsLane already covered it.
	//
	// THE HEADER HERE USED TO SAY "a parent that still owes its own work", and this
	// check does not ask that. IsCoordinated is provenance (hasEvacLeg); it says
	// nothing about whether the parent has already been to the lane. The gap
	// between the two readings is exactly where the handoff leak lived: every
	// coordinated demand passed this arm, including the ones that had already
	// collected and left. "Still owes its own work" is now a real gate, and it is
	// gate 3 below.
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
	// ── AND `faulted` IS ADDED HERE, NOT THERE ────────────────────────────
	//
	// The shared predicate's `default` arm means "not committed", and the two
	// callers read that answer with OPPOSITE consequences: for the swap hold it
	// means keep waiting, which is safe; here it means convert, which is the leak.
	// So the one status where they must differ is fixed locally. Widening
	// swapLegCommittedToFleet would be wrong for the swap caller, which genuinely
	// wants a faulted sibling to read not-committed because it may yet recover and
	// do its part.
	//
	// `faulted` is POST-DISPATCH BY CONSTRUCTION — every inbound edge to it comes
	// from acknowledged, dispatched, in_transit or staged — so a faulted holder
	// standing here has been handed to the fleet and, having passed the claim
	// walk, has its bin out of this lane. A jammed aisle is jammed by the ROBOT,
	// not by a row; an empty aisle held for a faulted order is the same leak
	// wearing a different status, and a worse one, because `faulted` sits outside
	// the runtime-stuck population and nothing alarms on it.
	//
	// THE BIN THAT COMES BACK IS ALREADY COVERED. A dropped load set down again in
	// this lane is a bin sitting in this lane, which legStillNeedsLane sees on the
	// next evaluation — before control can reach this function at all. The order
	// re-owing the lane after a recovery is likewise a fresh acquire, not this row.
	//
	// The drop-back shape — a plan that picks from this lane and later drops back
	// into it — is NOT released here: holderStillOwesTheLane answers that from the
	// lane's own state before the caller ever reaches this function.
	if swapLegCommittedToFleet(parent) || parent.Status == StatusFaulted {
		log.Printf("dig lock: demand %d has already collected from lane %d (status %s) — releasing the "+
			"corridor rather than converting its dig row. Nothing is standing at the mouth to protect, "+
			"and a hold taken here would outlast the demand's own visit",
			parent.ID, laneID, parent.Status)
		return false
	}

	outcome, hErr := reservations.HandOffLaneToPicker(
		d.db.DB, laneID, parent.ID, parent.ID, digHandoffReservedBy)
	if hErr != nil {
		log.Printf("dig lock: could not convert dig %d's own claim on lane %d to its outbound "+
			"hold (%v) — leaving the claim in place; the compound's own teardown releases it",
			parent.ID, laneID, hErr)
		return true // the row may or may not have moved: do not release on top of a failed write
	}

	// ── PAST THIS POINT THE LANE IS NEVER THE CALLER'S TO RELEASE ─────────
	//
	// All three outcomes report true, and that is the correction rather than a
	// shortcut. Two of them are "nothing was converted", and the old bool said so
	// in a way the caller read as PERMISSION — whereupon its mode-blind release
	// deleted whatever row it found for this owner, including the outbound row a
	// concurrent pass had just created. See reservations.HandOff.
	//
	// The two gates above are where false comes from now, and both of them decide
	// BEFORE touching the row. That is the honest shape: a release is authorized
	// by what the holder is, never by what a write happened to find.
	switch outcome {
	case reservations.HandOffConverted:
		log.Printf("dig lock: demand %d finished its own excavation of lane %d and kept the corridor "+
			"as an OUTBOUND hold. Nothing may drop into it until the demand has its bin, and the "+
			"hold ends with the demand however it ends", parent.ID, laneID)
	case reservations.HandOffNoDigRow:
		log.Printf("dig lock: lane %d had no dig row left for demand %d when the handoff ran — a "+
			"concurrent pass already converted or released it, under the same lane lock. Standing "+
			"down: releasing on top of that would delete whatever it just put there", laneID, parent.ID)
	case reservations.HandOffPickerNotCollector:
		log.Printf("dig lock: demand %d holds lane %d INBOUND, so it is dropping into the lane rather "+
			"than collecting from it and the corridor is not its hold to take. The dig row is gone; "+
			"its own inbound row stays", parent.ID, laneID)
	}
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
// ── IT READS LANE STATE, AND IT DOES NOT READ THE ROUTE ───────────────────
//
// This briefly grew a second arm that parsed the holder's remaining steps looking
// for a LATER dropoff back into a lane it had already picked from. That arm is
// gone, and the ruling behind its removal is the useful part:
//
//	A LATER VISIT IS THE LANE'S OWN BUSINESS. Every shape the plant actually
//	builds announces itself in state that is already read here. An IN-LANE MOVE
//	names the lane in DeliveryNode — the arm above. A RELAY parks its bin IN the
//	lane between visits, claimed, where legStillNeedsLane sees it. A PICK-ONLY
//	visit is over at the lift. Three shapes, three lane-state answers, and the
//	release timing falls out of each without anybody consulting a step list.
//
//	THE SHAPE THAT NEEDED THE ROUTE HAS NO PRODUCER. Pick from a lane, leave,
//	come back later, finish elsewhere: not the operator bin-move door (two steps,
//	so its in-lane form finishes in the lane), not the relay (its dropoff comes
//	BEFORE its pickup), not anything five readers or the owner could name. It is
//	refused at plan time instead — see LaneRevisitError — so if a door ever starts
//	emitting one we hear about it rather than mis-holding a corridor quietly.
//
//	AND A ROUTE READ CANNOT BE MADE EXACT HERE ANYWAY. "Which steps are still
//	ahead" has no reliable answer at this seam: wait_index advances when a segment
//	is APPENDED, one gate ahead of the robot, so a positioned scan skips the
//	segment being executed, and the fallback — read every step — cannot tell a
//	completed drop from a pending one and holds the lane, as a DIG, through
//	transport it owes nothing to. Both regimes were wrong in opposite directions.
//
// A RE-ENTRY IS NOT THIS FUNCTION'S PROBLEM either. An order that comes back to a
// lane acquires it again through the ordinary mouth gate, with its own admission
// and its own spliced wait; it does not ride a row taken for an earlier visit.
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
