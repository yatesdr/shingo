//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TWO REFILLS FOR ONE KEEP-STAGED SPOT, back to back.
//
// A keep-staged cell whose spot is bare when a swap is requested sends two plain
// retrieves to the spot: one the swap lifts, one to stand after it (Edge's
// reconcileSpot). The spot is a parentless staging node, so nothing reserves it
// for a plain order; what keeps the second off the first is the dropoff gate,
// which counts an order delivering there that holds a claimed bin. The first is
// dispatched; the second waits waiting_for_slot until the spot is clear again.
func TestTwoRefillsForOneSpot_TheSecondWaitsForTheFirst(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	spot := &nodes.Node{Name: "KS-SPOT-CORE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spot), "spot")
	createTestBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "KS-REFILL-1")
	other := &nodes.Node{Name: "KS-MARKET-2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(other), "second source")
	createTestBinAtNode(t, db, sd.Payload.Code, other.ID, "KS-REFILL-2")

	eng := newTestEngine(t, db, simulator.New())
	for _, uuid := range []string{"ks-refill-a", "ks-refill-b"} {
		eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID: uuid, OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
			DeliveryNode: spot.Name, Quantity: 1,
		})
	}

	a := testdb.RequireOrder(t, db, "ks-refill-a")
	b := testdb.RequireOrder(t, db, "ks-refill-b")
	if a.VendorOrderID == "" || a.BinID == nil {
		t.Fatalf("the first refill did not go: status %q (%s)", a.Status, a.QueueCause)
	}
	if b.VendorOrderID != "" || !protocol.IsAcquiring(b.Status) {
		t.Fatalf("the second refill went while the first is bound for the spot: status %q vendor %q", b.Status, b.VendorOrderID)
	}
	if b.QueueCode != string(protocol.QueueWaitingForSlot) {
		t.Errorf("the second waits under %q (%s), want %q", b.QueueCode, b.QueueCause, protocol.QueueWaitingForSlot)
	}
	n, err := orders.CountInFlightByDeliveryNode(db.DB, spot.Name)
	testutil.MustNoErr(t, err, "gate count")
	if n != 1 {
		t.Errorf("%d orders hold a bin bound for the spot, want 1", n)
	}
}
