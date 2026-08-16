//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// dig_wait_visibility_docker_test.go — a demand waiting on a dig says WHICH dig.
//
// ── THE COMPLAINT ─────────────────────────────────────────────────────────
//
// "Rearranging lane LSD_01 to reach PANEL-B" is true, and it is one word plus a
// lookup. An operator watching a demand sit there has three questions the board
// could not answer: which excavation is running, what is it uncovering, and is
// it the one that will free me. All three join on the dig's order id.
//
// This drives the REAL park path — proposeLaneClearDig refuses because the lane
// is already dig-locked, the complex arm parks the demand — and reads the
// sentence back off the order row, which is what the board renders.

// TestDigWait_SentenceNamesTheExcavation is the end-to-end.
//
// MUTATION (verified): drop the digWaitFor call from the lane-busy arm and the
// sentence falls back to lane+payload — the exact wording the complaint is
// about, which is why the assertion names the dig id rather than merely checking
// the sentence changed.
func TestDigWait_SentenceNamesTheExcavation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, lane, _, _, laneSlots, _, bp := setupDwellGroup(t, db, "DIGWAIT", 2, true)

	// A wall: a blocker in front of the bin the demand needs.
	createTestBinAtNode(t, db, bp.Code, laneSlots[0].ID, "DIGWAIT-BLOCKER")
	createTestBinAtNode(t, db, bp.Code, laneSlots[1].ID, "DIGWAIT-TARGET")

	// AN EXCAVATION IS ALREADY RUNNING on that lane, uncovering the target slot.
	// This is the ordinary 1:many shape — one dig serves every demand behind the
	// same wall — and it is the wait the operator cannot resolve from the board.
	dig := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "digwait-dig"
		o.OrderType = OrderTypeMove
		o.Status = protocol.StatusReshuffling
		o.DigTargetNode = laneSlots[1].Name
	})
	if !d.laneLock.TryLock(lane.ID, dig.ID) {
		t.Fatal("precondition: the dig must hold the lane")
	}

	// A second demand arrives for the buried bin and is refused.
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "digwait-demand"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusQueued
	})

	res := d.proposeLaneClearDig(lane, laneSlots[1], demand)
	if res.outcome != serviceDigLaneBusy {
		t.Fatalf("outcome = %v, want serviceDigLaneBusy — this test is about the wait a demand "+
			"gets when somebody else's dig already holds its lane", res.outcome)
	}

	// Park it the way the complex arm does, then read what the board would show.
	digID, digTarget := digWaitFor(d.db, d.laneLock, lane.ID)
	d.setQueueReason(demand, protocol.QueueStorageRearranging, CauseLaneLocked,
		QueueParams{Lane: lane.Name, Payload: bp.Code, DigOrderID: digID, DigTarget: digTarget})

	got, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the parked demand")

	// (a) THE DIG IS NAMED. This is the join key for every other question.
	if !strings.Contains(got.QueueReason, itoaDigWait(dig.ID)) {
		t.Errorf("the wait does not name the excavation. It reads:\n  %s\nAn operator watching this "+
			"demand has to leave the board to find which dig is running, and on a lane that has had "+
			"several during the wait there is no way to tell which one frees them", got.QueueReason)
	}
	// (b) AND WHAT IT IS UNCOVERING.
	if !strings.Contains(got.QueueReason, laneSlots[1].Name) {
		t.Errorf("the wait does not name the slot being uncovered. It reads:\n  %s", got.QueueReason)
	}
	// (c) The lane and payload it already carried are still there — the clause is
	// additive, not a replacement.
	if !strings.Contains(got.QueueReason, lane.Name) || !strings.Contains(got.QueueReason, bp.Code) {
		t.Errorf("the wait lost its lane or payload. It reads:\n  %s", got.QueueReason)
	}
}

// TestDigWait_UnresolvableDigRendersExactlyAsBefore is the other half, and it is
// the one that keeps the clause honest.
//
// A lane held by an ordinary order — or a lock read that fails — has no dig to
// name. The sentence must be exactly what it was before this feature existed: a
// dig id that could not be resolved must never be invented, and zero means "not
// known", not "none".
func TestDigWait_UnresolvableDigRendersExactlyAsBefore(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, lane, _, _, _, _, bp := setupDwellGroup(t, db, "DIGWAITNONE", 2, true)

	// No dig holds this lane.
	digID, digTarget := digWaitFor(d.db, d.laneLock, lane.ID)
	if digID != 0 || digTarget != "" {
		t.Fatalf("resolved dig %d/%q on a lane no dig holds", digID, digTarget)
	}

	withClause := FormatQueueSentence(protocol.QueueStorageRearranging,
		QueueParams{Lane: lane.Name, Payload: bp.Code, DigOrderID: digID, DigTarget: digTarget})
	without := FormatQueueSentence(protocol.QueueStorageRearranging,
		QueueParams{Lane: lane.Name, Payload: bp.Code})

	if withClause != without {
		t.Errorf("an unresolved dig changed the sentence:\n  with: %s\n  without: %s\n"+
			"A wait with no identifiable excavation must read exactly as it did before the clause "+
			"existed", withClause, without)
	}
}

func itoaDigWait(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
