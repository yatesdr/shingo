//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// breakReservationReads makes every read of the reservations table fail the way
// a transport error does, and returns the function that ends the outage.
//
// Renaming a column the mouth queries select, on the same reasoning
// breakNodeReads states: testdb hands each test its own database, so the blast
// radius is this test, and it is a REAL driver error rather than a fake.
//
// `reservations` and not `nodes`, deliberately. The evaluator opens by reading
// the lane node and derives its candidates from `orders`; breaking either of
// those stops the pass before it reaches a candidate at all, and the test would
// assert nothing. Breaking the reservations read leaves the pass intact and
// fails it exactly where the classifier asks its first physical question — the
// dig-hold read in admitLane.
func breakReservationReads(t *testing.T, db *store.DB) (heal func()) {
	t.Helper()
	if _, err := db.DB.Exec(`ALTER TABLE reservations RENAME COLUMN state TO state_during_outage`); err != nil {
		t.Fatalf("start the reservation-read outage: %v", err)
	}
	healed := false
	heal = func() {
		if healed {
			return
		}
		healed = true
		if _, err := db.DB.Exec(`ALTER TABLE reservations RENAME COLUMN state_during_outage TO state`); err != nil {
			t.Fatalf("end the reservation-read outage: %v", err)
		}
	}
	t.Cleanup(heal)
	return heal
}

// TestLaneGate_ClassifierErrorWritesACause — a robot dwelling because Core could
// not READ the lane must say so on its row.
//
// The evaluator has two ways to leave a candidate where it is. The refusal arm
// says why, on the row, and has since `dcb2c014`: a dwell was the one wait state
// with nothing an operator could read, and three robots ran 77 minutes that way
// on the lane-stress rig before anyone could name what they were waiting for.
// The classifier-ERROR arm three lines above it logged and continued, so it
// still wrote nothing at all.
//
// The two are not the same wait and must not read the same. A refusal is a busy
// lane; an error is Core declining to answer, which is a different
// investigation and a different fix — during a database outage dozens of orders
// park at once, and a surface that renders that as congestion sends someone to
// look at lanes. CauseAdmissionError exists for exactly this distinction and the
// compound leg's equivalent arm already uses it.
//
// MUTATION RUN: drop the setQueueReason call from the classifier-error arm →
// assertion (b) fires, the dwelling order's queue_cause is blank, and it is
// indistinguishable on the row from a candidate nobody has evaluated yet.
func TestLaneGate_ClassifierErrorWritesACause(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	lane, _, w, _, _ := clearLaneFixture(t, db, "CLSERR")
	line := lineNode(t, db, "CLSERR-LINE")

	// A foreign dig owns the lane, which is what keeps the dweller at the mark
	// long enough to be evaluated. Its cause while readable is lane-dig-active;
	// the point of the test is what the row says when it is NOT readable.
	foreign := digHolder(t, db, "CLSERR-foreign-dig")
	if !d.laneLock.TryLock(lane.ID, foreign.ID) {
		t.Fatal("precondition: the foreign dig must hold the lane")
	}

	dweller := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller is not gate-staged (wait_index=%d vendor=%q)",
			dweller.WaitIndex, dweller.VendorOrderID)
	}
	markStaged(t, db, dweller.ID)
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller already has %d tail append(s) — it is not actually dwelling", n)
	}

	// The lane is otherwise fine. The only thing wrong is that Core cannot read
	// the rows it needs to decide.
	heal := breakReservationReads(t, db)

	d.EvaluateLaneReleases(lane.ID)

	held, err := db.GetOrder(dweller.ID)
	if err != nil {
		t.Fatalf("reload dweller: %v", err)
	}

	// (a) IT IS STILL DWELLING. Fail-closed: an unreadable lane is a lane nobody
	// may enter, so the robot stays at the mark.
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller was released into a lane Core could not read (%d tail append(s)). "+
			"An undetermined answer must refuse, not admit", n)
	}

	// (b) AND THE ROW SAYS WHY. This is the finding.
	if held.QueueCause == "" && held.QueueCode == "" {
		t.Fatal("a robot is dwelling because Core could not read the lane, and its row says NOTHING " +
			"— no queue_code, no queue_cause. On the board it is indistinguishable from a candidate " +
			"nobody has looked at yet, which is the gap dcb2c014 closed for the refusal arm and " +
			"left open one branch over")
	}
	if held.QueueCause != string(CauseAdmissionError) {
		t.Errorf("queue_cause = %q, want %q — an unanswered read is Core declining, not a busy "+
			"lane, and the two are investigated differently", held.QueueCause, CauseAdmissionError)
	}
	if held.QueueCode != string(protocol.QueueWaitingForSlot) {
		t.Errorf("queue_code = %q, want %q", held.QueueCode, protocol.QueueWaitingForSlot)
	}

	// (c) THE CAUSE DOES NOT OUTLIVE THE OUTAGE. A park is only half a
	// disposition without the recovery: the database answers again, the dig
	// finishes, the same evaluator pass runs, and the order goes in with a clean
	// row.
	heal()
	d.laneLock.Unlock(lane.ID, foreign.ID)
	d.EvaluateLaneReleases(lane.ID)

	if n := appendsTo(backend, dweller.VendorOrderID); n == 0 {
		after, _ := db.GetOrder(dweller.ID)
		t.Fatalf("the dweller never went in after the outage ended — status %s, cause %q. A wait "+
			"parked under a read failure has to resume when the read works",
			after.Status, after.QueueCause)
	}
	out, err := db.GetOrder(dweller.ID)
	if err != nil {
		t.Fatalf("reload released dweller: %v", err)
	}
	if out.QueueCode != "" || out.QueueCause != "" {
		t.Errorf("released dweller still carries queue_code=%q queue_cause=%q; want both cleared",
			out.QueueCode, out.QueueCause)
	}
}
