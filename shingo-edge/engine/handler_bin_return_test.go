package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
)

// TestHandleBinReturn_NoticeOnly: the notice is stored against the cancelled
// order's uuid and the order itself does not move — the cancelled order stays
// cancelled. A late returning after returned does not displace it.
func TestHandleBinReturn_NoticeOnly(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	orderID, err := db.CreateOrder("uuid-br-1", orders.TypeRetrieve,
		nil, false, 1, "ALN_001", "", "", "", false, "PART-A", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(protocol.StatusCancelled)); err != nil {
		t.Fatalf("cancel order: %v", err)
	}

	eng.HandleBinReturn(protocol.BinReturn{OrderUUID: "uuid-br-1", BinLabel: "BIN-9",
		State: protocol.BinReturnReturned, Destination: "SMN_003"})
	eng.HandleBinReturn(protocol.BinReturn{OrderUUID: "uuid-br-1", BinLabel: "BIN-9",
		State: protocol.BinReturnReturning, Destination: "SMN_004"})
	// A uuid this station never saw is still stored: the row is display-only.
	eng.HandleBinReturn(protocol.BinReturn{OrderUUID: "uuid-elsewhere", BinLabel: "BIN-1",
		State: protocol.BinReturnHeld, Reason: "no slot"})

	got, err := db.BinReturnsForOrders([]string{"uuid-br-1", "uuid-elsewhere"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if r := got["uuid-br-1"]; r.State != protocol.BinReturnReturned || r.Destination != "SMN_003" {
		t.Errorf("stored = %+v, want returned to SMN_003", r)
	}
	if r := got["uuid-elsewhere"]; r.State != protocol.BinReturnHeld {
		t.Errorf("unknown-order notice not stored: %+v", r)
	}

	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if o.Status != protocol.StatusCancelled {
		t.Errorf("order status moved to %s; the notice must not touch it", o.Status)
	}
}
