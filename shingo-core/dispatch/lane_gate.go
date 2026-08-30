package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/service"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// THE MARK IS THE GATE'S WHOLE CONFIGURATION. A lane is gated if, and only if,
// it has a point for its robots to dwell at (PropLaneGatePoint on the LANE) —
// one fact, set by the person who knows the aisle, and the thing they set IS the
// thing that makes it true.
//
// ── THE MARK BUYS TWO THINGS AND THIS COMMENT USED TO NAME ONE ────────────
//
// It said the mark "chooses only the WAITING ROOM: park before dispatch, or
// drive out and dwell at a point", and closed with "Both are safe; the mark
// chooses which one the waiting happens in." The first half is true. The last
// sentence is not, and it cost a diagnosis round: it reads as though the two
// arms differ in comfort, when they differ in WHEN THE DESTINATION IS CHOSEN.
//
//	THE WAIT    where a robot stands while Core decides. This is the part the
//	            mark is named for, and the part that needs a real map point.
//	THE ORACLE  rebindGatedDropoff — the dropoff slot resolved AT RELEASE
//	            against the lane as it stands, through the owner-aware selector.
//	            It is reachable from the gated arm and from nowhere else.
//
// An UNMARKED lane binds its slot at dispatch and drives. Nothing re-asks. The
// robot arrives minutes later at a lane that has changed underneath it, and the
// two outcomes are `dropoff-occupied` and the air bubble — three stores each
// correctly picking the three deepest free slots, then ARRIVING in whatever
// order the fleet gives them, whoever lands first sealing the rest in. Neither
// is a collision, so "both are safe" was true in the narrow sense the paragraph
// above means it; both are also stale, which is what the old sentence hid.
//
// Collision safety genuinely does not live on the mark. The physical questions —
// is a foreign dig holding this lane, is a robot inside it, is the target
// reachable — are asked on every lane-entry path unconditionally, and occupancy
// rows are written unconditionally. That is why an unmarked lane is safe to run
// and still wrong about where the bin goes.
//
// So enablement is per-lane and incremental. A lane goes gated the day a human
// places its mark, and rollback is clearing it (robots already dwelling complete
// under the old rules). No marks existed at either plant as of 2026-08-31, which
// means THE ORACLE HAD NEVER RUN IN PRODUCTION — every lane-bound store at every
// plant has always used a slot chosen before the drive.
//
// (A three-valued `lane_enforcement` group property and a hardcoded
// `laneShareBasePriority` both stood here and are gone. Neither was ever set or
// applied at a plant — verified live 2026-08-08, zero rows on both cores — and
// this branch has never been deployed to one, so neither leaves a note under law
// 16. The ledger carries them.)

// laneGateReservedBy tags the mouth rows the gate writes. It used to say "for
// forensics" and that is no longer all it is: §R.101 gave the SOURCE hold the
// dig mode, so this tag is the only thing separating a demand that owns the lane
// it sources from and a reshuffle that is excavating one. See
// reservations.IsExcavation.
const laneGateReservedBy = reservations.BySourceLock

// laneWaitPoint returns the map point a lane's robots dwell at while Core decides
// whether they may enter, or "" when the lane has none.
//
// It is the whole of the gate's configuration, and it decides two things, not
// one — see the header. A non-empty value means: ship lane-bound orders UNSEALED
// to this point and append their tail when the lane is safe, which is also the
// only moment the dropoff slot is re-resolved (rebindGatedDropoff). Empty means:
// choose the slot before dispatch, park the order if the answer is no, and never
// ask again — so the slot the robot drives to is as old as the drive.
func (d *Dispatcher) laneWaitPoint(laneID int64) string {
	return d.db.GetNodeProperty(laneID, PropLaneGatePoint)
}

// laneIsGated reports whether Core stages robots at this lane rather than parking
// their orders before dispatch. Derived, never configured separately: the
// existence of the mark IS the answer.
//
// It is ALSO the answer to "does this lane get a late-bound dropoff", because the
// two ride one flag. If you are here asking the second question, that coupling is
// the thing to know: the header says why, and it is not obviously the right
// design — it is simply the design.
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

