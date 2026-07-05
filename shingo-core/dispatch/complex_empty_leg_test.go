//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestReserveConfirm_EmptyLegClaimsEmptyCarrier is the produce-node fix, ported to
// the 1c reserve/confirm split (D39): a complex swap stores the produced full and
// fetches an EMPTY carrier to refill the press. The empty pickup leg (step.Empty)
// must reserve+claim an empty carrier, NOT a payload-matching full sitting in the
// same source — the Hopkinsville bug where the press kept being handed full bins.
//
// Source node holds BOTH a full PART-A bin and an empty carrier. The empty-leg
// filter lives in reserveComplexPlan.findAvailableForNeed (emptyBinsOnly), so the
// reserve selects the empty and confirm claims exactly it.
func TestReserveConfirm_EmptyLegClaimsEmptyCarrier(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storeNode, lineNode, bp := setupTestData(t, db)

	// Supermarket the press pulls empties from — holds a full AND an empty.
	src := &nodes.Node{Name: "EMPTY-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create empty-source node")

	fullAtSrc := &bins.Bin{BinTypeID: 1, Label: "SRC-FULL", NodeID: &src.ID, Status: "available", PayloadCode: bp.Code}
	testutil.MustNoErr(t, db.CreateBin(fullAtSrc), "create full bin at source")
	emptyAtSrc := &bins.Bin{BinTypeID: 1, Label: "SRC-EMPTY", NodeID: &src.ID, Status: "available", PayloadCode: ""}
	testutil.MustNoErr(t, db.CreateBin(emptyAtSrc), "create empty carrier at source")

	// The produced full sitting on the line, picked up to be stored.
	lineFull := &bins.Bin{BinTypeID: 1, Label: "LINE-FULL", NodeID: &lineNode.ID, Status: "available", PayloadCode: bp.Code}
	testutil.MustNoErr(t, db.CreateBin(lineFull), "create line full bin")

	order := &orders.Order{
		EdgeUUID:     "uuid-empty-leg",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		ProcessNode:  lineNode.Name,
		DeliveryNode: lineNode.Name,
		PayloadCode:  bp.Code,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// pickup line full → store at storeNode → pickup EMPTY at src → deliver to line.
	steps := []resolvedStep{
		{Action: "wait", Node: lineNode.Name},
		{Action: "pickup", Node: lineNode.Name},
		{Action: "dropoff", Node: storeNode.Name},
		{Action: "pickup", Node: src.Name, Empty: true},
		{Action: "dropoff", Node: lineNode.Name},
	}
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, order.ProcessNode)
	assigned, outcome, rerr := d.allocator.reserveComplexPlan(order, plan)
	if rerr != nil {
		t.Fatalf("reserveComplexPlan: %v", rerr)
	}
	if outcome != reserveComplete {
		t.Fatalf("reserveComplexPlan outcome = %v, want reserveComplete — line full and src empty are both available", outcome)
	}
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned); cerr != nil {
		t.Fatalf("confirmComplexPlan: %v", cerr)
	}

	// The empty carrier is claimed by this order; the full at the same source is NOT.
	gotEmpty, _ := db.GetBin(emptyAtSrc.ID)
	if gotEmpty.ClaimedBy == nil || *gotEmpty.ClaimedBy != order.ID {
		t.Errorf("empty carrier claimed_by = %v, want order %d — the empty leg must claim the empty", gotEmpty.ClaimedBy, order.ID)
	}
	gotFull, _ := db.GetBin(fullAtSrc.ID)
	if gotFull.ClaimedBy != nil {
		t.Errorf("full bin at source claimed_by = %v, want nil — the empty leg must NOT claim the full", *gotFull.ClaimedBy)
	}

	// The order_bins row for the source node points at the empty carrier.
	obs, err := db.ListOrderBins(order.ID)
	if err != nil {
		t.Fatalf("ListOrderBins: %v", err)
	}
	var srcBinID int64
	for _, ob := range obs {
		if ob.NodeName == src.Name {
			srcBinID = ob.BinID
		}
	}
	if srcBinID != emptyAtSrc.ID {
		t.Errorf("order_bins source-leg bin = %d, want empty carrier %d", srcBinID, emptyAtSrc.ID)
	}
}
