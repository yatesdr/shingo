//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
)

// The missing-source case against the real store, through intake: a retrieve
// naming a node that does not exist is admitted (checkOrderRefs checks the
// payload and the delivery node, not the source), and the finder disposes of
// it. It waits as finder-source-missing, naming the node, and takes nothing
// from elsewhere. At e83cccd1 (c82c8654) it parked as loader-source-unreadable:
// tier 2 re-read the name, got sql.ErrNoRows from nodes.ScanNode, and
// reported the miss as a read failure.
func TestIntake_MissingSource_WaitsNamingIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storage.ID, "MISSING-SRC-FULL") // a full elsewhere it must not take

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	const uuid = "missing-source-1"
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: uuid, OrderType: OrderTypeRetrieve, PayloadCode: bp.Code, Quantity: 1,
		DeliveryNode: line.Name, SourceNode: "GONE-NODE",
	})
	o := dispatchSimpleViaScanner(t, d, db, uuid)
	if o.BinID != nil {
		t.Fatalf("claimed bin %d from elsewhere for a source that does not exist", *o.BinID)
	}
	if o.QueueCause != string(CauseFinderSourceMissing) {
		t.Errorf("status=%s queue_cause=%q, want %q", o.Status, o.QueueCause, CauseFinderSourceMissing)
	}
	if want := "Source GONE-NODE no longer exists — waiting for its configuration to be fixed"; o.QueueReason != want {
		t.Errorf("queue_reason = %q, want %q", o.QueueReason, want)
	}
}
