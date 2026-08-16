//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// floorReleaseRecords returns the recovery_actions rows this batch's floor wrote.
func floorReleaseRecords(t *testing.T, db *store.DB) []string {
	t.Helper()
	actions, err := db.ListRecoveryActions(100)
	if err != nil {
		t.Fatalf("list recovery actions: %v", err)
	}
	var out []string
	for _, a := range actions {
		if a.Action == floorReleaseAction {
			out = append(out, a.Detail)
		}
	}
	return out
}

// TestFloor_WakesAQuiescedPlantAndRecordsTheDefect is F-22's acceptance.
//
// ── THE SCENARIO, RECONSTRUCTED EXACTLY ───────────────────────────────────
//
// A robot dwells at a lane's mark behind a dig. The dig's lock then drops
// WITHOUT any event being emitted — which is the whole of F-22 and is why the
// fixture releases the lock through laneLock.Unlock rather than through
// unlockLaneForCompound: the latter evaluates the lane it frees (that is D2's
// fix), and calling it here would test the event path instead of its absence.
//
// What is left is the quiesced plant: a condition has cleared, the order that
// could have emitted the event is the one waiting to be released, and nothing
// re-asks. On 2026-08-10 that state held for thirteen minutes with eight orders
// in it and only ended when a human looked.
//
// The floor ends it within one interval, and — this is the half that makes it a
// batch rather than a patch — it says so on a queryable row naming the cause,
// which is what the emitter hunt is meant to run on afterwards.
//
// MUTATIONS RUN (both fire):
//  1. drop the RedriveHeldCompoundLegs + EvaluateLaneReleases calls from
//     SweepLaneWaiters → (a) the dweller stays staged forever, which is F-22
//     reproduced against the floor that is supposed to prevent it.
//  2. drop the recordFloorRelease call → (a) still passes and (b)/(c) fail: the
//     plant recovers and leaves no evidence that an event was missing, which is
//     the difference between a fix and a silent workaround.
func TestFloor_WakesAQuiescedPlantAndRecordsTheDefect(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	lane, _, w, _, _ := clearLaneFixture(t, db, "FLOORF22")
	line := lineNode(t, db, "FLOORF22-LINE")

	// A dig holds the lane, so the dweller parks behind it with a readable cause.
	digger := digHolder(t, db, "FLOORF22-dig")
	if !d.laneLock.TryLock(lane.ID, digger.ID) {
		t.Fatal("precondition: the dig must hold the lane")
	}

	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller is not gate-staged (wait_index=%d vendor=%q)",
			dweller.WaitIndex, dweller.VendorOrderID)
	}
	markStaged(t, db, dweller.ID)

	// One evaluation writes the cause on the row — the state an operator sees and
	// the state the floor will later report.
	d.EvaluateLaneReleases(lane.ID)
	parked, err := db.GetOrder(dweller.ID)
	if err != nil {
		t.Fatalf("reload dweller: %v", err)
	}
	if parked.QueueCause != string(CauseLaneDigActive) {
		t.Fatalf("dweller cause = %q, want %q — the fixture is not reproducing a dig-blocked dwell",
			parked.QueueCause, CauseLaneDigActive)
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller already has %d tail append(s) — it is not dwelling", n)
	}

	// ── THE PLANT QUIESCES. The lock drops and NOTHING is emitted. ──
	d.laneLock.Unlock(lane.ID, digger.ID)

	// Nothing has happened yet, and nothing will: this is the wall.
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller moved with no event and no floor pass — the fixture is not quiesced")
	}

	// ── ONE FLOOR PASS. What the 60s loop does, called directly. ──
	freed := d.SweepLaneWaiters()

	// (a) THE ROBOT MOVES.
	if n := appendsTo(backend, dweller.VendorOrderID); n == 0 {
		after, _ := db.GetOrder(dweller.ID)
		t.Fatalf("the floor did not free the dweller — status %s, wait_index %d, cause %q. The dig "+
			"lock is gone and every event that could have re-asked was consumed before it dropped; "+
			"if the periodic pass does not notice, nothing does. This is F-22",
			after.Status, after.WaitIndex, after.QueueCause)
	}
	if freed != 1 {
		t.Errorf("floor reported %d release(s), want 1 — the count is what the loop logs and what "+
			"the histogram is built from", freed)
	}

	// (b) IT IS RECORDED AS A DEFECT, not a log line.
	records := floorReleaseRecords(t, db)
	if len(records) != 1 {
		t.Fatalf("floor wrote %d recovery_actions record(s), want 1. A release the floor made and "+
			"did not record is a missing emitter nobody will ever find: the histogram of these rows "+
			"BY CAUSE is the worklist the emitter hunt runs on, and you cannot GROUP BY a log line",
			len(records))
	}

	// (c) AND THE RECORD NAMES THE CAUSE AND WHAT SHOULD HAVE FIRED.
	detail := records[0]
	if !strings.Contains(detail, string(CauseLaneDigActive)) {
		t.Errorf("the record does not name the cause the order was carrying (%q):\n  %s",
			CauseLaneDigActive, detail)
	}
	if !strings.Contains(detail, string(PopGateStaged)) {
		t.Errorf("the record does not name the population, so the loud case cannot be told from the "+
			"quiet one at read time:\n  %s", detail)
	}
	// THE PROBE MOVED, THE ASSERTION DID NOT. This looked for the literal
	// "unlockLaneForCompound" — a Go identifier that was in the releaser prose and
	// is no longer, because that prose is rendered verbatim into
	// recovery_actions.detail and read by a human on the Recovery tab. The
	// question is unchanged: does the record say what SHOULD have ended this wait.
	// It is now asked against the sentence rather than against a symbol name,
	// which is what the record is supposed to contain.
	if !strings.Contains(detail, "the dig holding this lane releases it") {
		t.Errorf("the record does not say what SHOULD have ended the wait, so it reads as 'something "+
			"was slow' rather than 'this event did not fire':\n  %s", detail)
	}
}

