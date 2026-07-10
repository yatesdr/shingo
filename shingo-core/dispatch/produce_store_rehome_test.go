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
