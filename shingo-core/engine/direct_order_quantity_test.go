//go:build docker

package engine

import (
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// TestCreateDirectOrder_CountsOneBin pins that the engineer's bin-move records a
// count of one rather than nothing.
//
// The order literal on this door omitted the field entirely, so the row stored
// Go's zero value. The table declares DEFAULT 1, but the INSERT names the column
// explicitly, so the default never got a chance — and the order screen showed
// "qty 0" on every direct move ever made through this door.
func TestCreateDirectOrder_CountsOneBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-QTY-DIRECT")
	eng := newTestEngine(t, db, simulator.New())

	res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "test-station",
		Priority:     1,
		Desc:         "quantity fixture",
	})
	if err != nil {
		t.Fatalf("CreateDirectOrder: %v", err)
	}

	got, err := db.GetOrder(res.OrderID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.Quantity != 1 {
		t.Errorf("direct move stored quantity = %d, want 1 — the order screen prints this number, and 0 is not something a move can be", got.Quantity)
	}
}
