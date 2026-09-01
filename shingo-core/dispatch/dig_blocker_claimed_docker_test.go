//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// dig_blocker_claimed_docker_test.go — two demands on one lane, minutes apart.
//
// Order A retrieves the shallowest bin and is dispatched: robot en route, hard
// claim on the bin. Order B wants the deepest bin in the same lane. B's dig
// includes A's bin in its blocker list (findBuriedBlockers filters nothing), the
// child-creation claim CAS correctly refuses it, and the question is what B does
// about that.
//
// It used to die. codeReshuffle is structural, so B failed terminally on traffic
// as ordinary as two demands on one lane — while the thing blocking it was a bin
// a robot was in the act of removing. The blocker is ceasing to exist; that is
// the purest congestion in the system, and congestion waits (D18-Q4).
//
// The soft case at the bottom used to pin the unconditional steal against the
// open dig/soft-holder contract question. That question is settled (§7): a soft
// hold is a promise, and the take goes by the demand ranking — so the dig yields
// to a holder it does not outrank.

// mkDigOrder is the retrieve that wants the deep bin.
func mkDigOrder(t *testing.T, db *store.DB, uuid, payloadCode, delivery string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:     uuid,
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusPending,
		Quantity:     1,
		PayloadCode:  payloadCode,
		DeliveryNode: delivery,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create dig order "+uuid)
	return o
}

// TestDig_BlockerHardClaimed_Parks is the defect.
//
// MUTATION (verified): in planning_service.go planBuriedReshuffle, delete the
// errors.Is(err, store.ErrBlockerClaimed) arm so the refusal falls through to
// codeReshuffle. The code assertion below fires first (reshuffle_error, not
// blocker_claimed), and the Transient() assertion behind it fires too — which is
// the pair that matters: the code is only interesting because it decides whether
// the order lives.
func TestDig_BlockerHardClaimed_Parks(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-HARD-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-HARD-TGT")

	// Order A: dispatched, robot en route, hard claim on the shallow bin. The
	// claim goes through the production reserve→claim→confirm sequence so the row
	// under test is a real hold, not a value written into the column.
	orderA := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dig-hard-a"
		o.Status = protocol.StatusDispatched
	})
	testdb.ClaimBinForTest(t, db, blocker.ID, orderA.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	orderB := mkDigOrder(t, db, "dig-hard-b", bp.Code, "LINE-HARD")

	_, pe := d.planner.planBuriedReshuffle(orderB, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	if pe == nil {
		t.Fatal("the dig planned a compound over a bin another order has hard-claimed — " +
			"it would drive to a bin a robot is already carrying away")
	}
	if pe.Code != codeBlockerClaimed {
		t.Fatalf("refused with code %q (%s), want %q", pe.Code, pe.Detail, codeBlockerClaimed)
	}
	if !pe.Transient() {
		t.Fatalf("code %q is not Transient() — order B fails TERMINALLY because another order's "+
			"robot is mid-pickup in the same lane. That is the D18-Q4 wait-not-fail violation this "+
			"test exists for", pe.Code)
	}

	// NOTHING STAYS HELD. A park that keeps the lane lock is a park that stops
	// every other dig in that lane for as long as it waits.
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane is still locked after a refused dig — the refusal must ride the same " +
			"unlock the other planning failures do")
	}

	// THE PARENT IS STILL SCANNABLE. CreateCompoundOrder writes the children
	// before it moves the parent precisely so this holds: `reshuffling` is outside
	// IsAcquiring, so a parent left there would never be looked at again and the
	// wait would be a permanent stall.
	after, err := db.GetOrder(orderB.ID)
	testutil.MustNoErr(t, err, "read back order B")
	if after.Status == protocol.StatusReshuffling {
		t.Fatalf("order B is %q with no compound under it — outside the acquiring set, so the "+
			"scanner never re-plans it and the wait never ends", after.Status)
	}

	// THE CAUSE IS ON THE ROW. An operator seeing "storage rearranging" with no
	// cause cannot tell this from a lane held by someone else's dig.
	if after.QueueCode != string(protocol.QueueStorageRearranging) {
		t.Errorf("queue_code = %q, want %q", after.QueueCode, protocol.QueueStorageRearranging)
	}
	if after.QueueCause != string(CauseDigBlockerClaimed) {
		t.Errorf("queue_cause = %q, want %q — the three reshuffle waits clear on three different "+
			"signals and must not share one tag", after.QueueCause, CauseDigBlockerClaimed)
	}

	// AND NO HALF-BUILT COMPOUND. The store's transaction rolls back; if it did
	// not, the retry would find its own orphaned legs.
	kids, err := db.ListChildOrders(orderB.ID)
	testutil.MustNoErr(t, err, "list children of the refused dig")
	if len(kids) != 0 {
		t.Errorf("refused dig left %d child order(s) behind", len(kids))
	}
}

