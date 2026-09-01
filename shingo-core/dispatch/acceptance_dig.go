package dispatch

import (
	"fmt"
	"log"
	"time"

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
//  3. NOBODY LIVE IS COMING FOR ANY BLOCKER. A hard claim is a robot in motion,
//     so the bin is leaving anyway and the correct disposition is the one the
//     floor already has: wait.
//
//     THE WORD "LIVE" IS THE 2e CORRECTION, and the old text is kept here because
//     it is what the arm believed: "A hard claim is a robot in motion, so the bin
//     is leaving anyway… This mirrors the burial guard's own hard-claims-only rule
//     rather than inventing a second definition of 'somebody is coming'." It
//     mirrored the burial guard faithfully and asked the question with a bare
//     `ClaimedBy == nil`, which reads IDENTICALLY for a robot driving the bin out
//     and for an order that stopped moving an hour ago. The second one suppressed
//     the dig forever. The question is now put to blockersSpokenFor — §R.98 stage
//     C's liveness-scoped predicate, the same one the dissolve path uses — so a
//     dead claim is not a releaser and does not stand in a dig's way.
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

	spokenFor, lErr := d.blockersSpokenFor(blockers)
	if lErr != nil {
		// Unreadable answers NO, like every other read in this function: the
		// disposition for "I could not tell" is to leave the order dwelling and
		// re-ask, and digging into a lane whose claims we could not read is the
		// expensive direction to be wrong in.
		log.Printf("lane gate: could not read who is coming for the blockers in front of %s for "+
			"order %d: %v — not summoning a dig", c.node.Name, c.order.ID, lErr)
		return acceptanceRequest{}, false
	}
	if spokenFor {
		// fact 3: somebody IS coming, and that somebody is moving. This is the
		// floor's ordinary wait and it ends by itself when that robot carries the
		// bin out.
		d.dbg("lane gate: order %d is walled in %s and at least one blocker has a LIVE claimant — "+
			"waiting", c.order.ID, lane.Name)
		return acceptanceRequest{}, false
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
	res := d.proposeLaneClearDig(lane, req.entry, req.order)
	switch res.outcome {
	case laneClearStarted:
		log.Printf("lane gate: order %d is walled at %s and has summoned its own dig (%d steps). Its "+
			"robot stays at the mark holding its bin; when the chapter closes Core appends its tail "+
			"where it stands", req.order.ID, lane.Name, res.steps)
	case laneClearLaneBusy:
		// Someone else owns the corridor. An ordinary wait with a real releaser,
		// and the row already carries the classifier's cause saying so.
		d.dbg("lane gate: order %d cannot dig %s open — the lane is held", req.order.ID, lane.Name)
	case laneClearBlockerClaimed:
		if res.blockerPromised {
			// THE RANKED REFUSAL IS NOT THE CLAIMED ONE (§7), and it must not reach
			// parkOnClaimedBlocker. That fork asks whether the holder's ROBOT has
			// stopped and files a RESOLVE-BY-HAND recovery row when it has; a
			// promise-holder has no robot, so the liveness question is meaningless
			// and the escalation would call an engineer out on a demand that is
			// simply ahead in the queue. The wait ends when that demand takes its
			// bin or ends — which is what dig-blocker-promised's releaser says.
			log.Printf("lane gate: order %d is walled at %s and yielded the dig — bin %d is promised "+
				"to order %d, whose demand outranks it. No robot is moving that bin; the wait ends "+
				"when order %d takes it or ends",
				req.order.ID, lane.Name, res.blockerBin, res.blockerClaimant, res.blockerClaimant)
			d.setQueueReason(req.order, protocol.QueueStorageRearranging, res.blockerCause,
				QueueParams{Lane: lane.Name, Payload: req.order.PayloadCode,
					HolderOrderID: res.blockerClaimant})
			return
		}
		d.parkOnClaimedBlocker(lane, req, res)
	case laneClearNothingInTheWay:
		d.dbg("lane gate: order %d needs no dig at %s after all — the lane changed under the verdict",
			req.order.ID, lane.Name)
	case laneClearLaneOccupied, laneClearNoShuffleSlot, laneClearParkingHeldByDig,
		laneClearEpisodeAlreadyDigging:
		// Congestion, each with its own live releaser, and the classifier's cause
		// on the row already names the one that applies. Nothing to arrange.
		d.dbg("lane gate: order %d cannot dig %s open yet (%v)", req.order.ID, lane.Name, res.outcome)
	case laneClearNoGroup, laneClearSlotNotInLane, laneClearUnplannable:
		// Geometry: the lane is in no group, the slot belongs to no lane, the plan
		// could not be built. Same arm and same shape as the complex path's
		// (complex_dispatch.go), for the same reason this is LOUD: nothing in the
		// plant will clear any of these on its own — no bin moving anywhere changes
		// which nodes exist — so the dweller's wait has no releaser, and a quiet
		// line here is a robot standing at a mark with nobody told why. §R.45
		// ruled a slot attached to no lane keeps failing loudly WITH THE SLOT
		// NAMED; res.err carries the slot from the planner's sentinel.
		//
		// The order is NOT failed: a committed robot cannot be re-planned, and the
		// next pass re-asks in case the configuration is fixed underneath it. Every
		// arm here leaves the floor exactly as it found it.
		log.Printf("lane gate: order %d is walled at %s (entry %s) and no dig can be planned there "+
			"(%v): %v — the robot is waiting on a corridor nothing is going to open; this is a "+
			"configuration fault, not congestion",
			req.order.ID, lane.Name, req.entry.Name, res.outcome, res.err)
	default:
		// Anything a future outcome adds. Held to the old default's shape — loud,
		// floor untouched — so an unnamed outcome can never fall silent just by
		// existing.
		log.Printf("lane gate: order %d could not dig %s open (%v): %v",
			req.order.ID, lane.Name, res.outcome, res.err)
	}
}

// StoppedBlockerAction is the recovery-action name a dig blocked by a STOPPED
// order is filed under. EXPORTED so the log line and the row it points at cannot
// drift — a reader told to search recovery_actions for a name that is not the one
// written there is worse served than one told nothing.
const StoppedBlockerAction = "dig_blocker_order_stopped"

// parkOnClaimedBlocker is the fork §R.115 ruled: the plan was refused because a
// bin it must move is hard-claimed, and the two worlds behind that one refusal
// get different waits because they have different releasers.
//
//   - THE HOLDER IS MOVING. A robot is carrying the wall out. Ordinary congestion,
//     the wait ends by itself, and it stays exactly as quiet as it always was.
//
//   - THE HOLDER HAS STOPPED WITHOUT TERMINATING. Nothing in the plant is going to
//     move that bin: the claim CAS will not take it from a live order (and must
//     not — R.30's JackUnload maximum is longer than any stall window we run), the
//     orphan sweep only reaches TERMINAL holders, and no timer is allowed to reap
//     it (§R.115 refused the age-based reaper and the automatic dissolve by name).
//     The releaser is a PERSON, so the wait says so and the alarm names who has to
//     act on what.
//
// ── WHY THIS ARM CAN TELL THEM APART AND THE OTHER TWO DOORS CANNOT ───────
//
// Because the acceptance arm asked (fact 3, blockersSpokenFor) before it summoned.
// The complex doors and the plain path do not ask the liveness question, so they
// cannot honestly write the stopped cause and they keep the congestion tag. That
// is a scope statement, not a gap: one spelling of "is somebody coming" exists,
// and only its callers may report what it answered.
//
// ── THE ALARM FIRES ON THE EDGE, NOT ON THE PASS ──────────────────────────
//
// setQueueReason no-ops when the sentence, code and cause are all unchanged, so
// "the row actually changed" IS "this is a new wait", and the alarm rides that
// edge. It has to: this arm re-runs on every lane event, and an alarm per pass is
// the 38,203-row shape this house has already paid for once. What stays
// continuously visible is the WAIT — the operator sentence naming the stopped
// order sits on the board for as long as the condition lasts — while the alarm is
// the durable, timestamped row that says a person was told. A wait that flips to
// another cause and back re-alarms, which is honest: it is a new wait.
func (d *Dispatcher) parkOnClaimedBlocker(lane *nodes.Node, req acceptanceRequest, res laneClearResult) {
	// IS THE HOLDER STOPPED, OR ARE WE THE REASON IT STOPPED? Asked BEFORE the
	// liveness question, because the stall window cannot tell them apart: a holder
	// refused at our own lane gate is never touched, so its updated_at freezes and
	// it ages into "stopped" no matter how healthy it is. §R.115a named that false
	// alarm as the trigger to split this population, and this is the split.
	if d.blockerIsWalledByThisDig(lane, res.blockerClaimant) {
		if d.setQueueReason(req.order, protocol.QueueStorageRearranging, CauseDigBlockerWaitsOnThisDig,
			QueueParams{Lane: lane.Name, HolderOrderID: res.blockerClaimant}) {
			log.Printf("lane gate: !! DEADLOCK on %s: order %d holds this dig's lane and waits for "+
				"order %d to move bin %d — but order %d is refused on THIS lane by THIS dig's own "+
				"lock, so neither can proceed. Order %d is NOT faulty; the lane has to be released "+
				"for it. No stopped-blocker alarm is filed: it would name a healthy order.",
				lane.Name, req.order.ID, res.blockerClaimant, res.blockerBin, res.blockerClaimant,
				res.blockerClaimant)
		}
		return
	}

	stopped, still, err := d.claimantStopped(res.blockerClaimant)
	if err != nil {
		// Unreadable answers "congestion", which is the direction that costs
		// nothing: the wait stays quiet, the next pass re-asks, and we have not
		// called an engineer out on a database hiccup.
		log.Printf("lane gate: could not read whether order %d (holding bin %d in front of order %d) "+
			"is still moving: %v — waiting under the ordinary claimed-blocker cause",
			res.blockerClaimant, res.blockerBin, req.order.ID, err)
		stopped = false
	}
	if !stopped {
		// A robot is already carrying the wall out. The wait ends when it does —
		// which is what the producer's cause says, and this arm does not re-decide
		// it. What this arm decides is the NARROWING below: the holder is not
		// moving, so the wait is not a drive at all.
		d.setQueueReason(req.order, protocol.QueueStorageRearranging, res.blockerCause,
			QueueParams{Lane: lane.Name})
		return
	}

	if !d.setQueueReason(req.order, protocol.QueueStorageRearranging, CauseDigBlockerStopped,
		QueueParams{Lane: lane.Name, StoppedOrderID: res.blockerClaimant}) {
		return // already standing under this wait; the row said so once and still does
	}

	detail := fmt.Sprintf("STOPPED ORDER IS BLOCKING A DIG: order %d has a robot standing at %s and "+
		"cannot get in, because bin %d in front of it is hard-claimed by ORDER %d — and order %d has "+
		"not moved for %s. Nothing automatic will clear this: the claim is not taken from a live "+
		"order, the orphan sweep only releases claims held by TERMINAL orders, and this house does "+
		"not reap holds on a timer. RESOLVE ORDER %d BY HAND — it is usually a configuration fault "+
		"or a genuine breakdown. The moment it moves, is cancelled, or gives up its claim, the next "+
		"lane pass plans the dig and the waiting robot goes. IF ORDER %d TURNS OUT TO BE FINE, that "+
		"is the named watch item: a robot legitimately sitting at a jack-unload can exceed this "+
		"window (measured p50 91s, max 959s), and one confirmed false alarm is the trigger to split "+
		"it from the re-plan window.",
		req.order.ID, lane.Name, res.blockerBin, res.blockerClaimant, res.blockerClaimant,
		still.Round(time.Second), res.blockerClaimant, res.blockerClaimant)

	log.Printf("lane gate: !! %s", detail)
	if err := d.db.RecordRecoveryAction(StoppedBlockerAction, "order", res.blockerClaimant,
		detail, "system"); err != nil {
		log.Printf("lane gate: could not record the stopped blocker %d for order %d: %v",
			res.blockerClaimant, req.order.ID, err)
	}
}

// blockerIsWalledByThisDig reports whether the order holding our blocker is
// itself refused at the lane THIS dig holds — the circular wait behind
// CauseDigBlockerWaitsOnThisDig.
//
// TWO FACTS, BOTH ALREADY ON THE ROWS. The holder's queue_cause is a lane-gate
// refusal produced by an exclusive mouth hold, and the exclusive hold on the lane
// its blocker sits in is OURS. Neither is inferred from timing, which is what
// separates this from claimantStopped's age test: that one asks "has it been
// still for a while", this one asks "is it standing there because of us".
//
// CONSERVATIVE ON EVERY UNREADABLE ANSWER, and the direction is deliberate.
// Returning false hands the decision back to the stall window, which is exactly
// today's behaviour — at worst a healthy order is named, which is the pre-existing
// accepted risk. Returning true on a guess would suppress a REAL stopped-blocker
// alarm and leave a genuine fault unreported, which is strictly worse than the
// false alarm this exists to remove.
func (d *Dispatcher) blockerIsWalledByThisDig(lane *nodes.Node, claimantID int64) bool {
	if lane == nil || claimantID == 0 {
		return false
	}
	claimant, err := d.db.GetOrder(claimantID)
	if err != nil || claimant == nil {
		return false
	}
	// A refusal produced by an exclusive mouth hold. Any other cause — a station
	// wait, a slot shortage, a fleet fault, or none at all — means the holder is
	// stopped for a reason that is not us, and the stall window is the right
	// question for it.
	switch QueueCause(claimant.QueueCause) {
	case CauseLaneDigActive, CauseLaneHeldSource:
	default:
		return false
	}
	// And the exclusive hold on this lane is ours. laneOwnerFor resolves a child
	// to its compound parent, which is who actually owns the row.
	owner, ok := d.laneOwnerFor(claimantID)
	if !ok {
		return false
	}
	excavator, xErr := d.laneLock.ExcavationOwner(lane.ID)
	if xErr != nil || excavator == 0 {
		return false
	}
	// The holder must not be the excavation's own family — a child working inside
	// its parent's dig is not walled by it, and reporting a deadlock there would
	// hide a real stall inside a compound.
	return excavator != claimantID && excavator != owner
}