// TestFloor_IsSilentWhenItFreesNobody is the anti-cry-wolf guard, and it is the
// reason the floor snapshots state instead of just calling the two re-drivers.
//
// AdvanceStuckReshuffleParents states the rule from its own scar: "A recovery log
// that fires when nothing was recovered destroys the same thing an alarm that
// cannot fire destroys — the next reader's ability to believe it — and it is
// arguably worse, because somebody will eventually count these." Somebody is
// going to count these; the whole deliverable is a count.
//
// The failure this guards is not noisy logs. It is that a floor which records on
// every pass makes the tripwire's number meaningless, and a meaningless number is
// indistinguishable from a healthy one — so the emitter hunt would run on noise
// and conclude there was nothing to find.
//
// MUTATION: record unconditionally (move recordFloorRelease above the
// state-comparison in SweepLaneWaiters) — this fires with a record written for
// an order that never moved.
func TestFloor_IsSilentWhenItFreesNobody(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	lane, _, w, _, _ := clearLaneFixture(t, db, "FLOORQUIET")
	line := lineNode(t, db, "FLOORQUIET-LINE")

	// The dig KEEPS the lane. The dweller is genuinely blocked, so a pass over it
	// is correct to change nothing.
	digger := digHolder(t, db, "FLOORQUIET-dig")
	if !d.laneLock.TryLock(lane.ID, digger.ID) {
		t.Fatal("precondition: the dig must hold the lane")
	}
	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	markStaged(t, db, dweller.ID)
	d.EvaluateLaneReleases(lane.ID)

	// Three passes, because a floor that records once per ORDER rather than once
	// per RELEASE would look clean on a single pass and produce a rising count on
	// a real plant, where the same blocked order is swept every minute all shift.
	for i := 0; i < 3; i++ {
		if freed := d.SweepLaneWaiters(); freed != 0 {
			t.Fatalf("pass %d reported %d release(s) — nothing could have moved, the dig still "+
				"holds the lane", i+1, freed)
		}
	}

	if records := floorReleaseRecords(t, db); len(records) != 0 {
		t.Errorf("the floor wrote %d recovery_actions record(s) over three passes that freed "+
			"nobody:\n  %s\nA recovery record that fires when nothing was recovered destroys the "+
			"reader's ability to believe the ones that matter — and this batch's whole deliverable "+
			"is a COUNT of these rows", len(records), strings.Join(records, "\n  "))
	}

	// And the order really is still waiting — the silence must be because nothing
	// happened, not because the pass never looked.
	still, err := db.GetOrder(dweller.ID)
	if err != nil {
		t.Fatalf("reload dweller: %v", err)
	}
	if still.QueueCause != string(CauseLaneDigActive) {
		t.Errorf("dweller cause = %q, want %q — if it is no longer blocked the silence proved nothing",
			still.QueueCause, CauseLaneDigActive)
	}
}

// TestFloorRefreshesCauseBeforeRedrive pins item 4b: the floor reports the cause
// the order is carrying when its lane is re-driven, not the one snapshotted when
// the pass opened.
//
// laneWaiters snapshots every lane's waiters at once, then the pass re-drives
// lane by lane. By the time the last lane is reached its snapshot is as old as
// all the re-drives in between, during which an evaluator may have written a more
// specific cause — and the defect record would then name a cause that had already
// been superseded, sending the reader to the wrong arm.
//
// It cannot be read AFTER the release instead: a successful release clears the
// cause, and a blank is a different defect record entirely.
func TestFloorRefreshesCauseBeforeRedrive(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := testdb.CreateOrder(t, db)
	if _, err := db.Exec(`UPDATE orders SET queue_cause=$2 WHERE id=$1`, order.ID, string(CauseGateRebindUnavailable)); err != nil {
		t.Fatalf("seed live cause: %v", err)
	}

	// The stale snapshot: what the pass recorded before the other lanes ran.
	waiters := []floorWaiter{{orderID: order.ID, laneID: 7, pop: PopStationWait, cause: CauseStationWait}}

	d.refreshWaiterCauses(waiters, 7)

	if waiters[0].cause != CauseGateRebindUnavailable {
		t.Errorf("cause = %q, want %q — the floor reported a snapshot the order had already moved past",
			waiters[0].cause, CauseGateRebindUnavailable)
	}

	// A waiter on a DIFFERENT lane is untouched: this lane's re-drive says nothing
	// about orders waiting elsewhere, and refreshing them here would read a value
	// that is stale again by the time their own lane runs.
	other := []floorWaiter{{orderID: order.ID, laneID: 99, pop: PopStationWait, cause: CauseStationWait}}
	d.refreshWaiterCauses(other, 7)
	if other[0].cause != CauseStationWait {
		t.Errorf("cause = %q for a waiter on another lane, want it untouched (%q)",
			other[0].cause, CauseStationWait)
	}
}
