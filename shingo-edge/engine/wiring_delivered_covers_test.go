package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestDeliveredNotBound_TheCheckCoversEveryOrigin: with the older Core's bind
// gone, checkDeliveredCarrier is what makes a delivery the seat doesn't hold
// loud, whoever the event came from. Where the seat holds the carrier, a
// noticed delivery is quiet and one with no order.delivered behind it is
// still said. Never does either move the seat. A delivered with nothing to
// check against is said only for that same origin ("matched no bind path").
func TestDeliveredNotBound_TheCheckCoversEveryOrigin(t *testing.T) {
	t.Parallel()
	const bin, other = int64(9301), int64(9302)
	for _, c := range []struct {
		name       string
		seated     int64 // 0: empty
		byName     bool  // no process node on the order: the fallback
		notice, no int   // alarms with an order.delivered behind it, and without
	}{
		{"empty seat", 0, false, 1, 1},
		{"another carrier", other, false, 1, 1},
		{"holds it", bin, false, 0, 1},
		{"empty seat, by name", 0, true, 1, 1},
		{"another carrier, by name", other, true, 1, 1},
		{"holds it, by name", bin, true, 0, 1},
		{"nothing to check against", 0, true, 0, 1},
	} {
		for _, fromNotice := range []bool{true, false} {
			db := testEngineDB(t)
			eng := testEngine(t, db)
			eng.wireEventHandlers()
			alarms := captureDeliveredNotBound(eng)
			_, nodeID, _, _ := emptySeat(t, eng, "COV", "PART-COV", 100)
			if c.seated != 0 {
				eng.HandleBinLanded(landing("COV-NODE", c.seated, 1, 70, "PART-COV", 7))
			}
			id, err := db.CreateOrder("uuid-cov", orders.TypeRetrieve, &nodeID, false, 1, "COV-NODE", "", "", protocol.Height{}, false, "", "", "")
			testutil.MustNoErr(t, err, "create order")
			bid := bin
			ev := OrderDeliveredEvent{OrderID: id, OrderUUID: "uuid-cov", OrderType: orders.TypeRetrieve,
				BinID: &bid, DeliveryNode: "COV-NODE", FromNotice: fromNotice}
			if !c.byName {
				ev.ProcessNodeID = &nodeID
			}
			if c.name == "nothing to check against" {
				ev.DeliveryNode = ""
			}
			eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: ev})

			want := c.no
			if fromNotice {
				want = c.notice
			}
			if len(*alarms) != want {
				t.Errorf("%s, notice %v: %d alarms, want %d: %+v", c.name, fromNotice, len(*alarms), want, *alarms)
			}
			gotBin, gotCount := int64(0), runtimeOf(t, eng, nodeID).RemainingUOPCached
			if rt := runtimeOf(t, eng, nodeID); rt.ActiveBinID != nil {
				gotBin = *rt.ActiveBinID
			}
			if gotBin != c.seated || (c.seated != 0 && gotCount != 70) {
				t.Errorf("%s, notice %v: seat = bin %d count %d, want %d untouched", c.name, fromNotice, gotBin, gotCount, c.seated)
			}
		}
	}
}

// TestDeliveredNotice_IsItsOwnFactWhateverTheKeySays: an order.delivered with
// no landing_sent is still a notice, and it binds nothing, whatever old
// carrier keys it has.
func TestDeliveredNotice_IsItsOwnFactWhateverTheKeySays(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngineWithOrderBridge(t, db)
	_, nodeID, _, _ := emptySeat(t, eng, "NTC", "PART-NTC", 100)
	var seen []OrderDeliveredEvent
	eng.Events.Subscribe(func(evt Event) {
		if p, ok := evt.Payload.(OrderDeliveredEvent); ok {
			seen = append(seen, p)
		}
	})
	id, err := db.CreateOrder("uuid-ntc", orders.TypeRetrieve, &nodeID, false, 1, "NTC-NODE", "", "", protocol.Height{}, false, "", "", "")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusInTransit)), "put the order in transit")

	var d protocol.OrderDelivered
	testutil.MustNoErr(t, json.Unmarshal([]byte(`{"order_uuid":"uuid-ntc","delivered_at":"2026-01-02T03:04:05Z","bin_id":9303,`+
		`"delivery_node":"NTC-NODE","uop_remaining":40,"delta_epoch":2}`), &d), "decode the delivered")
	testutil.MustNoErr(t, eng.orderMgr.HandleDeliveredNotice(&d), "handle the delivered")

	if len(seen) != 1 || !seen[0].FromNotice {
		t.Errorf("delivered events = %+v, want one from the notice", seen)
	}
	if rt := runtimeOf(t, eng, nodeID); rt.ActiveBinID != nil || rt.RemainingUOPCached != 0 {
		t.Errorf("seat = bin %v count %d, want empty: the delivered bound", rt.ActiveBinID, rt.RemainingUOPCached)
	}
}
