package dispatch

import (
	"errors"
	"fmt"
	"log"

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
	// serviceDigLaneOccupied — a robot from another order is INSIDE the lane
	// (Hold B), so an excavation would put a second one in there with it.
	//
	// A DIFFERENT FACT FROM serviceDigLaneBusy, which is about the MOUTH. A
	// mouth hold says who may work the corridor; an occupancy row says who is
	// physically in it, and the two have different lifetimes on purpose (see
	// the Hold B note in reservations/mouth.go). A dig can pass the mouth and
	// still be about to drive into an occupied lane, which is exactly the shape
	// this refuses.
	//
	// The releaser is live and already tabled: CauseLaneOccupied — "the robot
	// inside the lane places or picks, releasing its occupancy row".
	serviceDigLaneOccupied
	// serviceDigNoShuffleSlot — congestion. The pool frees as soon as anything
	// anywhere in the group places.
	serviceDigNoShuffleSlot
	// serviceDigParkingHeldByDig — right of way (§R.61): the group has room and it
	// is inside a lane another dig holds. Congestion like the row above, on a
	// narrower releaser — the named dig releasing its lane. THE DIG DID NOT START
	// AND HOLDS NOTHING, which is the whole point of refusing here rather than at
	// the leg: this outcome is reached before createServiceDigParent.
	serviceDigParkingHeldByDig
	// serviceDigEpisodeAlreadyDigging — the demand this would serve already has a
	// service dig running. Congestion of a kind the plant makes for itself, and
	// the releaser is that dig finishing.
	//
	// ── WHY ONE EPISODE GETS ONE EXCAVATION AT A TIME ─────────────────────
	//
	// A buried demand does not wait for its dig: it re-resolves onto whatever it
	// can reach, and a lane under excavation is the one place it cannot. So it
	// picks a second bin, finds THAT buried, and raises a second dig — for the
	// same one bin it needs. The two then compete for the same scarce parking.
	//
	// Measured on the lane-stress rig 2026-08-13: digs 2 and 8, both raised for
	// order 1, ended in a closed mutual hold — dig 2 holding LS_D1 and needing
	// parking only LS_D3 could give, dig 8 holding LS_D3 and needing parking only
	// LS_D1 could give. Every wait individually lawful, the walk closed. That is
	// the standoff the tripwire reported, and the second dig is what made it
	// possible: right of way (§R.61) is a PLAN-TIME rule, and both digs planned
	// before either had taken its lane.
	//
	// It also ends the excavation this plant was already wasting. A demand that
	// re-resolves leaves its first dig digging towards a bin nobody now wants —
	// which is the whole of the dig_target_abandoned population.
	//
	// SCOPED TO THE EPISODE rather than to a requester pointer, for the reason
	// §R.40 gives: a dig serves a LANE and one dig serves every demand behind it,
	// so there is no 1:1 identity to key on. The origin is the tie a dig has, and
	// two live excavations inside one episode is the shape being refused.
	serviceDigEpisodeAlreadyDigging
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
	case errors.Is(err, ErrLaneMouthHeld):
		// The mouth's own refusal, and ABOVE ErrNoShuffleSlot for the reason right
		// of way is: it names a lane, and reporting it as a full group loses the
		// name. Without this arm it falls to serviceDigUnplannable — the terminal
		// verdict — for what is an ordinary wait with a live releaser.
		//
		// ErrMouthUnreadable, its sibling, gets NO arm here on purpose: readFailed
		// below already answers for it and answers the same way. It was given one
		// first, and the mutation that deleted the arm broke nothing — which is
		// law 3's rider catching two spellings of one question before they had a
		// chance to disagree.
		return serviceDigLaneBusy
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
	// blockingDig is the excavation that caused the refusal, when the refusal
	// names one — today the already-digging arm, whose whole content is "another
	// dig is serving this episode".
	//
	// CARRIED STRUCTURALLY, not parsed back out of err. The id is already in the
	// error text ("dig %d is already excavating for this episode"), and reading it
	// back would be the PayloadDesc scar: a human sentence mined for a machine
	// fact, which breaks the day somebody improves the wording.
	blockingDig int64
}

