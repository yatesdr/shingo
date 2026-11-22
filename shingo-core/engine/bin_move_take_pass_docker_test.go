//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestBinMove_ATakenHolderIsReFoundAtOnce: a person's move that takes a bin
// another order had promised itself un-points that holder (TakeSoftHolds). The
// door then triggers a scan pass, so the holder re-finds another bin at once,
// as main's queue event did, instead of waiting for the 60 s floor; the door
// does not wait for that pass, so the test does, for far less than the floor.
// A move that takes nothing triggers no pass
// (TestAcquireOne_TriesOneOrderAndNotTheWaitingSet).
//
// RED at d0ca3617 (pins/bin-take-pass-red-d0ca3617.log): the holder is left
// un-pointed with the bin-taken cause until the floor.
func TestBinMove_ATakenHolderIsReFoundAtOnce(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	eng.fulfillment.RunOnce() // after the boot pass, so it asks nothing below

	store2 := &nodes.Node{Name: "TAKE-STORE-2", NodeTypeID: sd.StorageNode.NodeTypeID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(store2), "a second storage node")
	dest := &nodes.Node{Name: "TAKE-DEST", NodeTypeID: sd.LineNode.NodeTypeID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "the move's destination")
	taken := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "TAKE-B")
	other := testdb.CreateBinAtNode(t, db, sd.Payload.Code, store2.ID, "TAKE-C")

	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.StationUID = "take-holder"
		o.PayloadCode = sd.Payload.Code
		o.DeliveryNode = sd.LineNode.Name
		o.SourceIntent = "full"
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, taken.ID, "test"), "the holder's promise")
	testdb.HoldCart(t, db, holder.ID, taken.ID)

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: taken.Label, DestNodeName: dest.Name, StationID: "take-person",
	})
	testutil.MustNoErr(t, err, "the person's move")
	if res.Queued {
		t.Fatalf("the move parked (%s); this test needs it to take the bin", res.QueueReason)
	}

	var after *orders.Order
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		after, err = db.GetOrder(holder.ID)
		testutil.MustNoErr(t, err, "reload the holder")
		if after.Status == protocol.StatusDispatched || time.Now().After(deadline) {
			break
		}
	}
	if after.QueueCause == "bin-taken" {
		t.Errorf("the holder still waits with the bin-taken cause (%s, %q): nothing asked it after the take",
			after.Status, after.QueueReason)
	}
	if testdb.OnlyCart(t, after) == nil || *testdb.OnlyCart(t, after) != other.ID || after.Status != protocol.StatusDispatched {
		t.Errorf("holder is %s with bin %v, want dispatched with bin %d (%s)", after.Status, testdb.OnlyCart(t, after), other.ID, other.Label)
	}
}
