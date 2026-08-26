//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// plan_read_outage_docker_test.go — the PLANNING site of the read/absence split,
// driven against a database that really does stop answering.
//
// read_vs_missing_test.go pins the predicate and the FINDER's arm. The dig
// PLANNER has the same three-way split (planning_service.go planBuriedReshuffle)
// and had no test at all, which matters more here than at the finder: the finder
// is choosing between sources, while this site decides whether the demand behind
// a buried bin lives. Filing an unanswered SELECT as terminal kills it.
//
// And the disposition is only half a disposition without the other half. A park
// is a promise that the work resumes when the condition clears, so the outage
// ENDING has to be tested too — a park nothing re-drives is a stall wearing a
// queue reason, and the two are indistinguishable from the assertions on the
// parked row alone.

// breakNodeReads makes every read of the nodes table fail the way a transport
// error does — an error that is not sql.ErrNoRows — and returns the function
// that ends the outage.
//
// Renaming a column the node reader selects, rather than stopping the server:
// the store's node getters are the thing under test, testdb hands each test its
// own database (so the blast radius is this test), and the same mechanism is
// already how compound_dberror_test.go and fleet_handover_test.go inject a real
// DB error. Only `nodes` is broken — the dig planner's other reads (the lane's
// dig hold, in `reservations`, and the queue-reason write, in `orders`) must
// keep working or the test would be asserting against a dead database rather
// than an unreadable node.
func breakNodeReads(t *testing.T, db *store.DB) (heal func()) {
	t.Helper()
	if _, err := db.DB.Exec(`ALTER TABLE nodes RENAME COLUMN zone TO zone_during_outage`); err != nil {
		t.Fatalf("start the node-read outage: %v", err)
	}
	healed := false
	heal = func() {
		if healed {
			return
		}
		healed = true
		if _, err := db.DB.Exec(`ALTER TABLE nodes RENAME COLUMN zone_during_outage TO zone`); err != nil {
			t.Fatalf("end the node-read outage: %v", err)
		}
	}
	t.Cleanup(heal)
	return heal
}

// TestPlanBuriedReshuffle_UnreadableLane_ParksUnderTheReadCause is the planning
// site of the split, and the assertion that matters is the disposition rather
// than the code: a dig that terminates because one SELECT did not answer takes
// the retrieve behind it with it.
//
// MUTATION (verified): in planning_service.go planBuriedReshuffle, delete the
// `if readFailed(err)` arm so the read error falls through to `if err != nil ||
// lane == nil`. The refusal comes back as invalid_node carrying "config failure:
// lane node id 3 does not exist" — a message sending someone to fix a lane that
// is configured perfectly well — and the code assertion below stops the test
// there. The Transient() assertion behind it is the one that says what the code
// costs: invalid_node is terminal, so the retrieve behind the dig is dead.
func TestPlanBuriedReshuffle_UnreadableLane_ParksUnderTheReadCause(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-OUTAGE-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-OUTAGE-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	order := mkDigOrder(t, db, "dig-outage-park", bp.Code, "LINE-OUTAGE")

	breakNodeReads(t, db)

	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})
	if pe == nil {
		t.Fatal("the dig planned a compound off a lane it could not read — every downstream step " +
			"would be built from a lane nothing looked at")
	}
	if pe.Code != codeReadFailed {
		t.Fatalf("refused with code %q (%s), want %q — the lane row is fine and the database is not",
			pe.Code, pe.Detail, codeReadFailed)
	}
	if !pe.Transient() {
		t.Fatalf("code %q is not Transient(), so one unanswered SELECT terminates the retrieve behind "+
			"this dig. A database hiccup is not a configuration fault", pe.Code)
	}

	// THE CAUSE IS ON THE ROW, AND IT IS NOT A LANE-BUSY ONE. During an outage
	// dozens of orders park in the same second; rendered as congestion that sends
	// an operator to go and look at lanes that have nothing wrong with them.
	after, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "read back the parked order")
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("order is %q — terminal for a failed read", after.Status)
	}
	if after.QueueCode != string(protocol.QueueWaitingForSlot) {
		t.Errorf("queue_code = %q, want %q", after.QueueCode, protocol.QueueWaitingForSlot)
	}
	if after.QueueCause != string(CauseReadFailed) {
		t.Errorf("queue_cause = %q, want %q", after.QueueCause, CauseReadFailed)
	}

	// NOTHING STAYS HELD. The read failed before TryLock, so a lock here would be
	// one this planner never took — and a dig lock left on a lane stops every
	// other dig in it for as long as the outage lasts.
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane is locked after a refused dig that never reached TryLock")
	}
	kids, err := db.ListChildOrders(order.ID)
	testutil.MustNoErr(t, err, "list children after the refused dig")
	if len(kids) != 0 {
		t.Errorf("the refused dig left %d child order(s) behind", len(kids))
	}
}

// TestPlanBuriedReshuffle_ReadRecovers_ThenDigs is the other half of the row,
// and the half that makes the park mean anything: the outage ends and the same
// order digs.
//
// No state is reset between the two attempts and nothing new is subscribed —
// which is the claim in the production comment ("the ordinary retry loop
// re-drives it"), and the claim that would silently stop being true if the
// failed attempt left a lock, a half-built compound, or a queue reason the
// planner then refused to plan over. Only the database changed.
//
// MUTATION (verified): make the park leave the lane locked — take
// s.laneLock.TryLock(buried.LaneID, order.ID) before the GetNode in
// planBuriedReshuffle. The first attempt still parks, and the second refuses
// with lane_locked: the wait acquires no releaser, so it never ends.
func TestPlanBuriedReshuffle_ReadRecovers_ThenDigs(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-HEAL-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-HEAL-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	order := mkDigOrder(t, db, "dig-outage-heal", bp.Code, "LINE-HEAL")

	heal := breakNodeReads(t, db)
	if _, pe := d.planner.planBuriedReshuffle(order,
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID}); pe == nil || pe.Code != codeReadFailed {
		t.Fatalf("fixture: the first attempt must park on the failed read, got %v", pe)
	}

	heal()

	// Same call, same order, same buried bin. Only the database is answering again.
	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})
	if pe != nil {
		t.Fatalf("the dig still will not plan after the read recovered: %s: %s.\n"+
			"A park whose condition has cleared and which does not then proceed is a stall, and the "+
			"order behind it is as dead as if the read had been terminal", pe.Code, pe.Detail)
	}

	kids, err := db.ListChildOrders(order.ID)
	testutil.MustNoErr(t, err, "list children of the recovered dig")
	if len(kids) == 0 {
		t.Fatal("the dig reported success and created no legs")
	}
	if !d.laneLock.IsLocked(lane.ID) {
		t.Error("the recovered dig created its legs without holding the lane — the excavation is " +
			"unprotected against a second dig in the same lane")
	}
}
