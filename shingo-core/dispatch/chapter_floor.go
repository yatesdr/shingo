package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/store/orders"
)

// THE STALLED-CHAPTER WATCHDOG — the floor §R.91 owed and the one that ACTS.
//
// ── THE GAP (law 8) ───────────────────────────────────────────────────────
//
// Before §R.91 a complex demand whose source was buried waited in `queued`,
// inside IsAcquiring, swept by the fulfillment scanner every 60 seconds, while a
// synthetic folder wore `reshuffling` on its behalf. §R.91 made the demand ITSELF
// wear `reshuffling` — which is the one non-terminal status no floor covers.
// AdvanceStuckReshuffleParents sweeps the half of it where every child is
// terminal; the half where a leg is still open had nothing. A machine-owned wait
// population with no periodic floor is a law-8 violation, and this one was found
// by a reviewer reading the run rather than by anything in the tree.
//
// ── AND IT RESOLVES RATHER THAN POINTS (§R.99) ────────────────────────────
//
// The lane floor next door says, in its own header, that a floor DECIDES NOTHING
// — it re-triggers level-triggered machinery and contains no policy, because a
// floor that decided anything would be a second answer to a question the event
// path already answers. That sentence still governs the lane floor. It does not
// govern this one, and the difference is a ruling rather than an oversight:
//
//	"the watchdog RESOLVES: dissolve-and-re-plan is the default disposition
//	 wherever re-planning is safe … the oracle adapts, the demand re-queues,
//	 the scanner re-resolves … I really don't like seeing the corpses."
//
// So this asks ONE question of a chapter that has stopped — can it be safely
// re-planned now? — and there are exactly three answers.
//
// ── THE THREE ANSWERS, AND WHY THE TEST IS A FACT AND NOT A TIMER ─────────
//
//	NO VEHICLE IS COMMITTED. Every open leg is still pre-fleet: Core has not
//	handed any of them to a robot. Nothing is moving, so nothing can be
//	disturbed, and what has gone stale is a PLAN. DISSOLVE — through the same
//	dissolveCompound the dispatch path calls, so there is one dissolver with two
//	triggers rather than two policies that drift. The demand re-queues with its
//	cause and the scanner re-resolves against the world as it now is (law 7: the
//	reserve is a plan).
//
//	A VEHICLE IS COMMITTED. Some open leg is a mission the fleet holds and has
//	not finished. A robot may be carrying a bin down a lane right now, and no
//	elapsed time on Core's side is evidence about that — R.30 measured mid-order
//	JackUnload waits at p50 91s and max 959s, so any bound tight enough to catch
//	a dead mission terminates normal production. This is a WAIT, and it gets what
//	a wait owes: a cause and a live releaser on the parent, so the board says
//	which leg and what will end it.
//
//	THE FLEET IS DONE AND CORE NEVER HEARD. An open leg's mission reached a
//	terminal vendor state while the leg stayed non-terminal — a dropped event,
//	not congestion. Nobody is coming, but the robot has already acted, and
//	dissolving would cancel a leg whose bin may be somewhere Core has not
//	written down yet. ALARM. This is the residue §R.99 leaves for a human, and
//	it is the only arm here that neither moves nor waits.
//
// TERMINATION: NEVER, on any path (§R.98, refused 4/4). The dissolve arm cancels
// the dig's own legs — which it already does by design — and the demand behind
// them survives every branch.

// stalledChapterAction is the recovery-action name the residue is filed under.
const stalledChapterAction = "chapter_stalled_unresolvable"

// chapterStallWindow is how long a whole family may go without a single write
// before this pass treats the chapter as stopped.
//
// Three floor ticks, the same number and the same reasoning as
// claimantStallWindow: one tick cannot tell "between passes" from "stopped", and
// the two windows answer the same kind of question about the same kind of row, so
// two different numbers would only invite the question of why.
//
// IT IS NOT A DEADLINE. Nothing here terminates when it expires; it decides when
// to ASK, and the answer is then taken from facts about the fleet rather than
// from the clock. A chapter with a live mission stays waiting no matter how long
// this window is.
const chapterStallWindow = claimantStallWindow