// laneHold is a (lane, mode) an order must hold to work that lane: THE FULL LANE
// LOCK on the lane it picks from, inbound when it drops into one.
//
// ── THE SOURCE HOLD IS A LOCK, NOT A DOORWAY (§R.101) ─────────────────────
//
// The source used to be ModeOutbound, which shares: two orders both picking from
// one lane were admitted together. §R.101 generalizes arm 2's dig rule to every
// order — "a demand that resolves onto a bin owns that lane until the bin leaves
// by its mover" — so the source hold is ModeDig, which excludes everyone.
//
// One law for lanes, and it buys a whole class of churn back. The shape it makes
// unconstructible is just-moved-then-dug: a store places a bin in front of a
// target another demand has already resolved onto, and a dig is then paid for to
// un-place it. Under the lock the store is refused the lane and picks another,
// before a drive is burned rather than after.
//
// THE DESTINATION IS UNCHANGED, deliberately. Destination-lane locking is not
// ruled and belongs to the destination-owner batch; a dig's destination keeps the
// dwell's late choice. Only the SOURCE side is the demand's to own.
//
// The serialization this adds is not new waiting. A lane is single-file, so two
// orders picking from it were always going to take turns — the lock moves the
// turn-taking from the mouth to the resolve, which is cheaper by exactly one
// drive. §R.101 examined that cost and accepted it.
type laneHold struct {
	laneID int64
	mode   reservations.Mode
}

// resolveOrderLaneHolds computes the mouth holds a plain order needs from its
// source and destination nodes: the source lane is outbound (the order picks
// there), the destination lane is inbound (the order drops there). A node that is
// not a direct lane slot (LaneForNode == nil) contributes no hold.
//
// ── EVERY LANE CONTRIBUTES (§R.95/§R.96 stage 2, a1) ──────────────────────
//
// This used to skip any lane without a gate mark, and the paragraph here said so
// plainly, ending: "an unmarked plant yields zero holds, so the gate is a no-op
// at both plants today." §R.95's census turned that sentence from a caveat into
// a finding — zero holds on unmarked lanes at Springfield and Hopkinsville,
// complex never acquiring at all. The mouth machinery existed, was correct, and
// had never once run where it mattered.
//
// The skip conflated two different things the mark answers. WHERE A ROBOT WAITS
// is a configuration fact: a lane with a gate point stages the robot at the mark,
// a lane without one parks the order before dispatch. That is still `laneIsGated`
// and it still decides staging, at `enteredAtDispatch` and `entryDeferredToGate`.
// WHETHER THE MOUTH IS SEQUENCED AT ALL is not a configuration fact — it is the
// physics of a single-file lane, true of every lane on both plants whether or not
// anyone drew a point on the map.
//
// So the mouth is universal and the staging stays configured. An unmarked lane
// now yields its holds and an order refused on one parks pre-dispatch, which is
// exactly what an unmarked lane has always done with every other refusal.
func (d *Dispatcher) resolveOrderLaneHolds(sourceNode, destNode *nodes.Node) ([]laneHold, error) {
	var holds []laneHold
	seen := map[int64]int{} // laneID -> index into holds; one entry per lane
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
		// ── ONE OWNER, ONE LANE, ONE HOLD — THE STRONGER MODE WINS ─────────
		//
		// The source arm and the dest arm can resolve to the SAME lane: an
		// order that picks from a lane and drops back into it, same-lane
		// source+destination. Undeduped, that is two rows for one owner on one
		// lane — the incoherent state admitMouth exists to refuse — and the
		// refusal it raises does NOT wrap ErrReservationConflict (it is the
		// "still a caller bug" arm, and AcquireLanesFor returns its error
		// before the verdict switch translates it), so acquireOrderLanes'
		// conflict rollback never fires: the first mode's row is committed in
		// its own transaction, the error path returns without releasing it,
		// and every retry meets the order's own committed row. The lane is
		// wedged by the order that needs it, permanently.
		//
		// The same rule resolvePlanLaneHolds states a plan walk's version of:
		// an order that both picks from and drops into a lane owns it for the
		// whole visit, and dig is the stronger mode. Reachable through the
		// operator bin-move door, which is how it surfaced.
		if i, ok := seen[lane.ID]; ok {
			if holds[i].mode != reservations.ModeDig && mode == reservations.ModeDig {
				holds[i].mode = reservations.ModeDig
			}
			return nil
		}
		seen[lane.ID] = len(holds)
		holds = append(holds, laneHold{laneID: lane.ID, mode: mode})
		return nil
	}
	// The SOURCE lane is locked, not merely queued at (§R.101). See laneHold.
	if err := add(sourceNode, reservations.ModeDig); err != nil {
		return nil, err
	}
	if err := add(destNode, reservations.ModeInbound); err != nil {
		return nil, err
	}
	return holds, nil
}

