//go:build docker

package www

import (
	"net/http"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// THE HTTP MANUAL-ORDER DOOR IS AN INTAKE OFF THE SCANNER'S GOROUTINE.
//
// apiManualOrderSubmit calls Dispatcher.HandleOrderRequest on the request's own
// goroutine, the same call the Kafka read loop makes for an Edge. So it is held
// to the same rule as engine's TestTwoHoldersThroughIntakeBuriedArm: while a
// scan has found order A's bin for node N and not yet claimed it, a manual
// retrieve to N whose source is buried must not end with a second order holding
// a bin bound for N. Intake names the burial and queues; the dig is planned by a
// scan, behind the dropoff gate, under scanMu.
func TestManualOrderDoor_TwoHoldersOnOneNode(t *testing.T) {
	t.Parallel()
	h, db := testHandlersWithSim(t, simulator.New())
	d := h.engine.Dispatcher()

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "MOTH", NumSlots: 2, TargetSlot: 2, TargetAge: 2 * time.Hour,
	})
	line := sc.LineNode
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "MOTH-A-SRC")

	bDone := make(chan int, 1)
	hookFired := false
	d.SetPostFindHook(func() {
		d.SetPostFindHook(nil)
		hookFired = true
		go func() {
			rec := postJSON(t, h.apiManualOrderSubmit, "/api/orders/spot", map[string]any{
				"order_type": "retrieve", "source_node": sc.Grp.Name, "delivery_node": line.Name,
				"payload_code": sc.Payload.Code,
			})
			bDone <- rec.Code
		}()
		select {
		case code := <-bDone:
			bDone <- code
		case <-time.After(2 * time.Second):
		}
	})

	d.HandleOrderRequest(testdb.Envelope(), &protocol.OrderRequest{
		OrderUUID: "moth-a", OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	if !hookFired {
		t.Fatal("precondition: the post-find hook never fired — A was not found by a scan")
	}
	select {
	case code := <-bDone:
		if code != http.StatusOK {
			t.Fatalf("the manual order was refused: HTTP %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the manual order never returned")
	}

	n, err := orders.CountInFlightByDeliveryNode(db.DB, line.Name)
	testutil.MustNoErr(t, err, "gate count")
	if n != 1 {
		t.Fatalf("%d orders hold a claimed bin bound for %s, want exactly 1", n, line.Name)
	}
	a := testdb.RequireOrder(t, db, "moth-a")
	if a.BinID == nil || protocol.IsAcquiring(a.Status) {
		t.Errorf("A is not the holder (status %q)", a.Status)
	}
}