// chapterVerdict is what one stalled chapter turned out to be.
type chapterVerdict int

const (
	chapterReplannable chapterVerdict = iota // no vehicle committed — dissolve
	chapterWaiting                           // a mission is live — visible wait
	chapterResidue                           // the fleet finished and Core never heard
)

// ChapterSweepResult counts one pass. Every field is zero on a healthy plant.
type ChapterSweepResult struct {
	Dissolved int
	Waiting   int
	Residue   int
}

// SweepStalledChapters is one watchdog pass over the reshuffling-with-an-open-leg
// population. Returns what it did, which is nothing at all on a healthy plant.
func (d *Dispatcher) SweepStalledChapters() ChapterSweepResult {
	var out ChapterSweepResult
	cutoff := clock.Now().UTC().Add(-chapterStallWindow)
	parents, err := d.db.ListStalledChapters(cutoff, 100)
	if err != nil {
		log.Printf("chapter floor: could not read the stalled set: %v (skipping this pass; the next one retries)", err)
		return out
	}
	for _, parentID := range parents {
		// ── `staged` IS TWO POPULATIONS, AND THIS WATCHDOG WANTS ONE OF THEM ──
		//
		// The SQL below now admits both statuses, because a §R.104 parent digs
		// its own lane open from `staged` and is exactly the wedge this pass
		// exists for: its chapter can go quiet with no vehicle committed, and
		// nothing else will dissolve it — the evaluator skips it (an open
		// chapter is a running dig) and the dissolver used to refuse it ("not
		// reshuffling").
		//
		// But `staged` is also the OPERATOR's word for a robot staged at a
		// station wait, and those rows are the abandon sweep's: AbandonStuckOrders
		// selects `staged` and exempts exactly IsGateStaged, so the ownership
		// line is already drawn there in one spelling. This filter reads the
		// same predicate off the same row, and a staged parent this pass does
		// not recognize falls through untouched.
		parent, pErr := d.db.GetOrder(parentID)
		if pErr != nil || parent == nil {
			log.Printf("chapter floor: could not reload parent %d: %v (holding)", parentID, pErr)
			continue
		}
		if parent.Status == protocol.StatusStaged && !IsGateStaged(parent) {
			continue // an operator-staged row: AbandonStuckOrders owns it
		}
		legs, lErr := d.db.ListChildOrders(parentID)
		if lErr != nil {
			log.Printf("chapter floor: could not read the legs of compound %d: %v (holding)", parentID, lErr)
			continue
		}
		switch verdict, leg := classifyStalledChapter(legs); verdict {
		case chapterReplannable:
			d.replanStalledChapter(parentID)
			out.Dissolved++
		case chapterWaiting:
			d.markChapterWaiting(parentID, leg)
			out.Waiting++
		case chapterResidue:
			d.recordChapterResidue(parentID, leg)
			out.Residue++
		}
	}
	return out
}

// classifyStalledChapter answers the one question, and returns the leg the answer
// is about so the record can name it.
//
// A VEHICLE OUTRANKS EVERYTHING. The scan does not stop at the first open leg: a
// chapter with one pre-fleet leg and one live mission is WAITING, and reading them
// in the other order would dissolve a chapter with a robot inside it. So a live
// mission wins outright, the residue is second, and re-plannable is what is left
// when no open leg was ever handed to the fleet at all.
func classifyStalledChapter(legs []*orders.Order) (chapterVerdict, *orders.Order) {
	var residue *orders.Order
	for _, leg := range legs {
		if protocol.IsTerminal(leg.Status) {
			continue
		}
		if leg.VendorOrderID == "" {
			continue // never handed to the fleet: no robot to disturb
		}
		if leg.VendorState != "" && isTerminalVendorState(leg.VendorState) {
			if residue == nil {
				residue = leg
			}
			continue
		}
		return chapterWaiting, leg
	}
	if residue != nil {
		return chapterResidue, residue
	}
	return chapterReplannable, nil
}

