//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestHandleStoreBlockCompleted_RecordsIntermediateStore covers the
// Hopkinsville #130/#132 divergence: a complex swap stores its line bin at a
// supermarket slot mid-order, then retrieves an empty and continues. Before
// the fix the store dropoff was a no-op, so the stored bin stayed recorded at
// _TRANSIT (its slot read empty) until whole-order FINISHED — and a mid-flight
// cancel stranded it while a downstream order could double-store the occupied
// slot. handleStoreBlockCompleted now lands the stored bin at its slot the
// moment the store block finishes.
func TestHandleStoreBlockCompleted_RecordsIntermediateStore(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	storeNode := sd.StorageNode // intermediate store destination
	lineNode := sd.LineNode     // final delivery (and line pickup)

	// Two-pickup complex swap: pick line bin → store at storeNode → retrieve
	// empty → deliver empty to lineNode.
	ord := &orders.Order{
		EdgeUUID:     "store-block-1",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       dispatch.StatusInTransit,
		SourceNode:   lineNode.Name,
		DeliveryNode: lineNode.Name,
		ProcessNode:  lineNode.Name,
		PayloadDesc:  "swap",
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create complex order")

	// The full line bin being stored and the empty being retrieved — both
	// claimed by the order (in flight). Junction rows carry each bin's dest.
	binStore := testdb.CreateBinAtNode(t, db, sd.Payload.Code, lineNode.ID, "CARRIER-STORE")
	binRetr := testdb.CreateBinAtNode(t, db, sd.Payload.Code, lineNode.ID, "CARRIER-RETR")
	testdb.ClaimBinForTest(t, db, binStore.ID, ord.ID)
	testdb.ClaimBinForTest(t, db, binRetr.ID, ord.ID)
	testutil.MustNoErr(t, db.InsertOrderBin(ord.ID, binStore.ID, 1, "pickup", lineNode.Name, storeNode.Name), "order_bin store leg")
	testutil.MustNoErr(t, db.InsertOrderBin(ord.ID, binRetr.ID, 3, "pickup", "EMPTY-SRC", lineNode.Name), "order_bin retrieve leg")

	// Store dropoff block finishes at the supermarket slot.
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "store-block-1-b3",
		Location: storeNode.Name,
		BinTask:  "JackUnload",
	})

	// The stored bin is now recorded at its slot and unclaimed — a normal
	// stored bin, not stranded at _TRANSIT.
	testdb.RequireBinAtNode(t, db, binStore.ID, storeNode.ID)
	testdb.RequireBinUnclaimed(t, db, binStore.ID)

	// The in-flight empty is untouched — still claimed, still at its node.
	testdb.RequireBinAtNode(t, db, binRetr.ID, lineNode.ID)
	testdb.RequireBinClaimedBy(t, db, binRetr.ID, ord.ID)

	// Idempotent: replaying the same store block is a no-op — the bin is now
	// unclaimed, so resolveDropoffBin won't re-match it.
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "store-block-1-b3",
		Location: storeNode.Name,
		BinTask:  "JackUnload",
	})
	testdb.RequireBinAtNode(t, db, binStore.ID, storeNode.ID)
	testdb.RequireBinUnclaimed(t, db, binStore.ID)
}

// TestHandleStoreBlockCompleted_SkipsFinalDelivery verifies the final delivery
// dropoff is NOT recorded early here — it stays on the whole-order FINISHED
// path (handleOrderDelivered), which also ships the Edge OrderDelivered notice.
func TestHandleStoreBlockCompleted_SkipsFinalDelivery(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	lineNode := sd.LineNode

	ord := &orders.Order{
		EdgeUUID:     "store-block-2",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       dispatch.StatusInTransit,
		SourceNode:   lineNode.Name,
		DeliveryNode: lineNode.Name,
		ProcessNode:  lineNode.Name,
		PayloadDesc:  "swap",
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create complex order")

	binRetr := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CARRIER-RETR2")
	testdb.ClaimBinForTest(t, db, binRetr.ID, ord.ID)
	testutil.MustNoErr(t, db.InsertOrderBin(ord.ID, binRetr.ID, 3, "pickup", "EMPTY-SRC", lineNode.Name), "order_bin retrieve leg")

	// Dropoff at the FINAL delivery node — must be skipped here.
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "store-block-2-b5",
		Location: lineNode.Name,
		BinTask:  "JackUnload",
	})

	// Untouched: still claimed, still at its source node (no early arrival).
	testdb.RequireBinAtNode(t, db, binRetr.ID, sd.StorageNode.ID)
	testdb.RequireBinClaimedBy(t, db, binRetr.ID, ord.ID)
}
