package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestStartChangeover_DiggingOrderIsCancelledHoweverTheEdgeLearnedOfIt pins the
// two ways the Edge can hold an order that Core is digging out (reshuffling),
// and asserts a changeover start treats them alike.
//
// The live push never mirrors reshuffling: ApplyCoreStatus has no arm for it, so
// the row keeps its pre-fleet status (queued here) and the start cancels it. The
// boot snapshot does write reshuffling. Before the classifier called reshuffling
// cancel, the same order then survived the start on a restarted Edge and could
// later deliver the outgoing style to the line.
func TestStartChangeover_DiggingOrderIsCancelledHoweverTheEdgeLearnedOfIt(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"live push", "boot snapshot"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
			eng := testEngine(t, db)

			uuid := "uuid-dig-" + path
			orderID, err := db.CreateOrder(uuid, orders.TypeRetrieve,
				&nodeID, false, 1, "CO-NODE", "", "", "", false, "PART-OLD", "", "")
			testutil.MustNoErr(t, err, "create order")
			testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusQueued)), "queue it")

			var wantMirror protocol.Status
			if path == "live push" {
				o, gerr := db.GetOrder(orderID)
				testutil.MustNoErr(t, gerr, "load order")
				testutil.MustNoErr(t, eng.orderMgr.ApplyCoreStatus(o, protocol.StatusReshuffling, "digging"), "live push")
				wantMirror = orders.StatusQueued
			} else {
				testutil.MustNoErr(t, eng.orderMgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
					OrderUUID: uuid, Found: true, Status: string(protocol.StatusReshuffling),
				}), "boot snapshot")
				wantMirror = protocol.StatusReshuffling
			}
			o, gerr := db.GetOrder(orderID)
			testutil.MustNoErr(t, gerr, "reload order")
			if o.Status != wantMirror {
				t.Fatalf("after the %s the row reads %s, want %s", path, o.Status, wantMirror)
			}

			if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
				t.Fatalf("changeover refused with a digging order at the node: %v", err)
			}
			o, gerr = db.GetOrder(orderID)
			testutil.MustNoErr(t, gerr, "reload order after start")
			if o.Status != orders.StatusCancelled {
				t.Errorf("after the %s the start left the order %s, want cancelled", path, o.Status)
			}
		})
	}
}