// digOwnership says who OWNS the excavation proposeLaneClearDig is about to
// write, and it is the whole of §R.91's unification expressed as two values.
//
// ── THE RULING ────────────────────────────────────────────────────────────
//
// "All demand that creates a dig should become the parent." A demand that
// cannot move re-parents onto its own excavation, wears `reshuffling` while it
// runs, and resumes through `queued` into its normal lifecycle. That is what
// the plain buried retrieve has always done, and the two complex paths now do
// it too.
//
// ── AND THE ONE CARVE-OUT, WHICH IS PHYSICS ───────────────────────────────
//
// The gate-dweller heal keeps a folder, and not by a near miss: {staged →
// reshuffling} is not a legal transition and should not become one. A staged
// order is a robot at a point holding an unsealed waybill; moving it to
// `reshuffling` would say the demand is being re-planned while a vehicle is
// committed to it. The answer is not a new state for the dweller — it is that
// the dweller does not move at all, and something else digs.
//
// The law-14 restatement the round supplied: "a dig is owned by the demand that
// caused it, UNLESS a vehicle is already committed to that demand — in which
// case the dig is a service to the lane." One predicate, physical, checkable in
// one place. That place is the caller, because only the caller knows whether it
// is holding a dweller.
type digOwnership int

const (
	// digOwnedByRequester — the demand becomes the dig's parent (§R.91). The
	// caller's order is re-parented onto the plan and comes back through
	// `queued` when the corridor is open.
	digOwnedByRequester digOwnership = iota
	// digOwnedByFolder IS DELETED (§R.104). It named the gate-dweller carve-out:
	// a synthetic parent owning the dig because a staged order supposedly could
	// not. A staged order owns its dig without moving at all, so the value has no
	// producer, no consumer, and nothing it could describe.
	//
	// The TYPE survives with one value on purpose. It is the statement that dig
	// ownership is a settled question with one answer — and the place a future
	// second answer would have to argue for itself, rather than appearing as an
	// untyped bool somebody added to a signature.
)

