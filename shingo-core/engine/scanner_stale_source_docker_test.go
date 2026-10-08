//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestScanner_AGatedRetrieveIsClassifiedOnTheSourceTheScannerFilled: the
// scanner finds the bin and writes the order's source_node and bin_id to the
// row, and the gated create classifies the order it was handed. That struct
// must carry the source the scanner filled, or the classifier resolves a stale
// source, errors, and leaves the order staged with no cause: only the floor
// frees it (demo 103, 267, 409 on 297290ba; latent at the base, 19 times in the
// before run). A bin in a free lane goes straight in.
func TestScanner_AGatedRetrieveIsClassifiedOnTheSourceTheScannerFilled(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	_, slots := laneWithTwoSlots(t, db, "STALESRC")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, slots[0].ID, "STALESRC-BIN")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "stalesrc-1"
		o.OrderType = dispatch.OrderTypeRetrieve
		o.Status = protocol.StatusQueued
		o.PayloadCode = sd.Payload.Code
		o.SourceNode = ""
		o.DeliveryNode = sd.LineNode.Name
	})
	eng := newTestEngine(t, db, simulator.New())
	eng.RunFulfillmentScan()

	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.SourceNode != slots[0].Name {
		t.Fatalf("fixture: the scanner sourced %q, want %s — the rest is about that source", got.SourceNode,
			slots[0].Name)
	}
	if dispatch.IsGateStaged(got) {
		t.Fatalf("the retrieve is left at the mark (status %s, cause %q) with its bin in a free lane: the "+
			"classifier was handed the order as it was before the scanner filled its source, could not "+
			"resolve it, and left it for the floor", got.Status, got.QueueCause)
	}
}
