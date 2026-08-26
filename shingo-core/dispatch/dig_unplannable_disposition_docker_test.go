//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// dig_unplannable_disposition_docker_test.go — the CALLER half of the four
// situations the owner ruled on (PLAN §R.45). The classifier half is in
// classify_plan_error_test.go, and the split between the two files is explained
// below rather than assumed.
//
// The residual "cannot be planned" bucket was enumerated from the planner before
// anything was written:
//
//	reshuffle.go:78-81    BlockersInFrontOf read fails   → situation 1
//	reshuffle.go:85-88    ListBinsByNode read fails      → situation 2
//	reshuffle.go:221-222  ListChildNodes read fails      → situation 3
//	reshuffle.go:114-116  slot is in no lane             → situation 4
//
// Three are a momentary database stutter with a completely healthy plant; today
// each one killed the operator's order. The fourth is a genuine configuration
// fault. The owner's words: "we dont want orders to fail because of a stutter.
// that demand isnt going away. config error? yeah fail loudly so the engineer can
// fix."
//
// The three stutters share a disposition but NOT a call site, and per-site pins
// are the only thing that catches the class this branch keeps paying for — a
// correct fix applied at one of three sites and never carried to the others,
// which complex_no_shuffle_slot_docker_test.go was written about twice. Where a
// site cannot be isolated honestly, that is said out loud below rather than
// papered over with a fixture that proves nothing.

// breakLaneReads makes reads of the nodes/bins tables fail the way a transport
// error does — an error that is NOT sql.ErrNoRows, which is the distinction the
// whole disposition turns on. Returns the healer.
//
// Renaming a column rather than stopping the server: testdb hands each test its
// own database, so the blast radius is this test, and it is the mechanism
// plan_read_outage_docker_test.go already uses for the layer above.
func breakLaneReads(t *testing.T, db *store.DB, table, col string) func() {
	t.Helper()
	if _, err := db.DB.Exec(`ALTER TABLE ` + table + ` RENAME COLUMN ` + col + ` TO ` + col + `_broken`); err != nil {
		t.Fatalf("start the %s read outage: %v", table, err)
	}
	healed := false
	heal := func() {
		if healed {
			return
		}
		healed = true
		if _, err := db.DB.Exec(`ALTER TABLE ` + table + ` RENAME COLUMN ` + col + `_broken TO ` + col); err != nil {
			t.Fatalf("end the %s read outage: %v", table, err)
		}
	}
	t.Cleanup(heal)
	return heal
}

// assertParkedOnTheRead is the disposition all three stutters share.
func assertParkedOnTheRead(t *testing.T, db *store.DB, demand *orders.Order, situation string) {
	t.Helper()
	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "read the demand back")

	if protocol.IsTerminal(after.Status) {
		t.Fatalf("%s: the demand is %q (%q).\n"+
			"A database stutter killed an operator's order. The plant is healthy — the lane, the bins "+
			"and the robot are all fine — and the same read usually answers on the next sweep. "+
			"PLAN §R.45: the demand waits.", situation, after.Status, after.ErrorDetail)
	}
	if !protocol.IsAcquiring(after.Status) {
		t.Errorf("%s: the demand is %q, which is outside the acquiring set — nothing re-drives it, "+
			"so the park has no releaser", situation, after.Status)
	}
	if after.QueueCause != string(CauseReadFailed) {
		t.Errorf("%s: queue_cause = %q, want %q — during an outage dozens of orders park at once, and "+
			"rendering that as congestion sends an operator to look at lanes",
			situation, after.QueueCause, CauseReadFailed)
	}
	if strings.TrimSpace(after.QueueReason) == "" {
		t.Errorf("%s: queue_reason is blank — the operator has a parked order and nothing to read", situation)
	}
}

// buriedDemandFixture is a complex demand whose bin is buried behind one blocker.
func buriedDemandFixture(t *testing.T, db *store.DB, tag string) (*Dispatcher, *orders.Order, *BuriedError) {
	t.Helper()
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, tag+"-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, tag+"-TGT")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	demand := mkQueuedComplexParent(t, db, "uuid-"+tag, bp.Code)
	return d, demand, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID}
}

