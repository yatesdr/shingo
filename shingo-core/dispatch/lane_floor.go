package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// THE LANE LIVENESS FLOOR — the periodic pass that wakes a quiesced plant.
//
// ── THE DEFECT IT CLOSES (F-22) ───────────────────────────────────────────
//
// EvaluateLaneReleases and RedriveHeldCompoundLegs are both level-triggered and
// idempotent, and both are driven ONLY by change events. The evaluator's own doc
// said a dropped event "costs only latency until the next firing" — which
// assumes a next firing exists. When the plant quiesces there is none, and the
// set of orders that could produce one is exactly the set waiting to be
// released. Observed 2026-08-10: eight orders staged, thirteen minutes, no bin
// movement, Edge reconciling every second against a wall.
//
// The acquiring set never had this problem, and the reason is instructive: the
// fulfillment scanner has carried a 60-second periodic sweep since long before
// any of this, described in its own source as "a safety net". Compound parents
// stranded in `reshuffling` got theirs when AdvanceStuckReshuffleParents landed.
// Two of the four machine-owned wait populations had a floor; the two that did
// not are exactly the two F-22 named. This is not a new idea, it is the third
// and fourth instances of an existing one.
//
// ── IT DECIDES NOTHING ────────────────────────────────────────────────────
//
// The pass derives lanes from the same durable state the event handlers derive
// theirs from and calls the SAME two functions the wiring calls. It contains no
// policy: not a threshold, not an age, not a "has this waited long enough". A
// floor that decided anything would be a second answer to a question the event
// path already answers, and the two would drift — which is why the trigger set
// can be generous and why this can be added without touching either mechanism.
//
// BOTH POPULATIONS, ONE PASS. Splitting the mechanism is right (the evaluator
// appends a tail to a waybill the fleet holds; the re-drive dispatches a
// `pending` leg for the first time) but splitting the COVERAGE would recreate
// F-22 in whichever population the floor skipped. So the lane set is the union
// and both re-drivers run for every lane in it.
//
// ── CADENCE IS A MAXIMUM WAIT, NOT A POLL RATE ────────────────────────────
//
// 60 seconds is the longest a released-able order can sit after the event that
// should have freed it went missing. It is not how often the system checks
// whether work exists — events do that, continuously, and on a healthy plant
// this pass finds nothing and releases nobody. On a plant with no marked lanes
// and no dig in flight it costs three queries returning zero rows.

// floorWaiter is one order the floor found waiting, and enough of its state to
// tell afterwards whether the pass actually moved it.
//
// The snapshot exists because of the TRIPWIRE, not the release: the pass must
// record a defect for every order IT freed and stay silent otherwise, and
// "otherwise" is most passes. Comparing before and after is the only way to know
// which — EvaluateLaneReleases reports nothing to its caller, deliberately, and
// giving it a return value for this would put the floor's bookkeeping into the
// path the event handlers share.
type floorWaiter struct {
	orderID int64
	laneID  int64
	pop     WaitPopulation
	cause   QueueCause
	// state is the tuple that changes when an order is released: its status, how
	// far through its plan it is, and whether the fleet has it. Compared as a
	// whole rather than field by field because any one of them moving means the
	// pass did something.
	state string
}

func waiterState(o *orders.Order) string {
	return fmt.Sprintf("%s|%d|%s", o.Status, o.WaitIndex, o.VendorOrderID)
}

// refreshWaiterCauses re-reads the live queue_cause for the waiters on one lane,
// in place, immediately before that lane is re-driven.
//
// Best-effort by construction: an unreadable order keeps the cause it was
// snapshotted with, which is exactly the behaviour that existed before this. A
// bookkeeping refresh must never be able to stop the floor from freeing anybody.
//
// It does NOT re-derive who is waiting — laneWaiters owns that, and a second
// opinion on the population is how a backstop becomes a second mechanism. Only
// the label is refreshed.
func (d *Dispatcher) refreshWaiterCauses(waiters []floorWaiter, laneID int64) {
	for i := range waiters {
		if waiters[i].laneID != laneID {
			continue
		}
		o, err := d.db.GetOrder(waiters[i].orderID)
		if err != nil || o == nil {
			continue
		}
		waiters[i].cause = QueueCause(o.QueueCause)
	}
}

