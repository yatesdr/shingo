//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestHandleOrderIngest_RecordsManifestNoOrder pins the surviving ingest
// contract: a produce ingest records the bin's manifest and creates NO order —
// ingest is a manifest-only inventory write (the swap carries the bin).
func TestHandleOrderIngest_RecordsManifestNoOrder(t *testing.T) {
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
		OrderUUID:   "ingest-swap-1",
		PayloadCode: bp.Code,
		BinLabel:    bin.Label,
		SourceNode:  produceNode.Name,
		Quantity:    measured,
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

// TestHandleOrderIngest_BinIDPinsExactBin — Fix D: the release-time produce
// manifest pins the departing bin by id. By the time Core processes a deferred
// ingest, a press-index R2 may have indexed the fresh tote onto the manifest
// tote's position, and node-based resolution (which prefers a payload match)
// would credit the wrong bin. An explicit BinID must win over every node-based
// choice.
func TestHandleOrderIngest_BinIDPinsExactBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	bt, _ := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(bp.ID, []int64{bt.ID}), "set payload bin types")

	press := &nodes.Node{Name: "PRESS-BINID", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create press node")
	// The node-resolution favorite: co-located and payload-matching.
	decoy := &bins.Bin{BinTypeID: bt.ID, Label: "FRESH-TOTE", NodeID: &press.ID, Status: "available", PayloadCode: bp.Code}
	testutil.MustNoErr(t, db.CreateBin(decoy), "create decoy bin")
	// The departing tote the manifest is actually for.
	departing := &bins.Bin{BinTypeID: bt.ID, Label: "DEPARTING-TOTE", NodeID: &press.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(departing), "create departing bin")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	const measured = 61
	d.HandleOrderIngest(testEnvelope(), &protocol.OrderIngestRequest{
		OrderUUID:   "ingest-binid-1",
		PayloadCode: bp.Code,
		BinID:       departing.ID,
		SourceNode:  press.Name,
		Quantity:    measured,
		Manifest: []protocol.IngestManifestItem{
			{PartNumber: bp.Code, Quantity: measured, Description: bp.Code},
		},
	})

	gotDeparting, err := db.GetBin(departing.ID)
	testutil.MustNoErr(t, err, "get departing")
	if gotDeparting.UOPRemaining != measured {
		t.Errorf("departing bin UOP = %d, want %d — BinID must pin the manifest target", gotDeparting.UOPRemaining, measured)
	}
	gotDecoy, err := db.GetBin(decoy.ID)
	testutil.MustNoErr(t, err, "get decoy")
	if gotDecoy.UOPRemaining == measured {
		t.Error("payload-matching co-located bin got the manifest — node resolution overrode the explicit BinID")
	}
}