// LaneRevisitError is the TRIPWIRE on a plan shape nothing builds: a pickup in
// lane L, a LATER dropoff back into L, and a final destination outside L.
//
// ── WHY IT IS A REFUSAL AND NOT A HOLD ────────────────────────────────────
//
// Mouth holds are taken ONCE, at dispatch, and this walk deduplicates a lane
// named twice into one row on the rule that an order picking from and dropping
// into a lane "owns it for the whole visit". That rule is honest for the two
// shapes the plant actually emits — an in-lane MOVE (which finishes in the lane,
// so the visit and the order end together) and a RELAY (whose bin stands parked
// IN the lane between visits, claimed, where the lane's own state carries it).
//
// It is not honest for this one. The robot picks, LEAVES the lane entirely, and
// comes back later — so a single row spans a departure, and the only truthful
// lifetimes for it are both wrong: release at the lift and the corridor is open
// when the robot drives back down it; hold to terminalization and the corridor is
// shut through unrelated transport, which is the leak this whole seam was opened
// to close.
//
// Reading the remaining ROUTE to tell those apart was tried and removed: the read
// was wrong in both its regimes, and five reviewers plus the owner could not name
// a door that emits the shape. So the shape is refused instead — loudly, by name,
// at plan time. If a future door ever starts building one, this says so the first
// time rather than mis-holding a corridor quietly and forever.
type LaneRevisitError struct {
	Lane        string // the lane the plan picks from and later returns to
	PickupNode  string // the step it picks at
	DropoffNode string // the LATER step it drops back at
	FinalNode   string // where it actually finishes, outside the lane
}

func (e *LaneRevisitError) Error() string {
	return fmt.Sprintf("plan picks from %s at %s, later drops back into it at %s, and finishes at %s "+
		"outside the lane — a lane mouth is held once for one visit and cannot span the robot leaving "+
		"and returning. No order door builds this shape; fix the plan that did",
		e.Lane, e.PickupNode, e.DropoffNode, e.FinalNode)
}

