package dispatch

import (
	"fmt"
	"log"

	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// ── THE HOLD THAT NOBODY WILL EVER END (§R.76 / §R.77) ────────────────────
//
// Arm 2 made a service dig hold its lane until the bin it uncovered is
// collected, and keyed that release on a physical fact so no order's death can
// strand it. One thing can still strand it: nobody coming at all. The demand
// that caused the excavation is cancelled, or fails, or its whole episode ends
// while the bin sits there — and the dig goes on holding a corridor shut for a
// collection that is never going to happen.
//
// ── WHY IT IS NOT SIMPLY RELEASED, WHICH IS THE OBVIOUS FIX ───────────────
//
// Because the obvious fix is the hole. "If nobody is coming, drop the lane" is
// fail-open, and fail-open is exactly the behaviour arm 2 replaced: a dig whose
// claim went away releasing its lane is how the uncovered bin ends up exposed to
// the next order's shuffle slot. A timer would be worse — it would do it to digs
// whose demand is merely slow.
//
// RULED: this is an ALARM. A human reads the row and decides, and the escape
// hatch is the Core-side hard release, the same one every station-owned wait
// uses. That is the same disposition the mutual-dig-hold tripwire carries, for
// the same reason: the automatic response that suggests itself is the one that
// re-creates the defect.
//
// ── QUIET WHEN ZERO (law 9) ───────────────────────────────────────────────
//
// The normal state is a plant full of digs, none of them owing anything, and
// nothing said. A firing means an episode ended with its excavation still
// outstanding, which is worth a person's attention every single time.

// UnfetchedTargetAction is the recovery-action name an abandoned hold is filed
// under. EXPORTED so the engine's log line and the row it points at cannot
// drift — a reader told to search recovery_actions for a name that is not the
// one written there is worse served than one told nothing.
const UnfetchedTargetAction = "reshuffle_target_never_collected"

// SweepReshufflesHoldingTargets records every service dig that is holding a lane
// for a bin whose episode is over. Returns the number recorded — zero on a
// healthy plant, and the number a soak reads.
func (d *Dispatcher) SweepReshufflesHoldingTargets() int {
	if d.laneLock == nil {
		return 0
	}
	holds, err := reservations.ListDigHolds(d.db.DB)
	if err != nil {
		log.Printf("reshuffle target sweep: could not read the dig holds: %v (skipping this pass)", err)
		return 0
	}
	recorded, unanswerable := 0, 0
	for _, h := range holds {
		parent, pErr := d.db.GetOrder(h.OrderID)
		if pErr != nil || parent == nil {
			continue // unreadable this pass; the next one asks again
		}
		owes, oErr := d.db.DigStillOwesItsTarget(parent)
		if oErr != nil {
			log.Printf("reshuffle target sweep: %v", oErr)
		}
		if !owes {
			continue // holding for its own legs, or holding nothing: not this instrument's business
		}

		// THE FACT GAP IS REPORTED, NOT BRIDGED (§R.18). A dig with no origin
		// cannot be asked whether its episode is over, and the answer to "I could
		// not tell" is never to guess in the direction that drops a lane. It is
		// counted and named below so a blind spot reads as a blind spot rather
		// than as a clean sweep.
		if parent.OriginID == "" {
			unanswerable++
			continue
		}
		live, cErr := d.db.CountLiveOrdersInEpisode(parent.OriginID, parent.ID)
		if cErr != nil {
			log.Printf("reshuffle target sweep: %v (skipping dig %d this pass)", cErr, parent.ID)
			continue
		}
		if live > 0 {
			continue // somebody in the episode is still running: the ordinary case
		}

		d.recordUnfetchedTarget(parent, h.LaneID)
		recorded++
	}
	if unanswerable > 0 {
		log.Printf("reshuffle target sweep: %d dig(s) are holding a lane for a bin and carry NO ORIGIN, "+
			"so whether anybody is still coming for it cannot be asked. These are not counted as "+
			"findings and they are not counted as clean — an order created without an origin is its "+
			"own defect (origin_class 'orphan'), and it makes this instrument blind wherever it "+
			"happens", unanswerable)
	}
	return recorded
}

// recordUnfetchedTarget files one abandoned hold and says it loudly.
//
// It names the lane, the slot and the episode, because those are the three
// things the person ruling the incident needs: what is shut, what is standing in
// it, and which demand died owing it.
func (d *Dispatcher) recordUnfetchedTarget(parent *orders.Order, laneID int64) {
	laneName := fmt.Sprintf("%d", laneID)
	if lane, err := d.db.GetNode(laneID); err == nil && lane != nil {
		laneName = lane.Name
	}
	detail := fmt.Sprintf("RESHUFFLE %d IS HOLDING LANE %s FOR A BIN NOBODY IS COMING FOR: it dug %s "+
		"clear, the bin at %s is still standing there, and every other order in episode %s has reached "+
		"a terminal status. The hold is correct and it will not end on its own — it is keyed on the bin "+
		"leaving, and nothing is going to move it. NOTHING AUTOMATIC RUNS HERE, ruled: releasing the "+
		"lane on this signal is the fail-open behaviour arm 2 replaced, and a timer would do it to digs "+
		"whose demand is merely slow. A human rules the incident; the escape hatch is the Core-side "+
		"hard release.",
		parent.ID, laneName, laneName, parent.DigTargetNode, parent.OriginID)

	if err := d.db.RecordRecoveryAction(UnfetchedTargetAction, "order", parent.ID, detail, "system"); err != nil {
		log.Printf("reshuffle target sweep: could not record the abandoned hold for dig %d: %v",
			parent.ID, err)
	}
	log.Printf("RESHUFFLE TARGET NEVER COLLECTED: %s", detail)
}
