//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestHandleOrderIngest_SimpleProduce_RehomesAsCoordinated pins the PR1 re-backing:
// a simple-mode produce ingest (ManifestOnly=false, i.e. no swap carrying the bin)
// no longer mints a plain store order — that family was removed. Instead it applies
// the produce manifest to the bin and dispatches a COORDINATED
// [pickup@produce, dropoff@storage] order through the shared complex intake, with the
// storage destination resolved via FindStorageDestination. The order must reach
// dispatch end-to-end.
func TestHandleOrderIngest_SimpleProduce_RehomesAsCoordinated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db) // creates STORAGE-A1 (STOR), LINE1-IN, PART-A, DEFAULT

	bt, _ := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(bp.ID, []int64{bt.ID}), "set payload bin types")

	// A produce node holding the freshly-filled bin (the press output).
	produceNode := &nodes.Node{Name: "PRODUCE-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(produceNode), "create produce node")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "PRODUCE-BIN-1", NodeID: &produceNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create produce bin")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	const operatorMeasured = 500
	// Simple-mode produce ingest: no ManifestOnly flag → a store move is needed.
	d.HandleOrderIngest(testEnvelope(), &protocol.OrderIngestRequest{
		OrderUUID:   "ingest-rehome-1",
		PayloadCode: bp.Code,
		BinLabel:    bin.Label,
		SourceNode:  produceNode.Name,
		Quantity:    operatorMeasured,
		Manifest: []protocol.IngestManifestItem{
			{PartNumber: bp.Code, Quantity: operatorMeasured, Description: bp.Code},
		},
	})

	order, err := db.GetOrderByUUID("ingest-rehome-1")
	if err != nil {
		t.Fatalf("simple-mode produce ingest must create an order: %v", err)
	}

	// It must be a COORDINATED order (not a plain store — that path is gone).
	if order.OrderType != OrderTypeComplex {
		t.Fatalf("produce-store must re-home as a coordinated order, got type=%s", order.OrderType)
	}
	if order.StepsJSON == "" {
		t.Fatal("coordinated produce-store order must carry a persisted step plan")
	}
	// Plan shape: [pickup@produce, dropoff@storage].
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(order.StepsJSON), &steps), "unmarshal steps_json")
	if len(steps) != 2 ||
		steps[0].Action != protocol.ActionPickup || steps[0].Node != produceNode.Name ||
		steps[1].Action != protocol.ActionDropoff || steps[1].Node != "STORAGE-A1" {
		t.Fatalf("want [pickup@%s, dropoff@STORAGE-A1], got %+v", produceNode.Name, steps)
	}

	// And it must be dispatchable. The dispatcher-only harness has no scanner, so
	// drive the dispatch step the scanner would (mirrors submitComplexAndDispatch).
	if order.Status == StatusQueued {
		testutil.MustNoErr(t, d.DispatchPreparedComplex(order), "dispatch coordinated produce-store")
		order, err = db.GetOrderByUUID("ingest-rehome-1")
		testutil.MustNoErr(t, err, "re-get order after dispatch")
	}
	if order.Status != StatusDispatched {
		t.Fatalf("coordinated produce-store must reach dispatched, got status=%s", order.Status)
	}
	if order.VendorOrderID == "" {
		t.Error("dispatched coordinated produce-store must carry a vendor order id")
	}

	// The manifest was applied to the bin (operator-measured runtime UOP), regardless
	// of the subsequent dispatch — the produce count is recorded at ingest.
	gotBin, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if gotBin.UOPRemaining != operatorMeasured {
		t.Errorf("bin UOP = %d, want %d (operator-measured runtime)", gotBin.UOPRemaining, operatorMeasured)
	}
}

// TestHandleOrderIngest_SwapMode_ManifestOnlyNoOrder pins the other half of the
// re-backing contract: swap-mode produce (ManifestOnly=true) records the manifest
// and creates NO order — the swap already carries the bin. This behavior is
// unchanged by the store-family removal.
func TestHandleOrderIngest_SwapMode_ManifestOnlyNoOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	bt, _ := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(bp.ID, []int64{bt.ID}), "set payload bin types")

	produceNode := &nodes.Node{Name: "PRODUCE-2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(produceNode), "create produce node")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "PRODUCE-BIN-2", NodeID: &produceNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create produce bin")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	const measured = 300
	d.HandleOrderIngest(testEnvelope(), &protocol.OrderIngestRequest{
		OrderUUID:    "ingest-swap-1",
		PayloadCode:  bp.Code,
		BinLabel:     bin.Label,
		SourceNode:   produceNode.Name,
		Quantity:     measured,
		ManifestOnly: true,
		Manifest: []protocol.IngestManifestItem{
			{PartNumber: bp.Code, Quantity: measured, Description: bp.Code},
		},
	})

	if _, err := db.GetOrderByUUID("ingest-swap-1"); err == nil {
		t.Fatal("swap-mode (manifest-only) ingest must NOT create an order")
	}
	gotBin, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if gotBin.UOPRemaining != measured {
		t.Errorf("bin UOP = %d, want %d (manifest recorded even without an order)", gotBin.UOPRemaining, measured)
	}
}
