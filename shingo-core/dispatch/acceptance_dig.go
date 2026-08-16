package dispatch

import (
	"log"

	"shingo/protocol"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// THE ACCEPTANCE ARM — a staged order digs its own lane open (§R.104).
//
// ── ONE LAW, TWO MOMENTS ──────────────────────────────────────────────────
//
// §R.101a stated the law at RESOLVE: a demand that resolves onto a bin locks
// that bin's lane, and if the bin is buried it summons its own digs and comes
// back through the queue when the chapter closes. §R.104 states the SAME
// SENTENCE at ACCEPTANCE — the moment a staged order's lane step comes due, with
// its robot already standing at the mark:
//
//	CLEAR  → claim the bin, LOCK the lane, THEN append the tail.
//	BURIED → LOCK the lane, summon ITS OWN digs, and the robot keeps standing
//	         at the mark until they finish.
//
// ── THE LAW-14 REVERSAL THIS RESTS ON ─────────────────────────────────────
//
// Two review rounds concluded, 5/5 both times, that this shape was impossible:
// {staged → reshuffling} is not a legal transition, therefore a dwelling order
// that needs a dig must have a synthetic parent dig on its behalf, forever, as a
// physics carve-out.
//
// The rounds were right about RE-PLANNING and wrong about OWNERSHIP. A committed
// robot cannot be re-planned — but it does not need to be. Its plan is intact and
// merely PRECEDED by a dig chapter, so its resume is the SPLICE-APPEND rather
// than the queue round-trip that would need the transition. No status moves at
// either end, so the illegal transition is never reached, and the order is
// recognised as a parent by the only thing that ever mattered: it has children.
//
// ── WHAT THE ROBOT DOES MEANWHILE: NOTHING (§R.104a) ──────────────────────
//
// The dig children dispatch to the fleet like any other work, in parallel where
// the fleet allows, and RDS picks whatever robot is nearest and free. The staged
// robot does not moonlight on the excavation — one mission per vehicle makes
// that a serializer today rather than an optimization, and it stays PARKED as a
// named idea rather than being half-built.

// acceptanceRequest is a staged order the oracle answered BURIED for, plus the
// node its step works and the wall it is behind.
type acceptanceRequest struct {
	order    *orders.Order
	entry    *nodes.Node
	blockers []reshuffleBlocker
}

// acceptanceDigNeeded reports whether this candidate's refusal is the shape a dig
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
func (d *Dispatcher) acceptanceDigNeeded(lane *nodes.Node, c gateCandidate) (acceptanceRequest, bool) {
	blockers, err := findBuriedBlockers(d.db, c.node.ID)
	if err != nil {
		log.Printf("lane gate: could not read what is in front of %s for order %d: %v — "+
			"not summoning a dig", c.node.Name, c.order.ID, err)
		return acceptanceRequest{}, false
	}
	if len(blockers) == 0 {
		return acceptanceRequest{}, false // fact 1: nothing physical in the way
	}

	if !c.retrieve {
		// fact 2: a store's own slot must be empty, or the wall is not its problem.
		here, bErr := d.db.ListBinsByNode(c.node.ID)
		if bErr != nil {
			log.Printf("lane gate: could not read slot %s for order %d: %v — not summoning a dig",
				c.node.Name, c.order.ID, bErr)
			return acceptanceRequest{}, false
		}
		if len(here) > 0 {
			return acceptanceRequest{}, false
		}
	}

	for _, b := range blockers {
		if !binIsUnclaimed(b.bin) {
			// fact 3: somebody IS coming. This is the floor's ordinary wait and it
			// ends by itself when that robot carries the bin out.
			d.dbg("lane gate: order %d is walled in %s by bin %d, but it is claimed by order %d — waiting",
				c.order.ID, lane.Name, b.bin.ID, *b.bin.ClaimedBy)
			return acceptanceRequest{}, false
		}
	}

	occupants, err := reservations.OccupantsOf(d.db.DB, lane.ID)
	if err != nil {
		log.Printf("lane gate: could not read occupants of %s: %v — not summoning a dig", lane.Name, err)
		return acceptanceRequest{}, false
	}
	for _, occ := range occupants {
		if occ != c.order.ID {
			// fact 4: somebody is in there working. Let them finish.
			return acceptanceRequest{}, false
		}
	}

	return acceptanceRequest{order: c.order, entry: c.node, blockers: blockers}, true
}

// summonOwnDigs is the BURIED arm. Called OUTSIDE the evaluator's per-lane mutex
// — creating a compound dispatches its first leg synchronously, which emits, and
// the bus delivers on this goroutine to subscribers that re-enter the evaluator
// for this same lane. The mutex is not reentrant.
//
// ── LOCK → DIG CHAPTER → APPEND, AND THE LOCK IS ALREADY HELD ─────────────
//
// §R.104a makes the ordering law: the first act of ownership is the lock, and an
// append that precedes its lock is a defect. This arm satisfies it twice over.
// The order took its source lane at RESOLVE (§R.101), so by the time its step
// comes due the lock is in hand; and proposeLaneClearDig takes the lane again in
// the requester's own name before writing a single child, which is idempotent on
// a lock the order already holds (admitMouth's `admitIdempotent`) and is the
// acquisition for the case where the step's lane is not the one it resolved into.
//
// Summon-before-lock is outlawed by name: it would leave the corridor open to
// ordinary traffic for the length of an excavation.
func (d *Dispatcher) summonOwnDigs(lane *nodes.Node, req acceptanceRequest) {
	res := d.proposeLaneClearDig(lane, req.entry, req.order, digOwnedByRequester)
	switch res.outcome {
	case serviceDigStarted:
		log.Printf("lane gate: order %d is walled at %s and has summoned its own dig (%d steps). Its "+
			"robot stays at the mark holding its bin; when the chapter closes Core appends its tail "+
			"where it stands", req.order.ID, lane.Name, res.steps)
	case serviceDigLaneBusy:
		// Someone else owns the corridor. An ordinary wait with a real releaser,
		// and the row already carries the classifier's cause saying so.
		d.dbg("lane gate: order %d cannot dig %s open — the lane is held", req.order.ID, lane.Name)
	case serviceDigBlockerClaimed:
		// A robot is already carrying the wall out. The wait ends when it does.
		d.setQueueReason(req.order, protocol.QueueStorageRearranging, CauseDigBlockerClaimed,
			QueueParams{Lane: lane.Name})
	case serviceDigNothingInTheWay:
		d.dbg("lane gate: order %d needs no dig at %s after all — the lane changed under the verdict",
			req.order.ID, lane.Name)
	case serviceDigLaneOccupied, serviceDigNoShuffleSlot, serviceDigParkingHeldByDig,
		serviceDigEpisodeAlreadyDigging:
		// Congestion, each with its own live releaser, and the classifier's cause
		// on the row already names the one that applies. Nothing to arrange.
		d.dbg("lane gate: order %d cannot dig %s open yet (%v)", req.order.ID, lane.Name, res.outcome)
	default:
		// Unplannable: no shuffle slot, bad geometry, a write that failed. The
		// order keeps the cause the classifier wrote and the next pass re-asks —
		// every failure arm here leaves the floor exactly as it found it.
		log.Printf("lane gate: order %d could not dig %s open (%v): %v",
			req.order.ID, lane.Name, res.outcome, res.err)
	}
}
