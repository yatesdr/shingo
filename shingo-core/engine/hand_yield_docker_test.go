//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// hand_yield_docker_test.go — two dispatchers on one order, and the one that
// loses.
//
// A hand dispatch moves a pending order to `queued` before it takes its hand, so
// a scanner pass in between can take the same order through its held-bin arm.
// Under A1 the `→ dispatched` CAS runs inside the hand's transaction, before the
// fleet call, so the loser has taken nothing and created no vendor order. The CAS
// is the detector; the loser yields (SYNTH §2.4). What it must not do is treat
// the lost CAS as a fleet failure: fail the order, release its claim, or demote
// its holds, all of which now belong to the winner's dispatch.

// assertDispatchedWithItsClaim checks the order is dispatched, holds its bin, and
// has exactly the one vendor order the winner created.
func assertDispatchedWithItsClaim(t *testing.T, db *store.DB, sim *simulator.SimulatorBackend, orderID, binID int64) *orders.Order {
	t.Helper()
	got, err := db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "reload the order")
	if got.Status != protocol.StatusDispatched {
		t.Errorf("order %d is %q (%s), want dispatched — the winner dispatched it and a robot has it",
			orderID, got.Status, got.ErrorDetail)
	}
	if got.VendorOrderID == "" || !sim.HasOrder(got.VendorOrderID) {
		t.Errorf("order %d names vendor order %q, which the fleet does not have", orderID, got.VendorOrderID)
	}
	if n := sim.OrderCount(); n != 1 {
		t.Errorf("the fleet holds %d vendor orders, want 1 — the loser must create none", n)
	}
	bin, err := db.GetBin(binID)
	testutil.MustNoErr(t, err, "reload the bin")
	if bin.ClaimedBy == nil || *bin.ClaimedBy != orderID {
		t.Errorf("bin %d is claimed by %v, want order %d — the loser released the winner's claim",
			binID, bin.ClaimedBy, orderID)
	}
	return got
}

// TestHandDispatch_LosingTheCASToTheScannerYields: an operator's bin move whose
// order the scanner takes inside the window reports the move dispatched.
//
// MUTATION: drop the yield in CreateBinMove. The lost CAS takes the fleet-failure
// arm: failOrderAndEmit fails the scanner's dispatched order, ReleaseClaimForBin
// drops its claim, and the operator is told the fleet did not accept it.
func TestHandDispatch_LosingTheCASToTheScannerYields(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-HAND-LOSES")
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	scannerTook := false
	eng.Dispatcher().SetPreHandHook(func(int64) {
		if scannerTook {
			return
		}
		scannerTook = true
		eng.fulfillment.RunOnce() // the scanner takes the queued order here
	})

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection:    BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "test-station",
		Desc:         "the scanner gets there first",
	})
	if !scannerTook {
		t.Fatal("fixture: the pre-hand hook never fired")
	}
	if err != nil {
		t.Fatalf("CreateBinMove: %v — the move was dispatched, by the scanner; losing the CAS to it is "+
			"not a failure of the move", err)
	}
	got := assertDispatchedWithItsClaim(t, db, sim, res.OrderID, bin.ID)
	if res.VendorOrderID != got.VendorOrderID {
		t.Errorf("CreateBinMove reported vendor order %q, want the one the order carries (%q)",
			res.VendorOrderID, got.VendorOrderID)
	}
}

// TestScanner_LosingTheCASToAHandYields: the mirror. A scanner pass whose order
// a hand dispatch takes inside the window leaves that dispatch alone.
//
// MUTATION: drop the yield in the scanner's held-bin arm. The lost CAS is read as
// a fleet refusal and DemoteAfterFleetRefusal clears every claim under the order's
// id: the hand's dispatched order loses its bin.
func TestScanner_LosingTheCASToAHandYields(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-SCANNER-LOSES")
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	handTook := false
	eng.Dispatcher().SetPreHandHook(func(orderID int64) {
		if handTook {
			return
		}
		handTook = true
		fresh, err := db.GetOrder(orderID)
		testutil.MustNoErr(t, err, "hook: reload the order")
		_, err = eng.Dispatcher().DispatchHand(fresh, bin.ID, storageNode, lineNode)
		testutil.MustNoErr(t, err, "hook: the hand dispatch that wins")
	})

	binID := bin.ID
	order := &orders.Order{
		EdgeUUID:     "scanner-loses",
		StationID:    "test-station",
		OrderType:    protocol.OrderTypeMove,
		Status:       protocol.StatusQueued,
		Quantity:     1,
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		BinID:        &binID,
		OriginClass:  protocol.OriginClassNoDemand,
		SourceIntent: dispatch.SourceIntentLocal,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create the queued held-bin order")
	// A queued held-bin order holds its bin SOFT (a pending reservation); the hand
	// is what hard-claims it.
	testutil.MustNoErr(t, reservations.Acquire(db, order.ID, order.ID, bin.ID, "test"), "soft-hold the bin")

	eng.fulfillment.RunOnce() // or the boot pass, if it gets there first; the hook fires once either way
	if !handTook {
		t.Fatal("fixture: the pre-hand hook never fired — no scanner pass reached the hand")
	}
	assertDispatchedWithItsClaim(t, db, sim, order.ID, bin.ID)
}