// resolvePlanLaneHolds is resolveOrderLaneHolds over a whole coordinated plan:
// every pickup step's lane is a source (the full lock), every dropoff step's is a
// destination (inbound).
//
// ── COMPLEX HAD NEVER ACQUIRED ANYTHING (§R.95's census, §R.101's scope) ──
//
// admitComplexLanes asked admission's physical questions and then took no holds
// at all, so the entire mouth mechanism was reachable only from the plain path.
// That is not a small gap: complex is the bulk of both plants' lane traffic, and
// the tree says so at complex_dispatch.go's ungated arm. §R.101's rule is written
// about complex orders — a complex order resolving on an open or shallow bin
// proceeds as normal, the lane locks, and the lane clears on the pickup — so a
// source lock that only plain orders take is the rule applied to the smaller
// half of the plant.
//
// ONE PLAN CAN NAME ONE LANE TWICE and legitimately: a step picks from a lane the
// same plan later drops into, or two pickups come from one lane. The holds are
// deduplicated with the STRONGER mode winning, because an order that both picks
// from and drops into a lane owns it for the whole visit — and because inserting
// two rows for one owner on one lane is the incoherent state admitMouth exists to
// refuse.
func (d *Dispatcher) resolvePlanLaneHolds(steps []resolvedStep) ([]laneHold, error) {
	strongest := map[int64]reservations.Mode{}
	var order []int64 // deterministic output; map iteration is not

	// ── THE TRIPWIRE'S EVIDENCE, GATHERED ON THIS SAME WALK ───────────────
	//
	// It rides here rather than in a pass of its own because this loop is already
	// resolving every step's node and lane, and those are database reads on every
	// dispatch tick — a second walk would double them to answer a question this
	// one has in hand.
	//
	// Recorded BEFORE the gated-lane skip below, deliberately: whether a lane
	// defers its hold to its mark is configuration, and the shape is malformed
	// either way. A tripwire that a mark can switch off is not a tripwire.
	pickedFrom := map[int64]string{} // lane -> the node this plan picked at
	var revisit *LaneRevisitError
	revisitLane := int64(0)
	finalNode, finalLane, finalResolved := "", int64(0), false

	for _, step := range steps {
		mode := reservations.ModeInbound
		switch step.Action {
		case protocol.ActionPickup:
			mode = reservations.ModeDig
		case protocol.ActionDropoff:
		default:
			continue
		}
		// WHERE THE PLAN ENDS is the last actionable step, which is the same
		// answer extractEndpoints gives DeliveryNode — so the tripwire and
		// holderStillOwesTheLane's surviving arm read one definition of "final
		// destination" between them. Reset per step: only the last one counts.
		finalNode, finalLane, finalResolved = step.Node, 0, false
		if step.Node == "" {
			continue
		}
		node, err := d.db.GetNodeByDotName(step.Node)
		if err != nil || node == nil {
			// A step whose node does not resolve is admission's problem, not the
			// mouth's — and it already ran. Skipping matches admitPlan's own arm on
			// the same read, so the two walk the same steps.
			continue
		}
		lane, err := d.db.LaneForNode(node.ID)
		if err != nil {
			return nil, err
		}
		finalResolved = true
		if lane == nil || lane.ParentID == nil {
			continue
		}
		finalLane = lane.ID
		if mode == reservations.ModeDig {
			if _, seen := pickedFrom[lane.ID]; !seen {
				pickedFrom[lane.ID] = step.Node
			}
		} else if at, seen := pickedFrom[lane.ID]; seen && revisit == nil {
			// A DROP BACK INTO A LANE THIS PLAN ALREADY PICKED FROM. Not yet a
			// refusal — an in-lane move is exactly this and is legitimate. What
			// decides it is where the plan ENDS, which is not known until the walk
			// is over.
			revisit = &LaneRevisitError{Lane: lane.Name, PickupNode: at, DropoffNode: step.Node}
			revisitLane = lane.ID
		}
		// ── A GATED LANE'S ENTRY IS NOT THIS MOMENT ───────────────────────
		//
		// This caller declares entryWhenGated, and the declaration is about the
		// whole entry decision, not only about admission's half of it. A
		// coordinated create bound for a MARKED lane stops at the mark: the fleet
		// is sent `preWait` and nothing else, and the tail that actually puts the
		// robot in the corridor is appended later, when the evaluator says the
		// lane is safe. Taking the lane here would refuse the order BEFORE
		// DISPATCH and waste every step in front of the lane — which is the exact
		// thing the splice exists to stop: a compound of 5, 7 or 10 steps must not
		// queue on a lane block. It does all the work it can up to the lane.
		//
		// It is the same rule enteredAtDispatch states for occupancy and
		// entryDeferredToGate states for admission, now said a third time for the
		// mouth. On a marked lane the sequencing is the GATE's — one robot admitted
		// at a time, occupancy recorded at the append — so nothing is unsequenced
		// by deferring; the waiting simply happens at the mark instead of in the
		// queue. On an unmarked lane, which is every lane at both plants, there is
		// no mark to wait at and the hold below is the whole of the sequencing.
		if d.entryDeferredToGate(skipsForComplexEntry, lane) {
			d.dbg("lane mouth: order's entry to %s stops at its mark, so no hold is taken here; the "+
				"tail append is the moment it goes in", lane.Name)
			continue
		}
		prev, seen := strongest[lane.ID]
		if !seen {
			order = append(order, lane.ID)
			strongest[lane.ID] = mode
			continue
		}
		if prev != reservations.ModeDig && mode == reservations.ModeDig {
			strongest[lane.ID] = mode
		}
	}

	// ── AND NOW THE TRIPWIRE CAN ANSWER ───────────────────────────────────
	//
	// A revisit is only the ghost when the plan LEAVES the lane for good: if it
	// finishes there, the visit and the order end together, the single row is
	// honest for the whole of it, and holderStillOwesTheLane's DeliveryNode arm
	// releases it at the drop. That is the in-lane move, and it is ordinary.
	//
	// FAIL OPEN ON AN UNRESOLVED ENDING. If the last actionable step's node did
	// not resolve, "where does this plan finish" has no answer, and a tripwire is
	// the wrong thing to fire on a question it cannot ask. Admission owns an
	// unresolvable node and has already run.
	if revisit != nil && finalResolved && finalLane != revisitLane {
		revisit.FinalNode = finalNode
		return nil, revisit
	}

	holds := make([]laneHold, 0, len(order))
	for _, laneID := range order {
		holds = append(holds, laneHold{laneID: laneID, mode: strongest[laneID]})
	}
	return holds, nil
}