// SweepLaneWaiters is one floor pass. Returns the number of orders it FREED,
// which is the number of defect records it wrote.
//
// A pass that frees nobody is the expected outcome and says nothing at all —
// see recordFloorRelease for why that silence is load-bearing.
func (d *Dispatcher) SweepLaneWaiters() int {
	// THE STRANDED-HANDOFF WALK RUNS FIRST, AND IT RUNS UNCONDITIONALLY.
	//
	// It is not part of the waiter accounting below — it releases a ROW, not an
	// order, and it does its own logging — so it sits ahead of the early return
	// and outside the freed count. See sweepStrandedHandoffs.
	d.sweepStrandedHandoffs()

	before, err := d.laneWaiters()
	if err != nil {
		log.Printf("lane floor: could not read the waiting set: %v (skipping this pass; the next one retries)", err)
		return 0
	}
	if len(before) == 0 {
		return 0
	}

	// Distinct lanes, so a lane holding six dwellers is evaluated once. The pass
	// order is unspecified and does not matter: both re-drivers are idempotent
	// and derive from live state, and the per-lane mutex inside the evaluator is
	// what serializes this against a concurrent event firing — the floor takes no
	// lock of its own and must not, or it would be a second arbitration of a
	// question the evaluator already settles.
	lanes := make(map[int64]bool, len(before))
	for _, w := range before {
		lanes[w.laneID] = true
	}
	for laneID := range lanes {
		// RE-READ THE CAUSE FIRST, and read it HERE rather than after the release.
		//
		// The snapshot in `before` was taken for every lane at once, so by the time
		// the last lane is re-driven its waiters' causes are as old as all the
		// re-drives that ran in between — during which an evaluator may have written
		// a more specific one. Reporting the opening snapshot then attributes the
		// release to a cause that had already been superseded.
		//
		// After the release is too late: a successful release CLEARS the cause (it
		// describes a wait that is over), so a post-release read returns blank and
		// blank is a different defect record entirely. Immediately before the
		// re-drive is the last moment the live value still means "why this order is
		// waiting".
		d.refreshWaiterCauses(before, laneID)
		d.EvaluateLaneReleases(laneID)
		d.RedriveHeldCompoundLegs(laneID)
	}

	freed := 0
	for _, w := range before {
		after, gErr := d.db.GetOrder(w.orderID)
		if gErr != nil || after == nil {
			// Unreadable now: it may well have been freed, but "may well" is not
			// what a defect record asserts. Skipping is the quiet direction, and a
			// record that over-reports is the failure this whole shape is avoiding.
			continue
		}
		if waiterState(after) == w.state {
			continue // still waiting — the ordinary case, and it is silent
		}
		d.recordFloorRelease(w)
		freed++
	}
	return freed
}

// sweepStrandedHandoffs gives maybeReleaseStrandedHandoff the level-triggered
// door it landed without.
//
// ── F-22'S OWN RULE, APPLIED TO THE DOOR THAT MISSED IT ───────────────────
//
// Every path into that releaser is a lane-EXIT or teardown event, and the floor
// reached it only through EvaluateLaneReleases' `if freed` arm. The wedge it
// exists for is a corridor whose stranded row refuses every inbound comer: no
// admits, no exits, `freed` false by construction. So the fix was silent in
// exactly the state it was written for — the measured wedges (orders 163 and
// 202, eighteen sim-hours) are quiesced lanes, not busy ones. This is the header
// above's own sentence — "a dropped event costs only latency until the next
// firing, which assumes a next firing exists" — asked of one more mechanism.
//
// THE LANE SET IS DERIVED FROM THE ROWS, not from laneWaiters. The population
// queued behind a stranded handoff is `sourcing` demands the resolver turned
// away, which are neither gate-staged dwellers nor held legs; a waiter-derived
// set would miss the lane and the pass would return before its loop on a plant
// where nothing else is waiting at all. See reservations.LanesHeldByHandoff.
//
// IT DECIDES NOTHING, same as the rest of this file: it calls the same function
// the event path calls, which re-reads every holder and fails closed on each.
// On a healthy plant it is one query returning zero rows.
func (d *Dispatcher) sweepStrandedHandoffs() {
	lanes, err := reservations.LanesHeldByHandoff(d.db.DB)
	if err != nil {
		log.Printf("lane floor: could not read the lanes held by a dig handoff: %v "+
			"(skipping this pass; the next one retries)", err)
		return
	}
	for _, laneID := range lanes {
		d.maybeReleaseStrandedHandoff(laneID)
	}
}

