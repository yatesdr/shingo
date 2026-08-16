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
	//
	// THE STORED BIN IS SEEDED AT _TRANSIT, because that is where a bin about to
	// be dropped actually is. It was seeded at the LINE node — its source — which
	// is a state that cannot occur: a dropoff places a bin the robot is holding,
	// and holding it is exactly what the pickup handler in this file records by
	// moving it to _TRANSIT. The fixture was modelling a bin being put down
	// without ever having been picked up, and it passed only because the resolver
	// it exercised never asked where the bin was.
	//
	// The retrieve bin stays at the line node: it has NOT been picked yet, which
	// is the whole point of the last two assertions.
	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")
	binStore := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "CARRIER-STORE")
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

// TestHandleStoreBlockCompleted_StagingDropWithNoJunctionRowForIt is the
// three-pinned-robots wedge, reproduced.
//
// A two_robot swap drops its bin at the cell's INBOUND STAGING node on the way
// to the cell, then picks it up again from staging. The junction records PICKUP
// rows only — node_name is where a bin is picked, dest_node where it finally
// goes — so NO row's dest_node is ever the staging node. The old resolver
// matched on dest_node, found nothing, logged "no in-flight claimed bin matched"
// and returned: the bin was never recorded at staging, the order's OWN next step
// (a pickup there) had nothing to pick, and it sat at its wait holding an AMR.
//
// Measured on the lane-stress rig 2026-08-11: orders 7 and 10 staged from the
// first minute of the run to the end of the soak, one robot each.
//
// MUTATION (fires): resolve by matching a junction row whose dest_node equals
// the location, and the bin is never recorded at staging.
func TestHandleStoreBlockCompleted_StagingDropWithNoJunctionRowForIt(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	staging := sd.StorageNode // stands in for the cell's inbound staging slot
	lineNode := sd.LineNode

	ord := &orders.Order{
		EdgeUUID: "staging-drop-1", StationID: "line-1",
		OrderType: dispatch.OrderTypeComplex, Status: dispatch.StatusInTransit,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name, ProcessNode: lineNode.Name,
		PayloadDesc: "swap",
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create complex order")

	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")
	carried := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "CARRIER-STAGED")
	testdb.ClaimBinForTest(t, db, carried.ID, ord.ID)

	// The junction as the allocator writes it: the bin's FINAL destination is the
	// line, and nothing anywhere names the staging node it is about to be put
	// down at.
	testutil.MustNoErr(t, db.InsertOrderBin(ord.ID, carried.ID, 1, "pickup", lineNode.Name, lineNode.Name),
		"order_bin: dest is the LINE, not the staging slot")

	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID: ord.ID, BlockID: "staging-drop-1-b3",
		Location: staging.Name, BinTask: "JackUnload",
	})

	testdb.RequireBinAtNode(t, db, carried.ID, staging.ID)
}

// TestHandleStoreBlockCompleted_SingleBinOrderHasNoJunctionAtAll is the same
// wedge reached from the other side.
//
// The allocator writes junction rows only for MULTI-bin orders, so a single-bin
// complex order has none — dispatch.binForStep says so, and its sibling
// resolvePickupBin has carried an order.BinID fallback all along. The dropoff
// half never got one: it exited at len(rows)==0 and the store was never
// recorded.
//
// Measured: order 1 on the same run — bin 5, claimed by order 1, zero junction
// rows, staged holding an AMR for the whole soak.
func TestHandleStoreBlockCompleted_SingleBinOrderHasNoJunctionAtAll(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	staging := sd.StorageNode
	lineNode := sd.LineNode

	ord := &orders.Order{
		EdgeUUID: "single-bin-1", StationID: "line-1",
		OrderType: dispatch.OrderTypeComplex, Status: dispatch.StatusInTransit,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name, ProcessNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create complex order")

	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")
	only := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "CARRIER-ONLY")
	testdb.ClaimBinForTest(t, db, only.ID, ord.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(ord.ID, only.ID), "bin_id")
	// Deliberately NO InsertOrderBin — that is the shape.

	rows, err := db.ListOrderBins(ord.ID)
	testutil.MustNoErr(t, err, "list junction")
	if len(rows) != 0 {
		t.Fatalf("fixture: %d junction rows, want 0 — this test is only a test while there are none",
			len(rows))
	}

	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID: ord.ID, BlockID: "single-bin-1-b3",
		Location: staging.Name, BinTask: "JackUnload",
	})

	testdb.RequireBinAtNode(t, db, only.ID, staging.ID)
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
