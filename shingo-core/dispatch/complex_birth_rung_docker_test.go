//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// ---------------------------------------------------------------------------
// COMPLEX IS BORN PENDING, LIKE EVERY ORDER (C1).
//
// Every family climbs the same ladder: born `pending`, `pending → dispatched`
// when the whole hand is had, otherwise parked `pending → queued` with a cause.
// A complex order has resolved steps and no bins at birth, and the first thing
// its one acquire does with it is go looking.
//
// What the birth rung must buy is the scanner picking the order up, and the
// scanner keys on IsAcquiring = {pending, queued}. (Before C1 complex was born
// `sourcing`, a rung no order can now be in.)
// ---------------------------------------------------------------------------

// TestComplexIntake_IsBornSourcing pins the literal (now `pending`; the name is
// the old rung's).
func TestComplexIntake_IsBornSourcing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// A step list Core can resolve but that the scanner is NOT going to be able
	// to finish here — no dispatcher tick runs in this test, so the row stays as
	// intake left it, which is the whole point of the assertion.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-born-sourcing",
		PayloadCode: bp.Code,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
			{Action: protocol.ActionWait},
			{Action: protocol.ActionPickup, Node: lineNode.Name},
		},
	})

	got, err := db.GetOrderByUUID("complex-born-sourcing")
	testutil.MustNoErr(t, err, "get the order intake created")
	if got.Status != StatusPending {
		t.Errorf("a complex order was born %q, want %q.\n"+
			"Every order is born `pending`; its one acquire takes the whole hand or parks it "+
			"`queued` with a cause. Born `queued` it would claim a wait with no cause, which was "+
			"never true of it for a single tick.",
			got.Status, StatusPending)
	}
}

// TestComplexIntake_BornSourcingIsScannedAllocatedAndDispatched is the
// anti-wedge proof, and it is the assertion that matters more than the literal.
//
// A birth rung that nothing picks up is a wedge, and the caution is real.
// `pending` is inside IsAcquiring (C1), so the claim is that the ordinary
// machinery carries a born-pending order the whole way. Asserted end to end
// rather than argued.
func TestComplexIntake_BornSourcingIsScannedAllocatedAndDispatched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Material on the shelf, so the hunt can actually succeed.
	testdb.CreateBinAtNode(t, db, bp.Code, storage.ID, "CBS-BIN")

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-born-sourcing-e2e",
		PayloadCode: bp.Code,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: storage.Name},
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("complex-born-sourcing-e2e")
	testutil.MustNoErr(t, err, "get the order")
	if order.Status != StatusPending {
		t.Fatalf("fixture: born %q, not %q — this test proves nothing about the birth rung",
			order.Status, StatusPending)
	}

	// THE ORDINARY MACHINERY, entered exactly as the scanner enters it. The
	// scanner's own gate is IsAcquiring, which `pending` satisfies; this is the
	// call it makes next.
	if derr := d.DispatchPreparedComplex(order); derr != nil {
		t.Fatalf("a born-`pending` complex order did not dispatch: %v\n"+
			"If the rung it is born on is not one the scanner drives, the order is a wedge from "+
			"the moment it exists.", derr)
	}

	final, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read")
	if final.VendorOrderID == "" {
		t.Error("the order never reached the fleet — it was scanned but not dispatched")
	}
	if protocol.IsAcquiring(final.Status) {
		t.Errorf("status = %q — still acquiring after a successful dispatch", final.Status)
	}
	if final.BinID == nil {
		t.Error("the order dispatched holding no bin — the hunt is what its acquire does, " +
			"and it has to have finished")
	}
}

// TestComplexIntake_TheBirthRowIsTheEpisodeTheCauseLandsOn is the seam between
// the birth rung and the queue-detail stamp.
//
// SetQueueDetail aims a cause at the row of the episode the order is RESTING in.
// The birth row is `pending` (C1), a birth certificate and not a wait, so the
// stamp writes no row there: the cause rides the park onto the `queued` row it
// opens. One wait, one row, and it is the wait's own. (The name is the old
// rung's, when the birth row itself took the cause.)
func TestComplexIntake_TheBirthRowIsTheEpisodeTheCauseLandsOn(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-birth-episode",
		PayloadCode: bp.Code,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
			{Action: protocol.ActionWait},
			{Action: protocol.ActionPickup, Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("complex-birth-episode")
	testutil.MustNoErr(t, err, "get the order")

	// Park it under a cause the way the scanner does: the cause on the row, then
	// the park.
	d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseReserveHolding, QueueParams{Payload: bp.Code})
	testutil.MustNoErr(t, d.ParkAndTell(order, "test"), "park")

	history, err := db.ListOrderHistory(order.ID)
	testutil.MustNoErr(t, err, "list history")
	if len(history) != 2 {
		t.Fatalf("history has %d rows, want 2 (the birth row, then the park's own row)", len(history))
	}
	if history[0].Status != protocol.StatusPending || history[0].Code != "" {
		t.Errorf("history[0] = %q/%q, want the bare `pending` birth row", history[0].Status, history[0].Code)
	}
	if history[1].Status != protocol.StatusQueued {
		t.Errorf("history[1] says %q, want %q: the park opens the wait's own row", history[1].Status, protocol.StatusQueued)
	}
	if history[1].Code != string(protocol.QueueWaitingForMaterial) {
		t.Errorf("the park's row carries code %q, want %q — orders.queue_code is overwritten in "+
			"place, so this row is the only durable record of what the order waited for",
			history[1].Code, protocol.QueueWaitingForMaterial)
	}
}