// laneWaiters is the floor's candidate derivation: every order waiting on a lane
// that the event path would have released, with the lane it waits on.
//
// IT DERIVES THE SAME SETS THE EVENT PATH DOES, from durable order state and
// nothing else — gate-staged is IsGateStaged plus the wait step's lane, exactly
// as gateStagedForLane reads it; the leg set is orders.AwaitingFleetSQL, exactly
// as ListHeldLegParentsInLane reads it. A floor with its own idea of who is
// waiting would find a different population from the one it re-drives, which is
// how a backstop becomes a second mechanism.
func (d *Dispatcher) laneWaiters() ([]floorWaiter, error) {
	var out []floorWaiter

	// ── gate-staged dwellers ──────────────────────────────────────────────
	candidates, err := d.db.ActiveGateCandidates()
	if err != nil {
		return nil, fmt.Errorf("list gate candidates: %w", err)
	}
	for _, o := range candidates {
		if !IsGateStaged(o) || o.Status == StatusFaulted {
			continue
		}
		lane := laneOfGateWait(o)
		if lane == 0 {
			continue
		}
		out = append(out, floorWaiter{
			orderID: o.ID, laneID: lane, pop: PopGateStaged,
			cause: QueueCause(o.QueueCause), state: waiterState(o),
		})
	}

	// ── held compound legs ────────────────────────────────────────────────
	legs, err := d.db.ListLaneHeldLegs()
	if err != nil {
		return nil, fmt.Errorf("list held legs: %w", err)
	}
	for _, l := range legs {
		out = append(out, floorWaiter{
			orderID: l.OrderID, laneID: l.LaneID, pop: PopCompoundLeg,
			cause: QueueCause(l.QueueCause), state: l.State,
		})
	}
	return out, nil
}

// MarkStationWaitIfOwned writes CauseStationWait on an order that has just
// staged at a wait the STATION owns, and does nothing otherwise.
//
// ── WHY IT IS CONDITIONAL, AND WHY IT IS HERE AT ALL ──────────────────────
//
// An order dwelling at a station wait used to carry a blank row — no code, no
// cause, no sentence — which on the board and in every diagnostic query looks
// exactly like an order nobody has evaluated yet. That is the population the
// liveness floor cannot cover (PopStationWait: Core must not auto-release a
// wait whose precondition only the station observes), so a blank there is not
// a gap a floor will later fill. It is the permanent state of the row.
//
// A LANE wait is left alone. Its cause is written by the evaluator when it
// refuses, and those causes are specific — lane-occupied, lane-target-buried,
// lane-deeper-pending. Stamping a generic one here would overwrite the useful
// answer with a vague one, on the population that already has the machinery to
// say something better.
//
// Best-effort: this is advisory metadata on a status the fleet already reported,
// and failing to write it must not disturb the staged notification that follows.
func (d *Dispatcher) MarkStationWaitIfOwned(orderID int64) {
	order, err := d.db.GetOrder(orderID)
	if err != nil || order == nil {
		return
	}
	var steps []resolvedStep
	if json.Unmarshal([]byte(order.StepsJSON), &steps) != nil {
		return
	}
	w, ok := waitAt(steps, order.WaitIndex)
	if !ok || !IsStationWait(w.WaitKind) {
		return
	}
	d.setQueueReason(order, protocol.QueueWaitingForPartner, CauseStationWait,
		QueueParams{Destination: w.Node})
}

// laneOfGateWait returns the lane an order is parked at, or 0.
//
// Same walk IsGateStaged just did, and that duplication is deliberate: making
// IsGateStaged return the lane would give a widely-used predicate a second
// return value that almost every caller discards, and the parse is over a string
// already in memory.
func laneOfGateWait(o *orders.Order) int64 {
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(o.StepsJSON), &steps); err != nil {
		return 0
	}
	w, ok := waitAt(steps, o.WaitIndex)
	if !ok || w.WaitKind != WaitKindLane {
		return 0
	}
	return w.WaitLane
}

// floorReleaseAction is the recovery_actions verb. One string, because the
// histogram this batch exists to produce is a GROUP BY over these rows.
const floorReleaseAction = "lane_floor_release"