// TestDig_BlockerLeavesTheLane_ThenDigs is the other half: the wait ends by
// itself. The blocker being picked out of the lane is the whole releaser — no
// timer, no retry counter, and nothing new subscribed.
//
// The event wiring it rides is already there: engine/wiring.go registers
// triggerFulfillment on EventBinEnteredTransit (the pickup that moves the bin to
// _TRANSIT), EventBinUpdated (the arrival write that clears the claim), and
// EventOrderCompleted / Failed / Cancelled (the holder going terminal, which
// releases through TerminalizeOrder). Any of the five re-drives the scan; this
// test drives the state change those events announce and asserts the dig then
// plans clean.
func TestDig_BlockerLeavesTheLane_ThenDigs(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, shuffleSlots, bp := setupNodeGroupWithShuffle(t, db)

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-LEAVE-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-LEAVE-TGT")

	orderA := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dig-leave-a"
		o.Status = protocol.StatusDispatched
	})
	testdb.ClaimBinForTest(t, db, blocker.ID, orderA.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	orderB := mkDigOrder(t, db, "dig-leave-b", bp.Code, "LINE-LEAVE")

	if _, pe := d.planner.planBuriedReshuffle(orderB,
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID}); pe == nil || pe.Code != codeBlockerClaimed {
		t.Fatalf("fixture: the first attempt must park on the claimed blocker, got %v", pe)
	}

	// A's robot arrives and delivers: the bin leaves the lane and the claim
	// clears. TerminalizeOrder is the production release path for both.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(blocker.ID, shuffleSlots[3].ID, false),
		"move the blocker out of the lane")
	_, err := db.TerminalizeOrder(orderA.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "terminalize order A")

	// Same call, same order, no state reset — only the lane changed.
	_, pe := d.planner.planBuriedReshuffle(orderB, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})
	if pe != nil {
		t.Fatalf("the dig still will not plan after its blocker left the lane: %s: %s.\n"+
			"The wait has no releaser, which makes it a stall wearing a queue reason", pe.Code, pe.Detail)
	}

	kids, err := db.ListChildOrders(orderB.ID)
	testutil.MustNoErr(t, err, "list children of the dig")
	if len(kids) == 0 {
		t.Fatal("the dig reported success and created no legs")
	}

	// END-OF-SCENARIO LEDGER SWEEP. Order A went terminal holding a hard bin
	// claim and the confirmed reservation under it, and the dig it unblocked is
	// now live with legs and a lane lock of its own. The sweep separates the two:
	// everything the dead order held is gone, everything the live one holds is
	// untouched. See testdb.AssertNoOrphanedHolds.
	testdb.AssertNoOrphanedHolds(t, db)
}

