package messaging

import (
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
)

// TestHandleOrderError_AppliesTheRowsBeforeTheOrder: an order.error carrying
// Core's rows (a move refused at intake, "not at that source") writes them into
// the bins copy first, so the board redraws the source from Core's row before
// the refusal reaches the order, which the board then shows.
func TestHandleOrderError_AppliesTheRowsBeforeTheOrder(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	h := NewEdgeHandler(orders.NewManager(db, noopEmitter{}, "edge"))
	oid, err := db.CreateOrder("uuid-rows", protocol.OrderTypeMove, nil, false, 1, "HOLD-1", "", "SPOT-1", true, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	row := protocol.NodeBinsRow{Node: "SPOT-1", State: protocol.BinsStateEmpty}
	var applied []protocol.NodeBinsRow
	var statusAtApply string
	h.ApplyRows = func(rows []protocol.NodeBinsRow) {
		applied = rows
		if o, err := db.GetOrder(oid); err == nil {
			statusAtApply = string(o.Status)
		}
	}

	h.HandleOrderError(&protocol.Envelope{}, &protocol.OrderError{OrderUUID: "uuid-rows",
		ErrorCode: string(protocol.TermNoSourceBin), Detail: "bin B-1 is not at that source (SPOT-1)",
		Rows: []protocol.NodeBinsRow{row}})

	if len(applied) != 1 || applied[0].Node != row.Node || applied[0].State != row.State {
		t.Errorf("applied rows = %+v, want Core's row for SPOT-1", applied)
	}
	o, err := db.GetOrder(oid)
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if statusAtApply == "" || statusAtApply == string(o.Status) {
		t.Errorf("the order was already %q when the rows were applied; the rows go first", statusAtApply)
	}
}