// SITUATIONS 1 AND 3 ARE NOT PINNED END-TO-END HERE, AND THE REASON IS A FINDING
// RATHER THAN AN OMISSION.
//
// I wrote situation 1's fixture first — break the `nodes` table, drive the burial
// site, assert the demand parks — and it passed. It also passed with the ruling's
// arm REMOVED, which is how I learned it was proving nothing: breaking `nodes`
// trips the LANE read at the top of planBuriedReshuffleAtIntake, which has had
// its own readFailed arm since read_vs_missing.go landed. The demand parked, but
// upstream of the code under test.
//
// That is not a fixture problem to work around; it is a fact about the system:
//
//   - situation 1 (BlockersInFrontOf) reads `nodes`, and so does the lane read
//     that runs first. Any outage wide enough to reach the planner's first read
//     has already been caught and parked one layer up.
//   - situation 3 (ListChildNodes) reads `nodes` too, and is reached only after
//     situation 1's read succeeds.
//
// So both arms are BACKSTOPS for a narrow window — the nodes table answering the
// lane read and then failing a few statements later — and no honest end-to-end
// fixture can single them out, because the mechanism that breaks one breaks
// whichever read comes first. Manufacturing one would mean a test that looks like
// a proof and is not, which is exactly what the first attempt was.
//
// They are pinned where the decision is made instead: classify_plan_error_test.go
// covers every arm exhaustively, including both of these error shapes and the
// ordering hazard that would swallow them. Situation 2 below IS isolatable — it
// reads `bins`, which the lane read never touches — and is mutation-verified, so
// the caller half of the ruling is proved end-to-end there.

// SITUATION 2 — the read of what is IN a blocking slot fails.
//
// Same disposition, different call site — and the site is the point. This one is
// reached only after the first read succeeds.
//
// MUTATION (verified): delete the readFailed arm from classifyPlanError and this
// fires with the planner's own words — the demand comes back "failed" carrying
// "cannot plan reshuffle: list bins at blocker slot GRP-TEST-L1-S1: ERROR: column
// b.status does not exist". That is the ruling's whole point, reproduced: a
// column that is momentarily unreadable killing an operator's order.
func TestDigUnplannable_BinReadStutter_Waits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, demand, buried := buriedDemandFixture(t, db, "STUT2")

	heal := breakLaneReads(t, db, "bins", "status")
	d.planBuriedReshuffleAtIntake(demand, demand.PayloadCode, "line-1", buried)
	assertParkedOnTheRead(t, db, demand, "situation 2 (bins read)")

	heal()
	reloaded, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload after the outage")
	d.planBuriedReshuffleAtIntake(reloaded, reloaded.PayloadCode, "line-1", buried)
	laneClearFor(t, db, reloaded)
}

// SITUATION 4 — the slot is a child of no lane.
//
// KEEPS FAILING, and this pin exists because the behaviour is UNCHANGED: it is
// now a decision rather than an accident, and the next person to widen the wait
// arm needs to find out here that this one is deliberate.
//
// The message must name the slot: the first question anyone asks is "which one?",
// and an error that cannot answer sends them to the logs instead of the config.
//
// MUTATION (verified): route ErrSlotNotInLane into the read-failure arm and the
// demand parks under "storage rearranging" forever, with nobody told — this pin
// fires on the status.
func TestDigUnplannable_SlotInNoLane_FailsLoudly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, demand, buried := buriedDemandFixture(t, db, "NOLANE")

	// Detach the target slot from its lane: the one thing no bin moving anywhere
	// can repair.
	if _, err := db.DB.Exec(`UPDATE nodes SET parent_id=NULL WHERE id=$1`, buried.Slot.ID); err != nil {
		t.Fatalf("detach the slot: %v", err)
	}
	orphan, err := db.GetNode(buried.Slot.ID)
	testutil.MustNoErr(t, err, "reload the detached slot")
	buried.Slot = orphan

	d.planBuriedReshuffleAtIntake(demand, demand.PayloadCode, "line-1", buried)

	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "read the demand back")
	if !protocol.IsTerminal(after.Status) {
		t.Fatalf("the demand is %q, want a terminal — only a person editing configuration can attach "+
			"a slot to a lane, so waiting here is a park under a cause that never clears and nobody "+
			"is told anything is wrong (PLAN §R.45)", after.Status)
	}
	if !strings.Contains(after.ErrorDetail, orphan.Name) {
		t.Errorf("the failure detail does not name the slot: %q — the first question is which one",
			after.ErrorDetail)
	}
}