// isTerminalVendorState reads the vendor's own vocabulary. It is deliberately the
// SEER spelling rather than a mapped status: MapState turns FAILED into `faulted`,
// which is a Core status with a grace period and its own meaning, and the question
// here is only whether the fleet still has work outstanding on this mission.
func isTerminalVendorState(vendorState string) bool {
	switch vendorState {
	case "FINISHED", "STOPPED", "FAILED":
		return true
	}
	return false
}

// replanStalledChapter is the resolve arm: the plan is stale and nothing is
// moving, so it goes, and the demand behind it re-queues.
//
// It calls the SAME dissolveCompound the dispatch-time reachability refusal
// calls. Two triggers, one dissolver — a second implementation would be a second
// policy, and the two would drift on exactly the questions (which legs cancel,
// whether the lane lock drops, whether the parent transitions here) that took
// three rulings to settle.
func (d *Dispatcher) replanStalledChapter(parentID int64) {
	log.Printf("chapter floor: compound %d has an open leg and nothing has moved in the whole family "+
		"for %s, and no leg was ever handed to the fleet — the plan is what went stale, so it is "+
		"dissolved and the demand re-queues to be re-resolved against the world as it now is",
		parentID, chapterStallWindow)
	if err := d.dissolveCompound(parentID, fmt.Sprintf(
		"the chapter stopped with an open leg and no vehicle committed to any of it (quiet for %s)",
		chapterStallWindow)); err != nil {
		log.Printf("chapter floor: dissolve of compound %d failed: %v (the next pass retries)", parentID, err)
	}
}

// markChapterWaiting is the visible-wait arm: a robot is genuinely out there, so
// the chapter waits — and says so, with a cause and a releaser, on the parent the
// board renders.
//
// STAMPED ON THE PARENT, not the leg. The leg is doing exactly what it should;
// the row that looks stuck to an operator is the demand, and until now that row
// carried nothing at all — `reshuffling` with a blank cause, which reads as an
// order nobody has looked at.
func (d *Dispatcher) markChapterWaiting(parentID int64, leg *orders.Order) {
	parent, err := d.db.GetOrder(parentID)
	if err != nil || parent == nil {
		log.Printf("chapter floor: could not reload compound %d to stamp its wait: %v", parentID, err)
		return
	}
	d.setQueueReason(parent, protocol.QueueStorageRearranging, CauseChapterLegInFlight,
		QueueParams{DigOrderID: leg.ID})
	d.dbg("chapter floor: compound %d waits on leg %d (%s, vendor %s/%s) — a vehicle is committed, "+
		"so this is a wait and not a stale plan", parentID, leg.ID, leg.Status, leg.VendorOrderID, leg.VendorState)
}

// recordChapterResidue is the alarm arm — the one case that neither moves nor
// waits, because both would be a guess.
//
// LOUD, AND RARE BY CONSTRUCTION. The fleet has finished with a mission and
// Core's leg never left its non-terminal status, which means a status event was
// produced and lost. Nobody is coming, so this is not congestion; but the robot
// has already acted, and dissolving would cancel a leg whose bin may sit
// somewhere Core has not written down. A human rules it.
func (d *Dispatcher) recordChapterResidue(parentID int64, leg *orders.Order) {
	detail := fmt.Sprintf("STALLED CHAPTER, UNRESOLVABLE: compound %d has been quiet for %s and its "+
		"open leg %d is %s while the fleet reports its mission %s as %s — the fleet is DONE and Core "+
		"never heard. This is a dropped status event, not congestion: nothing is coming, so waiting "+
		"cannot end it, and the robot has already acted, so dissolving could cancel a leg whose bin is "+
		"somewhere Core has not written down. A human rules this one. Start at the vendor mission and "+
		"work back to which status event was produced and lost.",
		parentID, chapterStallWindow, leg.ID, leg.Status, leg.VendorOrderID, leg.VendorState)
	log.Printf("chapter floor: !! %s", detail)
	if err := d.db.RecordRecoveryAction(stalledChapterAction, "order", parentID, detail, "system"); err != nil {
		log.Printf("chapter floor: could not record the stalled chapter for compound %d: %v", parentID, err)
	}
}