// proposeLaneClearDig is THE ONE WRITER of a lane-clear dig: it takes the lane
// and writes the excavation that makes `target` reachable. Who ends up owning
// it is `own` — see digOwnership.
//
// ── IT USED TO SAY "IT NEVER TOUCHES THE REQUESTER" ───────────────────────
//
// That sentence, and the paragraph under it — "A dig is a SERVICE TO A LANE. It
// is not the requester wearing a different status: one dig serves every demand
// waiting behind the same wall ... The requester is carried for its ORIGIN ...
// It is deliberately NOT carried as an identity: a requester stamp would claim
// 1:1 about a 1:many truth" — is now true of ONE of the two shapes. It is kept
// below, scoped to the folder arm it still describes.
//
// The ruling does not dispute the 1:many observation; it disputes what follows
// from it. A second demand behind the same wall does not need the first demand's
// excavation to be ownerless — it needs a dig on that lane to exist, which the
// one-dig-per-lane mouth claim already guarantees, and it waits on the lane
// rather than on the dig. What ownership buys is that the excavation cannot
// outlive the reason for it: a folder's requester can cancel and leave the
// folder digging towards a bin nobody wants, which is the whole of the
// dig_target_abandoned population.
//
// ── WHAT A FOLDER DIG IS, AND WHY THAT ARM STILL EXISTS ───────────────────
//
// For digOwnedByFolder, a dig is a SERVICE TO A LANE. It is not the requester
// wearing a different status: one dig serves every demand waiting behind the
// same wall, and the refusal arms already encode that (a lane already dig-locked
// means wait — that dig's completion re-drives all of them). The requester is
// carried for its ORIGIN, so the cost of digging lands in the episode that
// caused it, and for the log line. It is deliberately NOT carried as an
// identity: a requester stamp would claim 1:1 about a 1:many truth, and would go
// stale the moment that one requester cancelled while the others still needed
// the lane (PLAN §R.40).
//
// That reasoning is why the carve-out is drawn on PHYSICS rather than on
// preference. Where a vehicle is not yet committed, §R.91 rules the other way
// and the demand takes the excavation as its own.
//
// The PLAIN retrieve re-parents through planBuriedReshuffle rather than through
// here, because its planner is different — it has a target BIN and calls
// PlanReshuffle, where every caller of this function has a target SLOT and calls
// PlanLaneMouthClear. Same ruling, two planners (pinned by
// TestPlainBuriedRetrieve_KeepsDemandAsItsOwnDigParent).
func (d *Dispatcher) proposeLaneClearDig(lane, target *nodes.Node, requester *orders.Order, own digOwnership) serviceDigResult {
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
	//
	// AND IT IS ASKED ON THE REQUESTER'S BEHALF, which is the other half of that
	// story. The row at LS_C5 belonged to THE ORDER THE DIG WAS BEING RAISED FOR:
	// a gate-staged dweller keeps its outbound hold until it places, and the wall
	// it is staged behind is precisely what this dig would clear. Owner-blind,
	// that is a two-cycle with itself — the dweller waits for the dig, the dig is
	// refused because the dweller waits — and no number of cheaper refusals ever
	// gets out of it. The requester's own hold is not an obstacle to the
	// requester's own rescue; everybody else's still is.
	digFor := digAskerFor(requester)
	if !d.laneLock.CanTakeFor(lane.ID, digFor) {
		return serviceDigResult{outcome: serviceDigLaneBusy}
	}

	// AND IS ANYBODY PHYSICALLY IN THERE. The mouth and the inside are two
	// different holds with two different lifetimes, and passing the first says
	// nothing about the second: a complex order takes NO mouth row anywhere
	// (DispatchPreparedComplex never acquires one), so the check above is blind
	// to the commonest robot in the plant. A dig could therefore take the lane
	// with a machine already inside it, and its first leg would be sent in
	// beside that machine.
	//
	// It did not, quite, because admission's arm 2 refuses the LEG on the same
	// row — but by then the dig exists and holds the lane, so the outcome is a
	// locked corridor with a parked excavation in front of it rather than a
	// clean refusal. The refusal belongs where the decision is made.
	//
	// ONE OF THREE CALLERS ASKED THIS. mouthHealNeeded has it as its fact 4 and
	// keeps it (it gets to skip the plan entirely); the two complex callers
	// asked nothing, and the complex arm is the one that carries the traffic.
	// Three copies of one predicate is what law 3 is about, so the ONE WRITER of
	// a service dig asks it once and every caller inherits it — the same
	// disposition, and for the same reason, as the blocker-claim check below.
	//
	// THE REQUESTER IS EXEMPT, matching fact 4 and matching the mouth question
	// one line up: an order's own presence must not refuse its own rescue, or
	// this is the LS_C5 two-cycle again wearing the other hold.
	//
	// SCOPED TO THE DUG LANE, not to the lanes the plan will park blockers in.
	// Those are a real question and they are NOT asked here: the plan does not
	// exist yet at this point, and moving this below the plan would pay for a
	// plan to discover a refusal that was knowable without one. The parking
	// lanes are answered at the leg, by admission's own arm 2. Named rather than
	// left as an apparent oversight.
	//
	// A read that fails REFUSES, like every other guard on this path: "I could
	// not tell whether a robot is in there" must not read as "go ahead".
	occupants, err := reservations.OccupantsOf(d.db.DB, lane.ID)
	if err != nil {
		return serviceDigResult{outcome: serviceDigReadFailed, err: err}
	}
	for _, occ := range occupants {
		if !digFor.Owns(occ) {
			return serviceDigResult{
				outcome: serviceDigLaneOccupied,
				err:     fmt.Errorf("order %d is inside lane %s", occ, lane.Name),
			}
		}
	}

	// ONE EPISODE, ONE EXCAVATION AT A TIME.
	//
	// Asked BEFORE the plan and before the parent, like the two guards around it,
	// because the answer does not change while the other dig runs: planning and
	// minting here would produce an order to cancel on every event, which is the
	// 16,947-cancellation shape this function has already paid for twice.
	//
	// A READ FAILURE DOES NOT REFUSE. Every other guard here fails closed, and
	// this one must not: "I could not tell whether another dig is running" would,
	// fail-closed, stop every dig in the plant on a database stutter. The cost of
	// being wrong in this direction is the standoff the tripwire already watches
	// for; the cost in the other is the whole excavation system.
	//
	// AND IT IS SILENTLY OFF FOR A DEMAND WITH NO EPISODE, which is recorded
	// rather than repaired — see noteUngatedDigProposal. The gate is keyed on the
	// origin, so a requester without one cannot be limited by it: every guard
	// below runs, and this one does not. That is not new and it is not being
	// changed here; closing the origin leak is what switches it on, and doing
	// both in one step would make a dispatch-shaping change arrive disguised as
	// a labelling fix. The count is how we will know how big that population was.
	dig, asked, err := d.db.LiveServiceDigInEpisode(requester.OriginID)
	switch {
	case err != nil:
		d.dbg("dispatch: could not check for a running dig in episode %s (%v) — proceeding",
			requester.OriginID, err)
	case !asked:
		d.noteUngatedDigProposal(requester)
	case dig != 0:
		return serviceDigResult{outcome: serviceDigEpisodeAlreadyDigging,
			blockingDig: dig,
			err:         fmt.Errorf("dig %d is already excavating for this episode", dig)}
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
	// being written" — the wording that measurement was taken under, kept here
	// verbatim so the record and the rig log still line up. The message reads
	// "service dig not started" now; see abandonServiceDigParent for why the old
	// one named one caller of three. Bin 17 was claimed by a frozen complex order for the whole
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

	// ── THE CREATION WINDOW IS MOOT (§R.104) ──────────────────────────────
	//
	// Forty lines stood here describing a race that only a FOLDER could have: the
	// gap between inserting a synthetic parent and writing its legs, in which the
	// parent is childless and the recognition predicate answers wrong about it.
	// Three options were costed, a fourth recommended, and an owner ruling was
	// pending on which to build.
	//
	// None is built and none will be. There is no folder to insert, so there is no
	// gap: a dig's parent is a live order that existed before the dig was thought
	// of. The question closes UNRULED because it stopped being a question, which is
	// the cheapest way any of them has ever closed.

	// ── §R.91: THE DEMAND TAKES ITS OWN EXCAVATION ──────────────────────────
	//
	// No folder is minted at all on this path, which is why it forks BEFORE
	// createServiceDigParent rather than fixing one up afterwards. The requester
	// is already a live order in an acquiring status, so CreateCompoundOrder can
	// write the legs under it and move it into `reshuffling` through the one
	// compound-creation door every other dig uses.
	//
	// THE LANE IS TAKEN IN THE REQUESTER'S OWN NAME, so the childless window the
	// folder arm below documents does not exist here: there is no moment where a
	// parent row exists without legs, because the parent existed already.
	//
	// AND IT COMES BACK THROUGH `queued`. The requester carries StepsJSON, so
	// IsCoordinated is true of it and the compound-completion arm routes it to
	// ResumeCompound — Reshuffling → Queued — rather than confirming it. Its own
	// work is still owed and the scanner re-resolves it against the corridor the
	// dig just opened. No new status edge: both transitions already exist.
	if own == digOwnedByRequester {
		if !d.laneLock.TryLockFor(lane.ID, requester.ID, digFor) {
			return serviceDigResult{outcome: serviceDigLaneBusy}
		}
		if err := d.CreateCompoundOrder(requester, plan); err != nil {
			d.laneLock.Unlock(lane.ID, requester.ID)
			if errors.Is(err, store.ErrBlockerClaimed) {
				return serviceDigResult{outcome: serviceDigBlockerClaimed, err: err}
			}
			return serviceDigResult{outcome: serviceDigUnplannable, err: err}
		}
		return serviceDigResult{outcome: serviceDigStarted, parent: requester, steps: len(plan.Steps)}
	}

	// ── THERE IS NO OTHER ARM (§R.104) ────────────────────────────────────
	//
	// A folder arm stood here: mint a synthetic parent, lock the lane in ITS name,
	// write the legs under it, and abandon it on any of three failures. It served
	// exactly one caller — the gate-dweller heal — on the reasoning that a staged
	// order could not own a dig because {staged → reshuffling} is illegal.
	//
	// It is deleted. A staged order owns its dig without moving at all, so every
	// dig in the system now has a live order for a parent and `own` has one value.
	// The parameter survives as the statement that it does.
	log.Printf("dispatch: BUG: proposeLaneClearDig reached with ownership %v for order %d — every dig "+
		"is owned by the demand that caused it; there is no other kind", own, requester.ID)
	return serviceDigResult{outcome: serviceDigUnplannable,
		err: fmt.Errorf("no dig ownership other than the requester's exists")}
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
