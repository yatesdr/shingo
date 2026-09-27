//go:build docker

package engine

import (
	"testing"
	"time"

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

// TestBinMove_APassCannotRunInsideTheDoorsAcquire: the race the door used to
// lose is closed by construction. Since B2 the door does not dispatch; it hands
// the move to Scanner.AcquireOne, which holds the scan lock, so a pass started
// while the door's hand is in flight waits until the door's acquire is done and
// then finds the move already dispatched. (The door's own yield,
// binMoveTakenByScanner, went with the inline hand. The mirror below, a hand
// inside the scanner's window, still exercises the scanner's yield.)
//
// It used to call RunOnce from inside the hand; that now deadlocks on the scan
// lock, which is the fact this pins.
func TestBinMove_APassCannotRunInsideTheDoorsAcquire(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-HAND-LOSES")
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	passDone := make(chan struct{})
	ranInside := false
	started := false
	eng.Dispatcher().SetPreHandHook(func(int64) {
		if started {
			return
		}
		started = true
		go func() {
			eng.fulfillment.RunOnce()
			close(passDone)
		}()
		select {
		case <-passDone:
			ranInside = true
		case <-time.After(200 * time.Millisecond):
		}
	})

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection:    BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "test-station",
		Desc:         "a pass tries to get there first",
	})
	if !started {
		t.Fatal("fixture: the pre-hand hook never fired")
	}
	if ranInside {
		t.Fatal("a scanner pass ran inside the door's acquire: the scan lock is not held across it")
	}
	testutil.MustNoErr(t, err, "CreateBinMove")
	<-passDone
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
