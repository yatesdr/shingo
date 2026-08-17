//go:build docker

package engine

import (
	"testing"
	"time"

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

	// The stored bin is now recorded at its slot, not stranded at _TRANSIT —
	// and STILL CLAIMED. This assertion read RequireBinUnclaimed until the
	// round-trip regression below showed what the unclaim cost at the far end:
	// a set-down mid-plan is not a handoff, and the order is coming back.
	testdb.RequireBinAtNode(t, db, binStore.ID, storeNode.ID)
	testdb.RequireBinClaimedBy(t, db, binStore.ID, ord.ID)

	// The in-flight empty is untouched — still claimed, still at its node.
	testdb.RequireBinAtNode(t, db, binRetr.ID, lineNode.ID)
	testdb.RequireBinClaimedBy(t, db, binRetr.ID, ord.ID)

	// Idempotent: replaying the same store block is a no-op, and retaining the
	// claim does not weaken that. resolveDropoffBin resolves by "the bin at
	// _TRANSIT under this order's claim" — a bin that has been set down is no
	// longer at _TRANSIT, so the replay matches nothing regardless of who holds
	// it. Idempotency is a property of WHERE the bin is, not of whether it is
	// claimed.
	//
	// Worth stating because it was not obvious: keeping the claim looked like it
	// must break this, and an explicit "already at this node, skip" guard was
	// written here on that assumption. Disabling the guard changed no observable
	// behaviour, which is how the assumption was found to be wrong. The guard is
	// gone; this assertion is what stands in its place.
	//
	// Asserted on updated_at rather than on the end state, because the end state
	// is identical either way: a re-applied store would move the bin to the node
	// it is already on. The row's timestamp is the only DB-visible difference —
	// and it stands in for the one that actually costs something, the duplicate
	// UOP adjustment this handler sends to Edge on every non-skipped pass. That
	// send has no observation seam in these tests (newTestEngine runs with
	// MsgClient nil), so this is the closest available pin on the same path.
	var beforeReplay time.Time
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT updated_at FROM bins WHERE id=$1`, binStore.ID).Scan(&beforeReplay),
		"read updated_at before replay")

	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "store-block-1-b3",
		Location: storeNode.Name,
		BinTask:  "JackUnload",
	})
	testdb.RequireBinAtNode(t, db, binStore.ID, storeNode.ID)
	testdb.RequireBinClaimedBy(t, db, binStore.ID, ord.ID)

	var afterReplay time.Time
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT updated_at FROM bins WHERE id=$1`, binStore.ID).Scan(&afterReplay),
		"read updated_at after replay")
	if !afterReplay.Equal(beforeReplay) {
		t.Fatalf("replayed store block re-applied the arrival: updated_at moved %v -> %v", beforeReplay, afterReplay)
	}
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

// TestBinRoundTrip_IntermediateStoreThenFinalDelivery pins the whole journey a
// swap's carrier actually makes: picked, PUT DOWN at staging, picked up again,
// delivered to the line. Nothing covered both halves before, and that gap is
// exactly how fe252c57 shipped.
//
// That commit fixed the first half — resolveDropoffBin had not matched these
// shapes, so the intermediate store silently no-op'd and the bin never landed
// at staging. Recording the store correctly then broke the second half:
// ApplyArrival unclaims, and applyBinArrivalForOrder's teleport guard refuses
// an unclaimed bin. The bin stopped stranding at staging and started stranding
// at _TRANSIT instead — one symptom traded for a worse one.
//
// Measured on the lane-stress rig, same plant and window, clean seed both ways:
//
//	                     5077373e (pre)   78824192 (post)
//	teleport skips             1                10
//	bins stranded              1                13
//	unrecorded stores         12                 0
//
// The invariant this test defends is that an order does not lose its bin by
// setting it down mid-plan. A store is not a handoff: the order is still
// responsible for that carrier and is coming back for it.
func TestBinRoundTrip_IntermediateStoreThenFinalDelivery(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	staging := sd.StorageNode // the cell's inbound staging slot
	lineNode := sd.LineNode

	ord := &orders.Order{
		EdgeUUID: "round-trip-1", StationID: "line-1",
		OrderType: dispatch.OrderTypeComplex, Status: dispatch.StatusInTransit,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name, ProcessNode: lineNode.Name,
		PayloadDesc: "swap",
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create complex order")

	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")
	carried := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "CARRIER-ROUNDTRIP")
	testdb.ClaimBinForTest(t, db, carried.ID, ord.ID)
	testutil.MustNoErr(t, db.InsertOrderBin(ord.ID, carried.ID, 1, "pickup", lineNode.Name, lineNode.Name),
		"order_bin: final dest is the LINE")

	// Leg 1: the robot sets the bin down at staging, mid-plan.
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID: ord.ID, BlockID: "round-trip-1-b3",
		Location: staging.Name, BinTask: "JackUnload",
	})
	testdb.RequireBinAtNode(t, db, carried.ID, staging.ID)

	// The claim survives the set-down. Without this the delivery below is
	// refused as a teleport and the bin strands.
	testdb.RequireBinClaimedBy(t, db, carried.ID, ord.ID)

	// Leg 2: picked back up and delivered to the line.
	ord.BinID = &carried.ID
	testutil.MustNoErr(t, db.UpdateOrderBinID(ord.ID, carried.ID), "set order bin for delivery")
	eng.applyBinArrivalForOrder(ord)

	// The bin reaches the line. It does not sit at _TRANSIT holding a carrier
	// out of circulation while the cell reads empty.
	testdb.RequireBinAtNode(t, db, carried.ID, lineNode.ID)
}
