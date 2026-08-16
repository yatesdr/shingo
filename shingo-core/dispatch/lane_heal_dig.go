package dispatch

import (
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// WINDOW 3 — THE STAGED-ORDER HEAL DIG.
//
// The last self-heal window, and the one the other three left behind. A robot is
// standing at a lane's mark holding an unsealed waybill. It cannot go in, because
// a bin sits in the corridor in front of the slot it is aimed at. And NOBODY IS
// COMING FOR THAT BIN: it carries no claim, no order names it, no dig is planned
// against it. Every wait in this system is supposed to name the thing that will
// end it; this one could not, because nothing in the plant was ever going to move
// that bin. Three robots dwelt like that for 77 minutes on the lane-stress rig
// (F-11), and the lane behind them held 14 free slots that no store could reach.
//
// ── WHAT THE OTHER WINDOWS ANSWER, AND WHY NONE OF THEM REACHES THIS ──────
//
// Every existing route from "buried" to "a dig" runs through the FINDER or a
// RESOLVER, at PLANNING time:
//
//	fresh-bin retrieve   source_finder's accessibility check → OutcomeReshuffle
//	NGRP full retrieve   group_resolver raises BuriedError
//	held-bin retrieve    window 2's re-check → BuriedForHeldBin
//	complex pickup       the NGRP re-resolve / supply widen → handleComplexBuriedOnReplay
//
// All four are asked BEFORE the order is dispatched. A gate-staged order is past
// all of them: it has been planned, admitted, sent, and its robot is parked at a
// point outside the lane. It will never go through a planner again. So the
// discovery has to happen where the refusal happens — at the gate — or it does
// not happen at all.
//
// ── THE ONE NEW FACT, AND IT IS AN ACTION RATHER THAN A STATE ─────────────
//
// Nothing here invents a status, a column, a releaser kind, or a second dig
// vocabulary. The dig is an ORDINARY COMPOUND created through the ORDINARY DOOR
// in the ORDINARY ORDER — writeCompoundChildren, then BeginReshuffle, then
// AdvanceCompoundOrder — which is what makes every existing law bind without
// being restated here:
//
//   - HARD-CLAIMED BLOCKER → WAIT. The claim CAS in the compound transaction
//     refuses a bin held by an order outside the compound (store.ErrBlockerClaimed),
//     because a hard claim means a robot is already on its way to it and the bin
//     is about to leave on its own. This code does not re-implement that test; it
//     asks, and takes the answer. (It also pre-checks the same fact one read
//     earlier, purely to avoid minting a parent order it is about to cancel.)
//   - SOFT-HELD BLOCKER → THE GUARD'S POLICY, which is that the dig wins and the
//     holder recalculates: "a blocker is positional — the dig has no choice about
//     which bins are in its way" (store/orders.go, stealSoftHold). Unchanged, and
//     deliberately not special-cased here.
//   - NO FREE SHUFFLE SLOT → WAIT. ErrNoShuffleSlot is congestion, never a fault.
//   - A DIG ALREADY OWNS THE LANE → do nothing. Its completion re-drives us.
//
// ── THE RELEASE IS THE EXISTING MACHINERY, WITH ONE HOLE CLOSED ───────────
//
// Nothing new releases the dwelling robot. The dig picks the blocker out of the
// lane (EventBinEnteredTransit), places it elsewhere (EventBlockCompleted,
// EventBinUpdated) and finally goes terminal (EventOrderCompleted) — all four are
// already lane-gate triggers, and the evaluator re-derives from live state.
//
// The hole was at the very end: the dig's LANE LOCK outlives all of those events,
// so the pass they each trigger still refuses with lane-dig-active, and by the
// time the lock drops there is no event left to re-ask. That is closed where the
// lock drops (compound.go unlockLaneForCompound), not here, because it is not
// window 3's bug — a gate-staged order refused behind ANY dig has always had that
// gap, and the traffic on a busy lane is the only thing that has been hiding it.

// healRequest is a gate-staged order that cannot enter, plus the physical facts
// that say a dig is the answer. Built inside the evaluator's per-lane critical
// section and acted on OUTSIDE it — see EvaluateLaneReleases.
type healRequest struct {
	// order is the dweller whose refusal surfaced the wall. It is carried for the
	// log line and for its origin, and NOT for its identity: the dig serves the
	// lane, so whichever candidate noticed first is the one named, and the other
	// dwellers behind the same wall are freed by the same dig.
	order *orders.Order
	// entry is the slot the order's plan works — its dropoff for a store, its
	// pickup for a retrieve. The dig's job is to make this slot reachable.
	entry *nodes.Node
	// blockers is what stands in front of it, shallowest first, as read at the
	// moment of the decision. Re-read by the planner; carried here only so the
	// log can say how big the wall was without asking twice.
	blockers []reshuffleBlocker
}

// mouthHealNeeded reports whether this candidate's refusal is the shape a dig
// fixes: bins physically in the corridor, nobody coming for them.
//
// It is asked of every candidate the pass did not release, whatever the reason it
// was not released, and that generosity is deliberate. The refusal REASON is not
// the question — a Tier-2 park, a lane-occupied refusal and a failed re-bind can
// all be sitting on top of the same wall, and any of them noticing it is enough.
// The question is the PHYSICS, and the physics is read directly.
//
// Four facts, all of them reads:
//
//  1. SOMETHING IS IN FRONT OF THE ENTRY SLOT. No blockers means the wait is
//     traffic, ordering, or the fleet — all of which clear themselves, and none of
//     which a dig would help. This is the common answer.
//
//  2. THE ORDER'S OWN SLOT IS FREE (stores only). If a bin is already sitting in
//     the slot a store is aimed at, the store's problem is not that it is walled —
//     it is that its destination is taken, and the answer to that is a re-bind or
//     another lane, not an excavation. Digging the corridor open would deliver the
//     robot to an occupied slot. Retrieves are exempt because a retrieve's entry
//     slot holding a bin is the entire point.
//
//  3. NOBODY IS COMING FOR ANY BLOCKER. A hard claim is a robot in motion, so the
//     bin is leaving anyway and the correct disposition is the one the floor
//     already has: wait. This mirrors the burial guard's own hard-claims-only rule
//     rather than inventing a second definition of "somebody is coming".
//
//  4. THE LANE IS OTHERWISE QUIET. An occupied lane is being worked, and whoever
//     is inside it may be the releaser. Digging into a corridor that already has a
//     robot in it would take the dig lock and then park the dig's own first leg
//     behind that robot — a wait, not a fault, but a wait that holds a whole lane
//     hostage to make a decision that is one pass away from being free. Occupancy
//     is transient by construction, so this costs at most one firing of latency.
//
// A read that fails answers NO. The disposition for "I could not tell" is to leave
// the order dwelling and re-ask, which is exactly what it was already doing; the
// cost of guessing wrong in the other direction is a robot sent to dig a lane
// whose contents we could not read.
func (d *Dispatcher) mouthHealNeeded(lane *nodes.Node, c gateCandidate) (healRequest, bool) {
	blockers, err := findBuriedBlockers(d.db, c.node.ID)
	if err != nil {
		log.Printf("lane gate: could not read what is in front of %s for order %d: %v — "+
			"not proposing a heal dig", c.node.Name, c.order.ID, err)
		return healRequest{}, false
	}
	if len(blockers) == 0 {
		return healRequest{}, false // fact 1: nothing physical in the way
	}

	if !c.retrieve {
		// fact 2: a store's own slot must be empty, or the wall is not its problem.
		here, bErr := d.db.ListBinsByNode(c.node.ID)
		if bErr != nil {
			log.Printf("lane gate: could not read slot %s for order %d: %v — not proposing a heal dig",
				c.node.Name, c.order.ID, bErr)
			return healRequest{}, false
		}
		if len(here) > 0 {
			return healRequest{}, false
		}
	}

	for _, b := range blockers {
		if !binIsUnclaimed(b.bin) {
			// fact 3: somebody IS coming. This is the floor's ordinary wait and it
			// ends by itself when that robot carries the bin out.
			d.dbg("lane gate: order %d is walled in %s by bin %d, but it is claimed by order %d — waiting",
				c.order.ID, lane.Name, b.bin.ID, *b.bin.ClaimedBy)
			return healRequest{}, false
		}
	}

	occupants, err := reservations.OccupantsOf(d.db.DB, lane.ID)
	if err != nil {
		log.Printf("lane gate: could not read occupants of %s: %v — not proposing a heal dig", lane.Name, err)
		return healRequest{}, false
	}
	for _, occ := range occupants {
		if occ != c.order.ID {
			// fact 4: somebody is in there working. Let them finish.
			return healRequest{}, false
		}
	}

	return healRequest{order: c.order, entry: c.node, blockers: blockers}, true
}

// healLaneMouth fires the dig. Called OUTSIDE the evaluator's per-lane mutex —
// see the note at its call site, and do not move it back inside: creating a
// compound dispatches its first leg synchronously, which emits events, which the
// event bus delivers on this same goroutine to subscribers that call
// EvaluateLaneReleases for this same lane. The mutex is not reentrant.
//
// Every failure arm here leaves the floor exactly as it found it and says nothing
// to the dwelling order. It already carries a cause explaining why it is waiting
// (dcb2c014); replacing that with a cause about a dig that did not happen would
// tell the operator less, not more.
func (d *Dispatcher) healLaneMouth(lane *nodes.Node, req healRequest) {
	res := d.proposeLaneClearDig(lane, req.entry, req.order)
	switch res.outcome {
	case serviceDigStarted:
		log.Printf("lane gate: HEAL DIG %d created for %s — %d blocker(s) in front of %s, none of them "+
			"claimed by anyone, with order %d dwelling at the mark behind them",
			res.parent.ID, lane.Name, res.steps, req.entry.Name, req.order.ID)
	case serviceDigNoGroup:
		log.Printf("lane gate: %s walls order %d but is in no node group, so a dig has nowhere "+
			"to park a blocker — the dweller cannot be healed from here", lane.Name, req.order.ID)
	case serviceDigNoShuffleSlot, serviceDigNothingInTheWay:
		d.dbg("lane gate: cannot clear %s for order %d yet: %v", lane.Name, req.order.ID, res.err)
	case serviceDigBlockerClaimed:
		d.dbg("lane gate: heal dig for %s lost a blocker to a live claim: %v", lane.Name, res.err)
	case serviceDigReadFailed:
		// The dweller already carries a cause; a database stutter changes nothing
		// it should be told. The next pass re-asks.
		d.dbg("lane gate: could not read %s while planning a heal dig for order %d: %v — re-asking",
			lane.Name, req.order.ID, res.err)
	case serviceDigSlotNotInLane, serviceDigUnplannable:
		log.Printf("lane gate: cannot plan a heal dig for %s (order %d is dwelling behind %d blocker(s)): %v",
			lane.Name, req.order.ID, len(req.blockers), res.err)
	case serviceDigLaneBusy:
		// somebody holds this lane; whatever frees it re-drives the gate
	}
}

// serviceDigOutcome names what a lane-clear proposal actually did.
//
// It exists because the two callers owe their requester different things. The
// gate's dweller already carries a cause explaining its wait, so every refusal
// there is silent; a buried COMPLEX demand owes the operator a queue cause and
// has to name which refusal happened. One proposer, two reporting policies —
// rather than two proposers that drift, which is the shape this file's own
// history (and F-04) argues against.
type serviceDigOutcome int

const (
	// serviceDigStarted — the dig exists and its first leg is dispatching.
	serviceDigStarted serviceDigOutcome = iota
	// serviceDigLaneBusy — somebody holds the lane. Whatever frees it re-drives
	// every waiter, so there is nothing to arrange.
	serviceDigLaneBusy
	// serviceDigNoShuffleSlot — congestion. The pool frees as soon as anything
	// anywhere in the group places.
	serviceDigNoShuffleSlot
	// serviceDigParkingHeldByDig — right of way (§R.61): the group has room and it
	// is inside a lane another dig holds. Congestion like the row above, on a
	// narrower releaser — the named dig releasing its lane. THE DIG DID NOT START
	// AND HOLDS NOTHING, which is the whole point of refusing here rather than at
	// the leg: this outcome is reached before createServiceDigParent.
	serviceDigParkingHeldByDig
	// serviceDigGroupCannotAfford — the usable-capacity claim (§R.75/§R.76): the
	// group's dig-free room is real and already owed to the digs running in it.
	// Congestion, on the narrowest releaser of the three — a running dig binding
	// its blocker's destination. LIKE THE ROW ABOVE, NOTHING IS HELD: the refusal
	// happens inside the planner, before any parent is minted or lane taken, which
	// is what makes serialization under famine cheap enough to be the ruled answer.
	serviceDigGroupCannotAfford
	// serviceDigGroupOwesCollection — arm 3 (§R.76): a dig in this group has
	// already uncovered a bin and nobody has collected it. Not a shortage at all —
	// the group may be half empty — but an ORDERING rule, because resolving that
	// collection returns room and starting this excavation would spend it. Nothing
	// is held here either; the refusal is in the planner.
	serviceDigGroupOwesCollection
	// serviceDigNothingInTheWay — the lane moved between the decision and the
	// plan. That is the outcome we wanted; re-ask.
	serviceDigNothingInTheWay
	// serviceDigNoGroup — the lane is in no node group, so a dig has nowhere to
	// park a blocker. Config geometry, not congestion.
	serviceDigNoGroup
	// serviceDigBlockerClaimed — a blocker was claimed while the dig was being
	// written. The holder is carrying it out, which is the releaser.
	serviceDigBlockerClaimed
	// serviceDigReadFailed — the database did not answer while the excavation was
	// being planned. NOT a fact about the lane: the plant is healthy and the same
	// question usually answers on the next sweep. Waits under CauseReadFailed
	// (PLAN §R.45).
	serviceDigReadFailed
	// serviceDigSlotNotInLane — the target slot is a child of no lane, so there is
	// no corridor to dig. A configuration fault: no bin moving anywhere will ever
	// change it, so it fails loudly and names the slot.
	serviceDigSlotNotInLane
	// serviceDigUnplannable — anything else out of the planner. Should now be
	// empty: every remaining path is either a read or the geometry above. Kept
	// fail-closed for whatever a future planner adds.
	serviceDigUnplannable
)

// classifyPlanError maps an excavation planner's error onto the disposition its
// requester gets. Extracted so it can be tested for ALL its inputs at once: three
// of the four planner reads are indistinguishable from the outside (they read the
// same tables through the same store), so the only place the decision can be
// pinned exhaustively is where the decision is made.
//
// ORDER MATTERS. readFailed() answers true for ANY non-nil error that is not
// sql.ErrNoRows — including every sentinel above it — so the named outcomes have
// to be asked first. Get that backwards and a configuration fault parks forever
// under a cause nothing can clear, which is worse than the failure it replaced.
func classifyPlanError(err error) serviceDigOutcome {
	switch {
	case err == nil:
		return serviceDigStarted
	case errors.Is(err, ErrDigHoldsTheParking):
		// ABOVE ErrNoShuffleSlot ON PURPOSE. The two are siblings and this one is
		// the specific: it does not wrap the general one today, and if a later
		// refactor makes it wrap, this arm must still be asked first or every right-
		// of-way refusal reports as a full group and loses the order it was naming.
		return serviceDigParkingHeldByDig
	case errors.Is(err, ErrGroupCannotAffordDig):
		// ALSO ABOVE ErrNoShuffleSlot, for the same reason and with the same
		// hazard: it does not wrap the general one today, and if it ever does this
		// arm must stay first or every affordability refusal reports as a full
		// group — which is precisely the confusion the cause split exists to end.
		return serviceDigGroupCannotAfford
	case errors.Is(err, ErrGroupOwesACollection):
		// ALSO ABOVE ErrNoShuffleSlot, and this one is the least like it of the
		// three: the group is not short of anything. Filing it as a full group
		// would tell an operator to go and make room, when what is needed is for
		// somebody to pick up a bin that is already sitting in the open.
		return serviceDigGroupOwesCollection
	case errors.Is(err, ErrNoShuffleSlot):
		return serviceDigNoShuffleSlot
	case errors.Is(err, ErrNothingInTheWay):
		return serviceDigNothingInTheWay
	case errors.Is(err, ErrSlotNotInLane):
		return serviceDigSlotNotInLane
	case readFailed(err):
		// The same predicate the layer above already uses for the lane read — ONE
		// spelling of "the database did not answer", not a second (law 3).
		return serviceDigReadFailed
	}
	return serviceDigUnplannable
}

// serviceDigResult is the proposal's answer. parent and steps are set only when
// the dig actually started; err carries the planner's or the transaction's own
// error for the caller's log.
type serviceDigResult struct {
	outcome serviceDigOutcome
	parent  *orders.Order
	steps   int
	err     error
}

// proposeLaneClearDig is THE ONE WRITER of a service dig: it mints a plain
// parent, takes the lane, and hands it the excavation that makes `target`
// reachable. It never touches the requester.
//
// ── WHAT A DIG IS, AND WHY THAT MAKES THIS ONE FUNCTION ───────────────────
//
// A dig is a SERVICE TO A LANE. It is not the requester wearing a different
// status: one dig serves every demand waiting behind the same wall, and the
// refusal arms already encode that (a lane already dig-locked means wait — that
// dig's completion re-drives all of them). The requester is carried for its
// ORIGIN, so the cost of digging lands in the episode that caused it, and for
// the log line. It is deliberately NOT carried as an identity: a requester stamp
// would claim 1:1 about a 1:many truth, and would go stale the moment that one
// requester cancelled while the others still needed the lane (PLAN §R.40).
//
// The one exception the ruling draws is the PLAIN retrieve, where the dig's last
// leg IS the demand's whole job — there re-parenting the demand costs nothing and
// saves a hand-off, so planBuriedReshuffle keeps its own shape and does not come
// through here (pinned by TestPlainBuriedRetrieve_KeepsDemandAsItsOwnDigParent).
func (d *Dispatcher) proposeLaneClearDig(lane, target *nodes.Node, requester *orders.Order) serviceDigResult {
	if lane.ParentID == nil {
		// A lane with no group has nowhere to park a blocker. Same terminal-shaped
		// geometry planBuriedReshuffle names, and equally not worth an order.
		return serviceDigResult{outcome: serviceDigNoGroup}
	}
	// ASK THE QUESTION THE ACQUIRE WILL ANSWER, NOT A NARROWER ONE.
	//
	// This was IsLocked, which asks only "does a DIG own this lane". TryLock
	// below is AcquireLanes(ModeDig), and a dig excludes EVERY other owner — so a
	// lane held by an ordinary order passed this guard, got a parent order
	// created for it, and was refused. Every time, because the answer does not
	// change while that order holds its mouth row, and a gate-staged order holds
	// its row until it places.
	//
	// Measured on the lane-stress rig 2026-08-10: LS_C5 held one `outbound` mouth
	// row belonging to a staged order. 16,947 heal parents were created and
	// cancelled against it, no dig ever started, and the plant did nothing else.
	if !d.laneLock.CanTake(lane.ID) {
		return serviceDigResult{outcome: serviceDigLaneBusy}
	}

	// reservations.Anyone IS THE RIGHT ASKER HERE, and it is not a shortcut.
	//
	// The asker exists to exempt the dig's OWN lane from right of way, and this
	// dig has no lane and no order: createServiceDigParent runs below, and the
	// dweller that asked for the dig is deliberately not its parent (see that
	// function's header). Anyone is excluded by every dig, which is exactly the
	// standing of an excavation that owns nothing yet — every dig-held lane in the
	// group belongs to somebody else.
	//
	// The requester is NOT the asker even though it is in hand. A dig is a service
	// to a lane, one dig serves every demand behind the wall (§R.40), so borrowing
	// one requester's exemption would let the dig park inside a lane that requester
	// happens to be digging — a 1:many truth wearing a 1:1 exemption.
	plan, err := PlanLaneMouthClear(d.db, target, lane, *lane.ParentID, reservations.Anyone)
	if err != nil {
		// ORDER MATTERS HERE. readFailed() answers true for ANY non-nil error that
		// is not sql.ErrNoRows — including every sentinel below it — so the named
		// outcomes have to be asked first. Get that backwards and a configuration
		// fault parks forever under a cause nothing can clear, which is worse than
		// the failure it replaced.
		return serviceDigResult{outcome: classifyPlanError(err), err: err}
	}

	// ASK THE QUESTION THE TRANSACTION WILL ANSWER, ONE LAYER DOWN.
	//
	// This is the guard above it, applied to the other refusal. That one was added
	// because 16,947 heal parents were created and cancelled against a lane an
	// ordinary order held; this one is the same shape against a BIN an ordinary
	// order holds, and it was measured on the lane-stress rig 2026-08-13: 38,203
	// parents created and cancelled over 2h15m, a steady 200-290 a minute, every one
	// of them ending "heal dig not started: a blocker was claimed while the dig was
	// being written". Bin 17 was claimed by a frozen complex order for the whole
	// window, so the answer never changed, and each cancellation was itself a
	// terminal event that re-drove the proposer that had just been refused.
	//
	// LAW 1 NAMES IT: a claimed blocker is CONGESTION, not a fault. The requester's
	// wait was already correct — CauseDigBlockerClaimed, releaser "the order holding
	// the blocker finishes carrying it out of the lane" — so nothing about the WAIT
	// changes here. What changes is that discovering it stops costing an order row,
	// a claim transaction and a cancellation.
	//
	// IT LIVES HERE, NOT IN THE CALLERS. mouthHealNeeded asks it as its fact 3 and
	// keeps doing so (it gets to skip the plan entirely), but the two complex
	// callers did not ask at all, and the rig's loop came through one of them. Three
	// copies of one predicate is what law 3 is about, so the ONE WRITER of a service
	// dig asks it once, on the plan it just built, and every caller inherits it.
	//
	// STILL NOT THE AUTHORITY, exactly as binIsUnclaimed's own header says: the
	// claim CAS inside the transaction is, because it tests and claims together. A
	// claim taken between this read and that write still lands on the arm below.
	// This closes the case where the answer was never going to change.
	//
	// THE PROPOSER IS NOT SUPPRESSED, and that is deliberate. The other shape the
	// ruling allowed — do not re-fire until claim state changes — needs state that
	// does not exist, and the cheap fix has to be measured insufficient before the
	// expensive one is earned (law 13). What re-fires now costs two SELECTs and
	// writes nothing, and it no longer feeds itself: the cancellation that was
	// re-driving the next attempt is gone with the parent it cancelled.
	for _, step := range plan.Steps {
		if step.StepType != protocol.StepUnbury {
			continue
		}
		b, bErr := d.db.GetBin(step.BinID)
		if bErr != nil {
			// Fail closed, and as a READ rather than as a fact about the lane — the
			// same disposition every other unreadable answer on this path takes.
			return serviceDigResult{outcome: serviceDigReadFailed, err: bErr}
		}
		if !binIsUnclaimed(b) {
			d.dbg("dispatch: not digging %s for order %d — blocker bin %d is claimed by order %d, "+
				"which is carrying it out", lane.Name, requester.ID, step.BinID, *b.ClaimedBy)
			return serviceDigResult{
				outcome: serviceDigBlockerClaimed,
				err:     fmt.Errorf("blocker bin %d is claimed by order %d", step.BinID, *b.ClaimedBy),
			}
		}
	}

	parent, err := d.createServiceDigParent(lane, target, requester, plan)
	if err != nil {
		return serviceDigResult{outcome: serviceDigUnplannable, err: err}
	}
	if !d.laneLock.TryLock(lane.ID, parent.ID) {
		// NOT NECESSARILY A DIG, and saying so cost a whole investigation. This
		// read "another dig took lane X first" for any refusal, so 16,947 losses to
		// an ordinary order's mouth hold all reported a dig that did not exist —
		// and the reason a reader believes a cancellation is that it names the
		// right thing. AcquireLanes refuses on any other owner's hold.
		d.abandonHealParent(parent, "lane "+lane.Name+" was taken between the check and the claim")
		return serviceDigResult{outcome: serviceDigLaneBusy}
	}
	if err := d.CreateCompoundOrder(parent, plan); err != nil {
		d.laneLock.Unlock(lane.ID, parent.ID)
		if errors.Is(err, store.ErrBlockerClaimed) {
			// Someone claimed a blocker between the pre-check and the transaction.
			// The transaction is the authority and this is its answer: wait. The
			// claim holder is carrying the bin out, which is a lane-clearing event
			// every waiter is already subscribed to.
			d.abandonHealParent(parent, "a blocker was claimed while the dig was being written")
			return serviceDigResult{outcome: serviceDigBlockerClaimed, err: err}
		}
		d.abandonHealParent(parent, "the dig could not be written")
		return serviceDigResult{outcome: serviceDigUnplannable, err: err}
	}
	return serviceDigResult{outcome: serviceDigStarted, parent: parent, steps: len(plan.Steps)}
}

// createServiceDigParent mints the order that OWNS the dig.
//
// ── WHY THE DWELLER CANNOT BE ITS OWN DIG'S PARENT ────────────────────────
//
// Every other dig re-parents the demand that discovered the burial: the buried
// retrieve becomes the compound's parent, goes to `reshuffling`, and comes back
// when the lane is open. That is unavailable here and not by a near miss —
// {staged → reshuffling} is not a legal transition, and it should not become one.
// A staged order is a robot at a point holding an unsealed waybill; moving it to
// `reshuffling` would say the demand is being re-planned while a vehicle is
// committed to it. The ruling that there is no new status cuts the same way: the
// answer is not a new state for the dweller, it is that the dweller does not move
// at all. It keeps dwelling, which is what a gate wait is for, and something else
// does the digging.
//
// ── SO IT IS A PLAIN PARENT, AND THAT IS WHY IT NEEDS NO RESUMPTION ───────
//
// Coordinated:false and no step plan, so the compound-completion arm routes it to
// CompleteCompound — reshuffling → confirmed — rather than ResumeCompound. It has
// no work of its own to come back for; clearing the corridor WAS the work. That is
// the same shape the retired restore-blockers parent had, and it is the reason
// this needs no new arm anywhere in compound.go.
//
// Created at `pending` rather than `queued`, deliberately: `queued` is in the
// acquiring set, so a fulfillment scan landing between the insert and
// BeginReshuffle would pick this row up and try to source a bin for it. `pending`
// is invisible to the scanner, and {pending → reshuffling} is legal.
//
// ENDPOINTS ARE SET, and they earn their keep rather than being decoration:
// LaneIDsForOrder reads them on the terminal event, so the parent's own completion
// re-evaluates the lane it just cleared. One more releaser, for free, from a
// column that had to be filled anyway.
//
// THE ORIGIN IS INHERITED FROM THE DWELLER, by the same rule compound children
// follow: an order created in service of another order inherits its origin AND its
// class. This dig exists because that demand could not move, so its cost belongs
// in that demand's episode. Stamping it no_demand would be cheaper and would be a
// lie — it would quietly move the cost of digging out of the episode that caused
// it, which is the one thing the origin grain exists to prevent.
func (d *Dispatcher) createServiceDigParent(lane, target *nodes.Node, requester *orders.Order, plan *ReshufflePlan) (*orders.Order, error) {
	first, last := plan.Steps[0], plan.Steps[len(plan.Steps)-1]
	parent := &orders.Order{
		EdgeUUID:  uuid.New().String(),
		StationID: requester.StationID,
		OrderType: OrderTypeMove,
		Status:    StatusPending,
		// Names the SLOT rather than "the mark": this parent now serves a gate
		// dweller and a buried complex demand alike, and the one thing true of both
		// is which slot the excavation is for.
		PayloadDesc: fmt.Sprintf("clear %s: %d blocker(s) in front of %s, for order %d",
			lane.Name, len(plan.Steps), target.Name, requester.ID),
		// THE SAME SLOT, WRITTEN WHERE MACHINERY MAY READ IT. PayloadDesc above
		// names it for a human and must never be parsed for it — that is the
		// planUsedExposeMode scar. This column is what the lane's release asks:
		// the claim holds until the bin standing here is collected, so a
		// cancelled claim no longer leaves it exposed.
		//
		// THIS IS THE ONE PLACE IT IS EVER WRITTEN. A service dig is the only
		// shape that owns no retrieve of its own and therefore the only one that
		// needs the debt recorded; a plain buried retrieve re-parents the demand,
		// so its fetch is one of its own legs and legStillNeedsLane already sees
		// it. Stamping this on any other order would make a lock outlive the work
		// it was taken for.
		DigTargetNode: target.Name,
		SourceIntent:  SourceIntentForType(OrderTypeMove),
		OriginID:      requester.OriginID,
		OriginClass:   requester.OriginClass,
	}
	if first.FromNode != nil {
		parent.SourceNode = first.FromNode.Name
	}
	if last.ToNode != nil {
		parent.DeliveryNode = last.ToNode.Name
	}
	if err := d.db.CreateOrder(parent); err != nil {
		return nil, err
	}
	return parent, nil
}

// abandonHealParent retires a heal parent that never became a dig.
//
// It exists because the parent is minted BEFORE the lane lock and the compound
// transaction, and either can refuse. Leaving the row at `pending` would leave an
// order nothing scans, nothing advances and nothing ever ends — a permanent
// no-op row in the orders table that every census and every stall checker would
// have to learn to ignore. Cancelling it says what happened.
//
// {pending → cancelled} is legal, and CancelOrder releases whatever the row might
// have picked up on the way. Nothing else in the system knows this order existed:
// it was never dispatched, never claimed a bin, and never had children.
func (d *Dispatcher) abandonHealParent(parent *orders.Order, why string) {
	d.lifecycle.CancelOrder(parent, parent.StationID, "heal dig not started: "+why)
}

// binIsUnclaimed is the pre-check half of fact 3, kept as a named predicate so the
// decision and the transaction that enforces it can be read side by side.
//
// It is NOT the authority. store.ErrBlockerClaimed out of the compound transaction
// is, because that test and the claim happen together under one lock; this one is
// a read taken earlier and can be stale by the time the write runs. It is here to
// keep the common case from minting an order it would immediately cancel, and the
// stale case is handled where it lands.
func binIsUnclaimed(b *bins.Bin) bool { return b != nil && b.ClaimedBy == nil }
