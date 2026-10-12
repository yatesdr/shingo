//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestBinMove_ATakenHolderIsReFoundAtOnce: a person's move that takes a bin
// another order had promised itself un-points that holder (TakeSoftHolds). The
// door then runs a scan pass, so the holder re-finds another bin at once, as
// main's queue event did, instead of waiting for the 60 s floor. A move that
// takes nothing runs no pass (TestAcquireOne_TriesOneOrderAndNotTheWaitingSet).
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
		o.StationID = "take-holder"
		o.PayloadCode = sd.Payload.Code
		o.DeliveryNode = sd.LineNode.Name
		o.SourceIntent = "full"
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, taken.ID, "test"), "the holder's promise")
	testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, taken.ID), "the holder points at the bin")

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: taken.Label, DestNodeName: dest.Name, StationID: "take-person",
	})
	testutil.MustNoErr(t, err, "the person's move")
	if res.Queued {
		t.Fatalf("the move parked (%s); this test needs it to take the bin", res.QueueReason)
	}

	after, err := db.GetOrder(holder.ID)
	testutil.MustNoErr(t, err, "reload the holder")
	if after.QueueCause == "bin-taken" {
		t.Errorf("the holder still waits with the bin-taken cause (%s, %q): nothing asked it after the take",
			after.Status, after.QueueReason)
	}
	if after.BinID == nil || *after.BinID != other.ID || after.Status != protocol.StatusDispatched {
		t.Errorf("holder is %s with bin %v, want dispatched with bin %d (%s)", after.Status, after.BinID, other.ID, other.Label)
	}
}
