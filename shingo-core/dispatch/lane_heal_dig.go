package dispatch

import (
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

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
	if lane.ParentID == nil {
		// A lane with no group has nowhere to park a blocker. Same terminal-shaped
		// geometry planBuriedReshuffle names, and equally not worth an order.
		log.Printf("lane gate: %s walls order %d but is in no node group, so a dig has nowhere "+
			"to park a blocker — the dweller cannot be healed from here", lane.Name, req.order.ID)
		return
	}
	if d.laneLock.IsLocked(lane.ID) {
		return // a dig already owns this lane; its completion re-drives the gate
	}

	plan, err := PlanLaneMouthClear(d.db, req.entry, lane, *lane.ParentID)
	if err != nil {
		// Both known arms are transient and both mean the same thing: not now.
		// ErrNoShuffleSlot is congestion (wait-not-fail, and the pool frees as soon
		// as anything else places); ErrNothingInTheWay means the lane moved between
		// the decision and the plan, which is the outcome we wanted anyway.
		if errors.Is(err, ErrNoShuffleSlot) || errors.Is(err, ErrNothingInTheWay) {
			d.dbg("lane gate: cannot clear %s for order %d yet: %v", lane.Name, req.order.ID, err)
			return
		}
		log.Printf("lane gate: cannot plan a heal dig for %s (order %d is dwelling behind %d blocker(s)): %v",
			lane.Name, req.order.ID, len(req.blockers), err)
		return
	}

	parent, err := d.createHealParent(lane, req, plan)
	if err != nil {
		log.Printf("lane gate: could not create the heal-dig parent for %s: %v", lane.Name, err)
		return
	}
	if !d.laneLock.TryLock(lane.ID, parent.ID) {
		d.abandonHealParent(parent, "another dig took lane "+lane.Name+" first")
		return
	}
	if err := d.CreateCompoundOrder(parent, plan); err != nil {
		d.laneLock.Unlock(lane.ID, parent.ID)
		if errors.Is(err, store.ErrBlockerClaimed) {
			// Someone claimed a blocker between the pre-check and the transaction.
			// The transaction is the authority and this is its answer: wait. The
			// claim holder is carrying the bin out, which is a lane-clearing event
			// this evaluator is already subscribed to.
			d.abandonHealParent(parent, "a blocker was claimed while the dig was being written")
			d.dbg("lane gate: heal dig for %s lost a blocker to a live claim: %v", lane.Name, err)
			return
		}
		d.abandonHealParent(parent, "the dig could not be written")
		log.Printf("lane gate: heal dig for %s could not be created: %v", lane.Name, err)
		return
	}

	log.Printf("lane gate: HEAL DIG %d created for %s — %d blocker(s) in front of %s, none of them "+
		"claimed by anyone, with order %d dwelling at the mark behind them",
		parent.ID, lane.Name, len(plan.Steps), req.entry.Name, req.order.ID)
}

// createHealParent mints the order that OWNS the dig.
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
func (d *Dispatcher) createHealParent(lane *nodes.Node, req healRequest, plan *ReshufflePlan) (*orders.Order, error) {
	first, last := plan.Steps[0], plan.Steps[len(plan.Steps)-1]
	parent := &orders.Order{
		EdgeUUID:  uuid.New().String(),
		StationID: req.order.StationID,
		OrderType: OrderTypeMove,
		Status:    StatusPending,
		PayloadDesc: fmt.Sprintf("clear %s: %d blocker(s) walling order %d at the mark",
			lane.Name, len(plan.Steps), req.order.ID),
		SourceIntent: SourceIntentForType(OrderTypeMove),
		OriginID:     req.order.OriginID,
		OriginClass:  req.order.OriginClass,
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