// laneAdmission is one acquireOrderLanes decision, carrying the cause the park
// needs.
//
// ── THE REFUSAL AND ITS LABEL ARE ONE READ ────────────────────────────────
//
// Both callers used to refuse here and then call causeForLaneHolds separately,
// which re-queries ActiveMouthRows for every lane the order wanted. That is a
// SECOND read of the same state, taken after the first one had already decided —
// and lane mouths are exactly the state that moves between two reads, because
// the thing that refuses this order is another order finishing with the lane.
// The two answers disagreeing is invisible at a park: the row would carry a
// cause describing a hold that had already cleared, and the operator would be
// sent to a lane nobody is in.
//
// One refusal, one classification, one value. Same shape as the swap gate's
// verdict and the storage-dropoff verdict: the arm that made the decision is the
// only thing that can name it.
type laneAdmission struct {
	admitted bool
	cause    QueueCause // zero when admitted
	err      error
}

// acquireOrderLanes takes every hold the order needs, all-or-nothing across
// modes, and on a refusal names the cause the caller parks under. A non-nil
// verdict.err is a transient DB failure; the caller requeues the order in
// sourcing under WAITING_FOR_SLOT, per Rule 1. With no holds (the common,
// unconfigured case) it admits immediately.
//
// AcquireLanes is per-mode all-or-nothing; an order picking from one lane and
// dropping into another needs one call per mode, so a conflict on the second
// mode rolls back the first via the order-scoped ReleaseLanesByOwner. All rows
// are owned by the order, so that release reclaims exactly this gate's takes.
//
// THE CLASSIFICATION IS TAKEN AFTER THE ROLLBACK, which is where it always
// happened — the callers ran it later still. Classifying before would read this
// order's own rows as competing traffic and label its wait after itself.
func (d *Dispatcher) acquireOrderLanes(orderID int64, holds []laneHold) laneAdmission {
	if len(holds) == 0 {
		return laneAdmission{admitted: true}
	}
	byMode := map[reservations.Mode][]int64{}
	for _, h := range holds {
		byMode[h.mode] = append(byMode[h.mode], h.laneID)
	}
	// ── THE ACQUIRE IS TAKEN FOR THE ORDER ITSELF (§R.101) ────────────────
	//
	// Anyone would be wrong now that the source hold is ModeDig. A demand that
	// re-enters — parked on a destination and retried next tick, or resuming out
	// of its own excavation — already holds this lane, and an owner-blind acquire
	// would have it refuse its own lock. Asking on its own behalf makes the
	// re-entry idempotent (its row is its own) and lets a weaker row it already
	// holds upgrade rather than collide, which is admitMouth's `admitUpgrade` arm
	// and exactly the plain re-parented shape.
	//
	// laneOwnerFor, not the raw id: a compound leg's holds belong to its parent,
	// so a leg working inside the lane its own demand locked must not be refused
	// by it. That routing already exists for every other lane question.
	owner, _ := d.laneOwnerFor(orderID)
	asker := reservations.AskerFor(orderID, owner)
	for mode, lanes := range byMode {
		// ── THE MARK EXEMPTION APPLIES HERE TOO, AND IT USED NOT TO ───────
		//
		// This site passed nil, on the reasoning that §R.101 made an ordinary
		// demand's SOURCE hold ModeDig so the mode alone does not make it an
		// excavation, and that the exemption was a statement about excavations.
		// That was right about the vocabulary and wrong about the physics, and a
		// whole sim run paid for the distinction.
		//
		// GATED SIM, 2026-08-31, ~4,158 orders in. Order 22 gate-staged at
		// Lane_08's mark holding that lane's inbound row. Order 23, a RETRIEVE,
		// wanted the bin one slot deeper; its §R.101 source hold takes Lane_08 in
		// ModeDig and was refused here — `lane-held-traffic`. Order 22's own
		// re-bind was then refused BECAUSE order 23 was coming for that bin:
		// storing at the mouth would seal it in. Each was the other's only
		// releaser and the plant stopped.
		//
		// That is the order-22 deadlock with the requester's costume changed. The
		// arm protects the CORRIDOR, and a corridor does not care whether the
		// robot asking for it is an excavation compound or a plain retrieve — only
		// that it is coming in, and that the holder is standing outside. So every
		// ModeDig acquire gets the exemption, and the set's own membership rule is
		// untouched: staged at this lane's GROUP's wait points.
		//
		// Computed only for the dig-mode lanes, because that is the only mode
		// admitMouth consults it for, and it costs a scan of the live gate
		// candidates. Resolved per lane, since a coordinated plan's lanes can
		// belong to different groups (reservations.StagedOutsideByLane).
		var staged reservations.StagedOutsideByLane
		if mode == reservations.ModeDig {
			staged = stagedAtMarkByLane(d.db, lanes...)
		}
		if aErr := reservations.AcquireLanesFor(d.db.DB, orderID, mode, asker, staged, laneGateReservedBy, lanes...); aErr != nil {
			if errors.Is(aErr, reservations.ErrReservationConflict) {
				// Roll back any holds taken for an earlier mode so the acquire is
				// all-or-nothing across the order's lanes. The freed lanes are
				// discarded deliberately: nothing was ever admitted into them, so no
				// dweller is waiting on this release — the caller parks and the
				// ordinary triggers re-ask.
				_, _ = reservations.ReleaseLanesByOwner(d.db.DB, orderID)
				return laneAdmission{cause: d.causeForLaneHolds(orderID, holds)}
			}
			return laneAdmission{err: aErr}
		}
	}
	return laneAdmission{admitted: true}
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
		if _, err := reservations.AcquireOccupancy(d.db.DB, orderID, laneID); err != nil {
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
//
// It carries the take's own report out (AcquireOccupancy's first return):
// took=true means THIS call inserted the row, took=false means the row was
// already there — a dweller's release re-taking the source-lane row its own
// dispatch took. The append's failure rollback gives back only what it took,
// so the two outcomes have to be distinguishable at the release site.
func (d *Dispatcher) takeLaneOccupancyByID(orderID, laneID int64) (took bool, err error) {
	if laneID == 0 {
		return false, nil // not a lane-owned wait — nothing to record
	}
	took, err = reservations.AcquireOccupancy(d.db.DB, orderID, laneID)
	if err != nil {
		return false, fmt.Errorf("take occupancy for order %d on lane %d: %w", orderID, laneID, err)
	}
	return took, nil
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
// Precedence is deliberate and is the fix. It used to read:
//
//	a readable dig anywhere  -> lane-held-dig        (definite, and it wins)
//	otherwise any failed read -> lane-held-unreadable (we cannot rule a dig out)
//	otherwise                 -> lane-held-traffic    (definite)
//
// and it now reads:
//
//	a readable EXCAVATION     -> lane-held-dig        (definite, and it wins)
//	otherwise any failed read -> lane-held-unreadable (we cannot rule one out)
//	otherwise a source lock   -> lane-held-source     (definite, §R.101)
//	otherwise                 -> lane-held-traffic    (definite)
//
// ── WHY THE MIDDLE ROW HAD TO BE ADDED (§R.101) ───────────────────────────
//
// The old table had "a dig" meaning mode='dig', which was the same thing as an
// excavation until §R.101 gave every demand's SOURCE hold that mode. After it,
// every ordinary retrieve holding the lane it sources from was reported as a
// dig — and the paragraph below about a dig stall and a traffic stall being
// investigated differently is exactly why that matters: the engineer goes
// looking for a reshuffle that was never planned. It also made lane-held-traffic
// nearly unreachable, since a source lock outranked it on every lane a demand
// had resolved onto.
//
// The kind comes off reserved_by (reservations.IsExcavation), a fact every
// writer already stamped and nothing read.
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
	sourceLocked := false
	for _, r := range reads {
		if r.err != nil {
			unreadable = true
			continue
		}
		for _, row := range r.rows {
			if row.OrderID == orderID || row.Mode != reservations.ModeDig {
				continue
			}
			if reservations.IsExcavation(row.ReservedBy) {
				return CauseLaneHeldDig
			}
			// A dig-mode row that is not an excavation is §R.101's source lock.
			// Recorded rather than returned: an excavation on a LATER lane is the
			// stronger fact and must still win, the same way it wins over an
			// unreadable sibling.
			sourceLocked = true
		}
	}
	if unreadable {
		return CauseLaneHeldUnreadable
	}
	if sourceLocked {
		return CauseLaneHeldSource
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
	adm := d.acquireOrderLanes(order.ID, holds)
	if adm.err != nil {
		return false, "", "", adm.err
	}
	if adm.admitted {
		return true, "", "", nil
	}
	// THE CAUSE COMES FROM THE VERDICT, not from a second read taken here.
	return false, adm.cause, d.laneDisplayName(holds), nil
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
		if order != nil {
			// SHADOWED: "holds no bin" is the folder's permanent state as well as
			// the fault this error was written for.
			owns, oerr := d.db.OrderOwnsNoCargo(order.ID)
			service.NoteFolderShadow(service.FolderSiteBuriedForHeldBin, order.ID, owns, oerr)
		}
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
//
// THE SECOND RETURN IS "I ANSWERED", and it exists for the one caller that sits
// beside a destructive write. The owner-scoped releases can take the fallback
// safely — aimed at the wrong order their WHERE matches nothing — but the fleet
// demote door turns this into "may I DELETE this order's lane rows", and there
// the fallback answers YES for a leg whose parent simply could not be read. That
// tears the corridor out from under a live dig. Beside a destructive write an
// unreadable answer is "no", and the next pass re-asks.
func (d *Dispatcher) laneOwnerFor(orderID int64) (int64, bool) {
	o, err := d.db.GetOrder(orderID)
	if err != nil || o == nil {
		log.Printf("lanegate: could not read order %d to resolve its lane owner: %v (assuming it "+
			"owns nothing it has not proved it owns)", orderID, err)
		return orderID, false
	}
	if o.ParentOrderID == nil {
		return orderID, true
	}
	return *o.ParentOrderID, true
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
	owner, _ := d.laneOwnerFor(orderID)
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
	owner, _ := d.laneOwnerFor(orderID)
	node, err := d.db.GetNodeByDotName(dropNodeName)
	if err != nil || node == nil {
		return
	}
	if err := d.releaseOrderLaneFor(owner, node); err != nil {
		log.Printf("lanegate: release hold for order %d (owner %d) on dropoff at %s: %v",
			orderID, owner, dropNodeName, err)
	}
}