// recordFloorRelease writes the defect record for one order the floor freed.
//
// ── IT IS A RECORD, NOT A LOG LINE ────────────────────────────────────────
//
// Same footing as advance_stuck_reshuffle: a recovery_actions row, queryable,
// on the operations surface. A log line is not a deliverable — the point of this
// batch is a HISTOGRAM OF FLOOR RELEASES BY CAUSE, which is a ranked worklist of
// missing emitters, and you cannot GROUP BY a log line.
//
// The cause is the payload. An order carries the cause it was refused under
// while it waits, and causeReleasers turns that cause into the sentence naming
// what should have ended the wait — so the record reads "this event did not
// fire", not "something was slow". That is the difference between a work item
// and a shrug.
//
// ── AND IT IS WRITTEN ONLY WHEN THE PASS ACTUALLY FREED SOMETHING ─────────
//
// AdvanceStuckReshuffleParents states the rule this obeys, and states it from
// the scar: "A recovery log that fires when nothing was recovered destroys the
// same thing an alarm that cannot fire destroys — the next reader's ability to
// believe it — and it is arguably worse, because somebody will eventually count
// these." Somebody is going to count these. On a healthy plant this function is
// never called, and TestFloorIsSilentWhenItFreesNobody is what keeps that true.
//
// ── ONE CAUSE IS EXPECTED RATHER THAN A DEFECT ────────────────────────────
//
// A fleet-refusal release is not a missing emitter — no event exists and none
// should be invented (causeReleasers spells out why). Its records are a
// fleet-health signal, and they are written the same way and separated at read
// time by the cause, rather than suppressed here. Suppressing would hide the
// gap exactly when it starts to matter, which is the burial shadow's lesson.
func (d *Dispatcher) recordFloorRelease(w floorWaiter) {
	// THREE CASES, AND THEY ARE DIFFERENT DEFECTS. Collapsing them was wrong on
	// the first live run: an order parked before its refusal arm wrote a cause
	// was reported as "causeReleasers has no row", which accuses the inventory of
	// a gap it does not have and sends the reader to the wrong file.
	cause := w.cause
	var should string
	switch r, ok := releaserFor(w.cause); {
	case w.cause == "":
		// A BLANK IS TWO DIFFERENT DEFECTS AND THIS USED TO NAME ONLY ONE.
		//
		// It said "find the arm that refused it without calling setQueueReason",
		// which is right when an arm did refuse. It is wrong — and actively
		// misdirecting — when NOTHING EVER EVALUATED the order, because then
		// there is no arm to find. §12.49 traced one: order 56 went `staged` at
		// 04:32:58 and the floor freed it 17 seconds later, while the evaluator's
		// refusal line fired 77 times across the run and not once for it. Nobody
		// refused it; nobody looked at it.
		//
		// The two want opposite investigations — fix an arm, versus find the
		// event that should have triggered an evaluation — so the record names
		// both and says how to tell them apart. Distinguishing them
		// automatically would take a "was this ever evaluated" bit the order row
		// does not carry; until it does, the reader is given the discriminator
		// rather than a guess.
		cause = "(none)"
		should = "THE ROW CARRIED NO CAUSE, and that is TWO possible defects. Either (a) something " +
			"refused this order without recording a reason — the log will show a refusal for it; " +
			"or (b) NOTHING EVER EVALUATED IT, in which case there is no refusal to find and the " +
			"defect is the missing event that should have triggered an evaluation before the " +
			"floor's tick. Check the log for a refusal naming this order: if there is none, it is " +
			"(b). This is not an inventory gap either way"
	case !ok:
		should = "no releaser is on file for this cause — the releaser inventory has no row for it, " +
			"which is itself the defect"
	case r.finding != "":
		should = r.finding
	case r.what != "":
		should = "the event that should have ended it: " + r.what
	}

	detail := fmt.Sprintf("freed by the lane liveness floor, not by an event (%s, lane %d, cause %q). %s",
		w.pop, w.laneID, cause, should)

	if err := d.db.RecordRecoveryAction(floorReleaseAction, "order", w.orderID, detail, "system"); err != nil {
		log.Printf("lane floor: could not record the release of order %d: %v", w.orderID, err)
	}

	// THE ONE LOUD CASE. A gate-staged order is a robot physically parked at a
	// mark holding an unsealed waybill, so a floor release there means a
	// committed vehicle stood still for up to a whole interval waiting for Core
	// to notice. A held leg is a row; this is a robot.
	if w.pop == PopGateStaged {
		log.Printf("LANE FLOOR: order %d was dwelling at lane %d under %q and only the periodic floor "+
			"freed it — a committed robot stood still for up to one interval. %s",
			w.orderID, w.laneID, cause, should)
		return
	}
	log.Printf("lane floor: released order %d (%s, lane %d, cause %q)", w.orderID, w.pop, w.laneID, cause)
}
