package dispatch

import (
	"fmt"
	"log"

	"shingocore/store/orders"
)

// ── THE EXCAVATION THAT WAS FOR NOTHING ───────────────────────────────────
//
// A service dig spends robot-minutes reaching one slot. By the time it arrives
// the episode that raised it can be over — the demand cancelled, failed, or
// finished off a bin somebody else's traffic made reachable first — and the bin
// it worked to expose is standing there with nobody coming.
//
// That is not a wedge. The corridor is released and the bin is an ordinary
// reachable bin the next demand may well take. It is WASTE, and it is invisible
// without this: the dig confirms, its legs confirm, every status reads healthy,
// and the only trace is a robot that moved three bins for no reason.
//
// This case used to be a HOLD rather than a release, and a sweep looked for it:
// the dig kept its lane until the bin was collected, so an episode that ended
// first left a corridor shut with no releaser in the world. Measured on the
// lane-stress rig 2026-08-13, five of them at once. The hold is gone — the
// corridor now goes to the collector or opens — so what is left to say is what
// was spent.
//
// ── WHY IT IS RECORDED HERE AND NOT SWEPT FOR ─────────────────────────────
//
// Because the question has an exact moment: the excavation finishing with
// nobody to hand the corridor to. A sweep would have to re-derive that moment
// from status, would ask it repeatedly of the same dig, and would go on asking
// after the bin had been collected by somebody unrelated. One event, one row.
//
// It is an ALARM and nothing more. There is no automatic response that helps:
// the digging has already happened, and the bin is exposed and usable. What a
// reader does with a run full of these is change what raises digs, which is a
// decision for a person.

// ── ONE NIL ANSWER, TWO DIFFERENT FACTS ───────────────────────────────────
//
// CollectorForDigTarget returns (nil, nil) for two reasons that are not the same
// reason, and this instrument used to file both under one name:
//
//   - THE EPISODE IS OVER. There is an origin, it was asked, and every
//     non-dig order in it has reached a terminal status. Nobody is coming.
//     This is the finding the tripwire was built for, and it is true.
//
//   - THERE WAS NO ORIGIN TO ASK WITH. The dig carries no episode, so the query
//     is not answerable and the store refuses to guess (lane_queries.go: "No
//     origin is not answerable and must not be guessed at"). Nothing was
//     learned about whether anybody is coming.
//
// The second is not a finding about the plant. It is a finding about the ORDER,
// and the row filed for it asserted "every other order in episode  has reached a
// terminal status" — with an empty episode id, about orders it never looked at.
// On run 5 all four alarms were of this kind: a 100% fire rate on NULL-origin
// digs, and a 0% rate of being about what the name says.
//
// That is this house's own rule — a check must know whether it had the input to
// check — and the cure is the one it prescribes: absence of data must never
// render as presence of a finding. TWO TYPED ROWS, so a reader counting
// abandoned excavations counts abandoned excavations.
//
// THE SPLIT LANDS BEFORE THE LEAK IS CLOSED, DELIBERATELY. Once every dig
// carries an origin the unattributed row goes to zero on its own and the
// abandoned row starts meaning what it says. Splitting afterwards would leave
// the whole run-5 population misfiled with no way to tell which was which.

const (
	// AbandonedExcavationAction is the recovery-action name a genuinely wasted
	// excavation is filed under: the episode was ASKED and it is over. EXPORTED
	// so the log line and the row it points at cannot drift — a reader told to
	// search recovery_actions for a name that is not the one written there is
	// worse served than one told nothing.
	AbandonedExcavationAction = "dig_target_abandoned"

	// UnattributedExcavationAction is the name for the other half: a dig that
	// finished with nobody identifiable to hand its corridor to BECAUSE IT HAS
	// NO EPISODE. Deliberately not a variant spelling of the row above — it is a
	// different fact, and the whole point of the split is that a query for one
	// does not silently return the other.
	UnattributedExcavationAction = "dig_target_unattributed"
)

// recordExcavationWithNobodyComing files one dig that cleared a lane and found
// nobody to hand it to — under whichever of the two names is TRUE.
//
// The branch lives here rather than at the call site on purpose. The call site
// has one question ("did the handoff find a picker?") and one answer; which of
// the two reasons produced that answer is a property of the dig, and spelling
// the origin test up there would put the same predicate in a second place.
//
// Both rows name the lane and the slot, because those are what a reader needs
// either way: which corridor was worked and what is standing in it. Only the
// abandoned row names an episode, because only it has one.
func (d *Dispatcher) recordExcavationWithNobodyComing(parent *orders.Order, laneName string) {
	action := AbandonedExcavationAction
	marker := "DIG TARGET ABANDONED"
	detail := fmt.Sprintf("DIG %d CLEARED %s FOR A BIN NOBODY IS COMING FOR: the bin at %s is exposed "+
		"and every other order in episode %s has reached a terminal status, so the work this "+
		"excavation was run for is over. The lane is RELEASED and the bin is an ordinary reachable "+
		"bin; nothing is stuck. What was spent is the excavation. Nothing automatic runs here: the "+
		"digging has already happened, and a run full of these is a reason to change what raises digs.",
		parent.ID, laneName, parent.DigTargetNode, parent.OriginID)

	if parent.OriginID == "" {
		action = UnattributedExcavationAction
		marker = "DIG TARGET UNATTRIBUTED"
		detail = fmt.Sprintf("DIG %d CLEARED %s AND NOBODY COULD BE ASKED FOR: the bin at %s is exposed, "+
			"but this dig carries NO EPISODE, so the question 'is anybody coming for it' was never put "+
			"to the database — there is no origin to key it on. THIS IS NOT A REPORT THAT THE WORK WAS "+
			"WASTED. It may well have been collected. What is broken is the dig's attribution, not the "+
			"plant: an order created without an origin is its own defect (origin_class 'orphan'), and "+
			"the lane is released a scan early because that is the recoverable direction. Fix the "+
			"create site that minted this dig without an episode and this row goes to zero.",
			parent.ID, laneName, parent.DigTargetNode)
	}

	if err := d.db.RecordRecoveryAction(action, "order", parent.ID, detail, "system"); err != nil {
		log.Printf("dig lock: could not record %s for dig %d: %v", action, parent.ID, err)
	}
	log.Printf("%s: %s", marker, detail)
}
