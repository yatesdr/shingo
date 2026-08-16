package dispatch

import (
	"log"
	"sync"

	"shingocore/store/orders"
)

// ── THE ADMISSION GATE THAT IS SWITCHED OFF AND SAYS NOTHING ──────────────
//
// Arm 3 of the service-dig proposer is ONE EPISODE, ONE EXCAVATION AT A TIME.
// It exists because a buried demand does not wait for its dig: it re-resolves
// onto whatever it can reach, finds THAT buried, and raises a second dig for
// the same one bin. Digs 2 and 8 on the lane-stress rig, 2026-08-13, ended in a
// closed mutual hold that way — every wait individually lawful, the walk closed.
//
// The gate is keyed on the ORIGIN, for the reason §R.40 gives: a dig serves a
// LANE, one dig serves every demand behind the wall, so there is no 1:1 identity
// to key on and the episode is the tie a dig has.
//
// WHICH MEANS A REQUESTER WITH NO EPISODE CANNOT BE GATED BY IT. The query has
// no key, the store declines to guess, and — until this instrument — returned a
// bare 0 that the caller could not distinguish from "I asked and nothing is
// running". So the gate did not fire, nothing said so, and an unattributed
// demand could raise as many concurrent excavations as it liked for one bin.
//
// ── WHY THIS IS RECORDED AND NOT FIXED ────────────────────────────────────
//
// Because the fix is somewhere else, and doing it here would hide it. Closing
// the origin leak — every create site carrying an episode — switches this gate
// ON for a population that has never had it, which is a DISPATCH-SHAPING change:
// digs that used to be admitted will start being refused with
// serviceDigEpisodeAlreadyDigging. Landing that inside a commit whose stated
// business is labelling is how a behaviour change arrives unannounced and gets
// attributed to the wrong cause a week later.
//
// So the sequence is: measure the population, then close the leak, then re-cut
// the baseline against a plant where this gate is on everywhere. This count is
// the first of those three, and its value on the run before the leak closes is
// the number of dig proposals that were never subject to admission control.
//
// ── INSTRUMENT RULES (bin_state_drift.go states them; this file follows) ───
//
//   - It names the MECHANISM: not "no origin", but "the one-dig-per-episode
//     admission gate did not run for this proposal".
//   - The COUNTER is the number. The per-event line exists to point at a
//     specific requester, not to be grepped and totalled.
//   - The tally line does not contain the per-event marker, so a grep for the
//     marker is not inflated by the tally that reports it.
//   - It LABELS AND COUNTS ONLY. It must never decide whether the dig proceeds:
//     an instrument that also changes the outcome cannot be used to measure
//     whether changing the outcome was warranted.

// UngatedDigMarker is the per-event line's search string, named once so the
// emitter and any guard test share one definition.
const UngatedDigMarker = "dig proposal not gated by episode"

// ungatedDigTally counts service-dig proposals that reached the planner without
// the one-dig-per-episode gate having been able to run.
//
// In-process and reset-on-restart, like the arrival-refusal and bin-state-drift
// tallies: a tripwire reading, not a fact anything recovers from. The durable
// evidence is the per-requester line.
var ungatedDigTally struct {
	mu sync.Mutex
	n  int
}

// UngatedDigTally returns how many dig proposals have skipped the episode gate
// so far. EXPECTED TO FALL TO ZERO once every create site carries an origin;
// until then it is the size of the population that has no admission control.
func UngatedDigTally() int {
	ungatedDigTally.mu.Lock()
	defer ungatedDigTally.mu.Unlock()
	return ungatedDigTally.n
}

// ResetUngatedDigTally exists for tests, which must not inherit a count from
// whichever test ran before them.
func ResetUngatedDigTally() {
	ungatedDigTally.mu.Lock()
	defer ungatedDigTally.mu.Unlock()
	ungatedDigTally.n = 0
}

// noteUngatedDigProposal records one proposal the episode gate could not judge.
//
// Called for its side effect and returns nothing, because nothing may branch on
// it — see the instrument rules above.
func (d *Dispatcher) noteUngatedDigProposal(requester *orders.Order) {
	if requester == nil {
		return
	}
	ungatedDigTally.mu.Lock()
	ungatedDigTally.n++
	n := ungatedDigTally.n
	ungatedDigTally.mu.Unlock()

	log.Printf("WARN: "+UngatedDigMarker+" — order %d carries no episode, so the one-dig-per-episode "+
		"admission gate could not be keyed and DID NOT RUN for this proposal (%d so far). The dig is "+
		"proceeding, unchanged and deliberately: an originless order gets no limit, because the "+
		"alternative is serialising every unattributed dig in the plant against every other. What is "+
		"missing is the ORIGIN on order %d, not the gate. Closing that leak switches this gate on for "+
		"this order's whole population, which is a dispatch-shaping change and is why it is a separate "+
		"step from this counting.",
		requester.ID, n, requester.ID)
}
