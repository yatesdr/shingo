//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// ---------------------------------------------------------------------------
// COMPLEX IS BORN SHOPPING.
//
// Every family climbs the same ladder, and `sourcing` is the rung the record's
// split test assigns to a hand that is not whole: a complex order has resolved
// steps and no bins, and the first thing the scanner does with it is go looking.
//
// Birth-`queued` was visibility doing lifecycle's job. What the literal bought
// was the scanner picking the order up, and the scanner keys on IsAcquiring =
// {queued, sourcing}, so it buys the same thing either way — which is exactly
// why this move is safe and why nothing observed the old rung: MoveToSourcing
// runs at the top of the first tick, and EmitOrderQueued runs that tick
// synchronously.
// ---------------------------------------------------------------------------

// TestComplexIntake_IsBornSourcing pins the literal.
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
	if got.Status != StatusSourcing {
		t.Errorf("a complex order was born %q, want %q.\n"+
			"Its hand is not whole at birth — it holds resolved steps and no bins — and `sourcing` "+
			"is the rung that means the material hunt. Born `queued` it claimed a complete paper "+
			"hand waiting only for its turn, which was never true of it for a single tick.",
			got.Status, StatusSourcing)
	}
}

// TestComplexIntake_BornSourcingIsScannedAllocatedAndDispatched is the
// anti-wedge proof, and it is the assertion that matters more than the literal.
//
// A birth rung that nothing picks up is a wedge, and the caution is real — it is
// exactly what born-`pending` would have been, since `pending` is outside
// IsAcquiring and no scanner pass selects it. `sourcing` is inside, so the claim
// is that the ordinary machinery carries a born-sourcing order the whole way.
// Asserted end to end rather than argued.
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
	if order.Status != StatusSourcing {
		t.Fatalf("fixture: born %q, not %q — this test proves nothing about the new rung",
			order.Status, StatusSourcing)
	}

	// THE ORDINARY MACHINERY, entered exactly as the scanner enters it. The
	// scanner's own gate is IsAcquiring, which `sourcing` satisfies; this is the
	// call it makes next.
	if derr := d.DispatchPreparedComplex(order); derr != nil {
		t.Fatalf("a born-`sourcing` complex order did not dispatch: %v\n"+
			"If the rung it is born on is not one the scanner drives, the order is a wedge from "+
			"the moment it exists — the caution the born-`pending` question raised, arriving "+
			"through the door it was not asked about.", derr)
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
		t.Error("the order dispatched holding no bin — the hunt is what `sourcing` names, " +
			"and it has to have finished")
	}
}

// TestComplexIntake_TheBirthRowIsTheEpisodeTheCauseLandsOn is the seam between
// this move and the queue-detail stamp.
//
// Intake writes a queue cause immediately after CreateOrder for a capacity-
// blocked order, and SetQueueDetail aims that cause at the row of the episode the
// order is RESTING in. The birth row is now `sourcing`, so the cause lands there.
// The old spelling of that stamp named 'queued' explicitly and, by its own note,
// "a complex order born `queued` matched no row at all" — this is the same class
// of mis-aim, and the reason the rung move needs an assertion here rather than an
// argument.
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

	// Park it under a cause the way the gates do, then read the history back.
	testutil.MustNoErr(t, db.SetOrderQueueDetail(order.ID,
		"Waiting for material: "+bp.Code, protocol.QueueWaitingForMaterial, string(CauseReserveHolding)),
		"stamp a wait")

	history, err := db.ListOrderHistory(order.ID)
	testutil.MustNoErr(t, err, "list history")
	if len(history) != 1 {
		t.Fatalf("history has %d rows, want 1 (the birth row) — the stamp must land ON the birth "+
			"episode, not append beside it", len(history))
	}
	if history[0].Status != protocol.StatusSourcing {
		t.Errorf("the birth history row says %q, want %q", history[0].Status, protocol.StatusSourcing)
	}
	if history[0].Code != string(protocol.QueueWaitingForMaterial) {
		t.Errorf("the birth row carries code %q, want %q — orders.queue_code is overwritten in "+
			"place, so this row is the only durable record of what the order waited for",
			history[0].Code, protocol.QueueWaitingForMaterial)
	}
}