// TestDig_BlockerSoftHeld_YieldsToAnOlderHolder is the flip this test asked for.
//
// It was TestDig_BlockerSoftHeld_StillSteals, pinning the unconditional steal
// against the open dig/soft-holder contract (plan §12.17,
// FINDINGS-compound-child-ledger-row-2026-08-09.md) and saying in its own doc
// that it was the thing to flip once that question was settled.
//
// It is settled (§7): a soft hold is a PROMISE, and the take at a positional
// blocker goes by the demand ranking. So the assertion is inverted on the
// fixture it always had — the holder is seeded first and both demands are
// priority 0, so the holder is older and keeps its bin. What this used to prove
// is now conditional on the dig outranking, and is pinned on that condition in
// ranked_take_docker_test.go.
func TestDig_BlockerSoftHeld_YieldsToAnOlderHolder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-SOFT-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-SOFT-TGT")

	// Soft: a pending reservation and nothing in claimed_by.
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dig-soft-holder"
		o.Status = protocol.StatusQueued
	})
	testdb.ReserveBin(t, db, holder.ID, blocker.ID)
	got, err := db.GetBin(blocker.ID)
	testutil.MustNoErr(t, err, "reload the soft-held bin")
	if got.ClaimedBy != nil {
		t.Fatalf("fixture: a soft hold must leave claimed_by NULL, got %v — this test would be "+
			"exercising the hard arm", got.ClaimedBy)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	orderB := mkDigOrder(t, db, "dig-soft-b", bp.Code, "LINE-SOFT")

	_, pe := d.planner.planBuriedReshuffle(orderB, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})
	if pe == nil {
		t.Fatal("the dig took a bin promised to an OLDER demand at the same priority. Ruling §7 " +
			"makes the take a ranked one: a dig that does not outrank the holder backs out and " +
			"waits, and the holder removing that bin is what clears the lane for it")
	}
	if pe.Code != codeBlockerClaimed {
		t.Errorf("the refusal came back as %q (%s), want the congestion code — an outranked dig is "+
			"a WAIT, and any other code fails the demand over somebody else's turn", pe.Code, pe.Detail)
	}
	after, aerr := db.GetOrder(orderB.ID)
	testutil.MustNoErr(t, aerr, "re-read the yielding dig")
	if after.QueueCause != string(CauseDigBlockerPromised) {
		t.Errorf("the yielding dig parked under %q, want %q — the holder has no robot, so the "+
			"releaser is not a drive", after.QueueCause, CauseDigBlockerPromised)
	}
	// And the holder kept its BOOK — the T3 shape on the real planner path. (This
	// fixture reserves without stamping bin_id, so the pointer half of the wedge
	// is asserted in the store-level suite where the holder carries one.)
	var res int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM reservations WHERE order_id=$1 AND bin_id=$2`,
		holder.ID, blocker.ID).Scan(&res), "count the winner's reservations")
	if res != 1 {
		t.Errorf("the winner holds %d reservation(s) on the contested bin, want 1. A refusal that "+
			"let the transaction continue falls through to supersedeBinLedger, which evicts the "+
			"whole bin's ledger — shredding the book of the order that WON.", res)
	}
}

// TestBlockerClaimedError_CarriesTheHolder — the refusal names who it is waiting
// on. Without it the queue reason can only say "somebody", and the first thing
// anyone asks about a stuck dig is which order has the bin.
func TestBlockerClaimedError_CarriesTheHolder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "BIN-HOLDER-ID")

	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "holder-id-stranger" })
	testdb.ClaimBinForTest(t, db, bin.ID, stranger.ID)

	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "holder-id-parent"
		o.Status = protocol.StatusReshuffling
	})
	pid := parent.ID
	_, err := db.CreateCompoundChildren([]store.CompoundChild{{
		Order: &orders.Order{
			EdgeUUID: "holder-id-child", StationID: parent.StationID,
			OrderType: protocol.OrderTypeMove, Status: protocol.StatusPending,
			Quantity: 1, ParentOrderID: &pid, Sequence: 1, BinID: &bin.ID,
		},
		BinID: bin.ID,
	}})

	if !errors.Is(err, store.ErrBlockerClaimed) {
		t.Fatalf("err = %v, want it to match store.ErrBlockerClaimed — the planners branch on this "+
			"to tell a congestion refusal from a fault", err)
	}
	var bc *store.BlockerClaimedError
	if !errors.As(err, &bc) {
		t.Fatalf("err = %v, want a *store.BlockerClaimedError", err)
	}
	if bc.HolderID != stranger.ID {
		t.Errorf("HolderID = %d, want %d", bc.HolderID, stranger.ID)
	}
	if bc.BinID != bin.ID {
		t.Errorf("BinID = %d, want %d", bc.BinID, bin.ID)
	}
}
