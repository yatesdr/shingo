//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// F1b — multi-tote adoption repro.
//
// A two-robot consume swap delivers with TWO order_bins: a SUPPLY bin dropped
// AT the consuming (process) node, and an EVAC bin picked up FROM it and driven
// out to the supermarket. order.BinID names the bin CLAIMED at the process node
// — for a swap that is the EVAC bin (picked up there), NOT the supply bin that
// stays and is consumed. The supply bin is identified only by its per-bin
// dest_node in order_bins (== order.ProcessNode).
//
// Pre-F1b, handleOrderDelivered ships BinID=nil for any multi-bin order, so the
// supply bin's id never rides the delivered envelope and Edge can never bind it:
// consume ticks pile in pending_uop_delta and the tile drains while Core holds
// the delivered value. This is the SNF3/CARRIER-0024 stranding.
//
// findDeliveredEnvelope drains the outbox and returns the single OrderDelivered
// message Core enqueued for the order under test.
func findDeliveredEnvelope(t *testing.T, db *store.DB) *protocol.OrderDelivered {
	t.Helper()
	rows, err := db.ListPendingOutbox(50)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	var found *protocol.OrderDelivered
	for _, row := range rows {
		if row.MsgType != protocol.TypeOrderDelivered {
			continue
		}
		env := &protocol.Envelope{}
		if err := json.Unmarshal(row.Payload, env); err != nil {
			t.Fatalf("decode outbox envelope: %v", err)
		}
		var d protocol.OrderDelivered
		if err := env.DecodePayload(&d); err != nil {
			t.Fatalf("decode OrderDelivered payload: %v", err)
		}
		if found != nil {
			t.Fatalf("more than one OrderDelivered enqueued; test expects exactly one")
		}
		cp := d
		found = &cp
	}
	if found == nil {
		t.Fatalf("no OrderDelivered message enqueued to the outbox")
	}
	return found
}

// dispatchMultiToteSwap builds the consume-swap fixture used by the F1b tests:
// a supply bin at storage (final dest = lineNode) and an evac bin at lineNode
// (final dest = outboundDest). ProcessNode is the lineNode, so order.BinID
// tracks the evac bin and the supply bin is reachable only via order_bins.
// Returns the delivered order plus both bin ids.
func dispatchMultiToteSwap(t *testing.T, db *store.DB, eng *Engine, sim *simulator.SimulatorBackend) (supplyBinID, evacBinID int64, lineNodeName string) {
	t.Helper()
	storageNode, lineNode, inboundStaging, _, outboundDest, bp := setupProductionNodes(t, db)

	supplyBin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-F1B-SUPPLY")
	evacBin := createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-F1B-EVAC")

	d := eng.Dispatcher()
	env := testEnvelope()

	// Evac leaves the line early (to inbound staging), supply lands at the line,
	// then evac is driven out to the supermarket last. Result:
	//   supply → lineNode        (the consumed bin — dest == ProcessNode)
	//   evac   → outboundDest     (final dropoff — NOT the process node)
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "f1b-multitote-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		ProcessNode: lineNode.Name, // the consuming node
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: lineNode.Name},        // grab evac off the line
			{Action: "dropoff", Node: inboundStaging.Name}, // park it
			{Action: "pickup", Node: storageNode.Name},     // grab supply
			{Action: "dropoff", Node: lineNode.Name},       // supply lands at the line
			{Action: "wait"},
			{Action: "pickup", Node: inboundStaging.Name}, // re-grab evac
			{Action: "dropoff", Node: outboundDest.Name},  // evac out to supermarket
		},
	})

	order := testdb.RequireOrder(t, db, "f1b-multitote-1")

	// Sanity: this is genuinely the multi-tote shape.
	orderBins, err := db.ListOrderBins(order.ID)
	if err != nil {
		t.Fatalf("list order bins: %v", err)
	}
	if len(orderBins) < 2 {
		t.Fatalf("expected >= 2 order_bins (multi-tote), got %d", len(orderBins))
	}

	// The supply bin is reachable only via its per-bin dest_node == ProcessNode,
	// and is NOT order.BinID (that names the evac bin claimed at the line).
	var supplyDestFound bool
	for _, ob := range orderBins {
		if ob.BinID == supplyBin.ID && ob.DestNode == lineNode.Name {
			supplyDestFound = true
		}
	}
	if !supplyDestFound {
		t.Fatalf("supply bin %d has no order_bins row destined for the process node %q", supplyBin.ID, lineNode.Name)
	}
	if order.BinID == nil {
		t.Fatalf("expected order.BinID set (evac bin claimed at process node)")
	}
	if *order.BinID == supplyBin.ID {
		t.Fatalf("order.BinID must NOT be the supply bin — the F1b gap requires the consumed bin to be the non-default one")
	}

	// Drive to delivered — handleOrderDelivered fires and enqueues OrderDelivered.
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")
	testdb.RequireOrderStatus(t, db, "f1b-multitote-1", "delivered")

	return supplyBin.ID, evacBin.ID, lineNode.Name
}

// TestF1b_MultiToteDelivered_ShipsConsumingBin proves F1b-2 (Core side): a
// multi-tote delivery selects the order_bin destined for the consuming process
// node and ships THAT bin's id + its dest node on the existing delivered
// envelope — not order.BinID (the evac bin), not nil. Edge binds it in F1b-3.
func TestF1b_MultiToteDelivered_ShipsConsumingBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	supplyBinID, evacBinID, lineNodeName := dispatchMultiToteSwap(t, db, eng, sim)

	delivered := findDeliveredEnvelope(t, db)

	if delivered.BinID == nil {
		t.Fatalf("F1b expects the consuming bin shipped, got BinID nil (multi-tote stranding not fixed)")
	}
	if *delivered.BinID != supplyBinID {
		t.Errorf("delivered.BinID = %d, want supply bin %d (the bin destined for the process node)", *delivered.BinID, supplyBinID)
	}
	if *delivered.BinID == evacBinID {
		t.Errorf("delivered.BinID = %d is the evac bin — must be the supply bin destined for the process node", evacBinID)
	}
	if delivered.BinDestNode != lineNodeName {
		t.Errorf("delivered.BinDestNode = %q, want the consuming node %q", delivered.BinDestNode, lineNodeName)
	}
	// The count snapshot must be for the SELECTED (supply) bin, present so Edge
	// seeds the runtime cache from it rather than a role default.
	if delivered.UOPRemaining == nil {
		t.Errorf("delivered.UOPRemaining = nil, want the supply bin's count snapshot")
	}
}
