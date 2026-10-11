//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// request_bin_move_pins_test.go — the bins page's "Request move" files the move
// it reports (D1 certification §4 item 1).
//
// The button answered "Transport requested: SRC → DEST" and created nothing: a
// person was told a robot was coming and none was. It now files a plain move of
// that bin to the chosen node through the manual-move door the Orders page uses
// for a named bin (CreateBinMove, by label), so it births through the one
// builder and refuses what that door refuses.

// TestRequestBinMove_FilesAMoveOrderThroughTheManualMoveDoor: a 200 means a move
// order row exists, from the bin's node to the chosen node, holding that bin,
// and the answer names it.
func TestRequestBinMove_FilesAMoveOrderThroughTheManualMoveDoor(t *testing.T) {
	t.Parallel()
	h, db := testHandlersWithSim(t, simulator.New())
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RQMOVE-OK")

	rec := postJSON(t, h.apiRequestBinMove, "/api/bins/request-move",
		map[string]any{"bin_id": bin.ID, "destination_node_id": sd.LineNode.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OrderID int64  `json:"order_id"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OrderID == 0 {
		t.Fatalf("the answer names no order: the page reported a move that was never filed")
	}
	o, err := db.GetOrder(resp.OrderID)
	if err != nil {
		t.Fatalf("the answer's order %d has no row: %v", resp.OrderID, err)
	}
	if o.OrderType != protocol.OrderTypeMove {
		t.Errorf("order type %q, want move", o.OrderType)
	}
	if o.SourceNode != sd.StorageNode.Name || o.DeliveryNode != sd.LineNode.Name {
		t.Errorf("order goes %s -> %s, want %s -> %s", o.SourceNode, o.DeliveryNode, sd.StorageNode.Name, sd.LineNode.Name)
	}
	if o.BinID == nil || *o.BinID != bin.ID {
		t.Errorf("order holds bin %v, want %d", o.BinID, bin.ID)
	}
	if o.OriginClass != string(protocol.OriginClassNoDemand) {
		t.Errorf("origin class %q, want no_demand: a person's move, as on the Orders page", o.OriginClass)
	}
}

// TestRequestBinMove_RefusesWhatTheManualMoveDoorRefuses: a lineside destination
// that already holds a bin is refused 409 with the door's sentence, and no order
// is written.
func TestRequestBinMove_RefusesWhatTheManualMoveDoorRefuses(t *testing.T) {
	t.Parallel()
	h, db := testHandlersWithSim(t, simulator.New())
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RQMOVE-SRC")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-RQMOVE-BLOCKING")

	rec := postJSON(t, h.apiRequestBinMove, "/api/bins/request-move",
		map[string]any{"bin_id": bin.ID, "destination_node_id": sd.LineNode.ID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409 (the manual-move door's answer for a full destination); body=%s",
			rec.Code, rec.Body.String())
	}
	all, err := db.ListOrders("", 100)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("%d order(s) written by a refused move, want 0", len(all))
	}
}
