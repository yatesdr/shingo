//go:build docker

package dispatch

// End-to-end coverage that ApplyComplexPlan is the live claim path. A two-pickup
// order dispatched through DispatchPreparedComplex must claim both bins, record
// both order_bins junction rows with the correct per-bin destinations, set the
// primary bin, and reach a non-failed terminal status.

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

func TestComplexDispatch_ApplyEndState(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Two independent pickup->dropoff pairs so each bin's destination is
	// unambiguous: SMKT bin -> LINE, DEPOT bin -> STAGE.
	for _, name := range []string{"SMKT", "LINE", "DEPOT", "STAGE"} {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: name, Enabled: true}), "create node "+name)
	}
	smkt, _ := db.GetNodeByDotName("SMKT")
	depot, _ := db.GetNodeByDotName("DEPOT")
	smktBin := testdb.CreateBinAtNode(t, db, "PART-A", smkt.ID, "SMKTBIN")
	testdb.CreateBinAtNode(t, db, "PART-A", depot.ID, "DEPOTBIN")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SMKT"},
		{Action: protocol.ActionDropoff, Node: "LINE"},
		{Action: protocol.ActionPickup, Node: "DEPOT"},
		{Action: protocol.ActionDropoff, Node: "STAGE"},
	}
	stepsJSON, _ := json.Marshal(steps)
	order := &orders.Order{
		EdgeUUID:     "apply-endstate",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  "PART-A",
		SourceNode:   "SMKT",
		DeliveryNode: "STAGE",
		ProcessNode:  "SMKT",
		StepsJSON:    string(stepsJSON),
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	if err := d.DispatchPreparedComplex(order); err != nil {
		t.Fatalf("DispatchPreparedComplex: %v", err)
	}

	got, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read order")
	if got.Status == StatusFailed {
		t.Fatalf("order failed unexpectedly: status=%q", got.Status)
	}
	if got.BinID == nil || *got.BinID != smktBin.ID {
		t.Errorf("primary Order.BinID = %v, want SMKT bin %d (process node)", got.BinID, smktBin.ID)
	}

	claimed, err := db.ListBinsByClaim(order.ID)
	testutil.MustNoErr(t, err, "list claimed")
	if len(claimed) != 2 {
		t.Errorf("claimed %d bins, want 2", len(claimed))
	}

	obs, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list order_bins")
	if len(obs) != 2 {
		t.Fatalf("junction rows = %d, want 2", len(obs))
	}
	dest := map[string]string{}
	for _, ob := range obs {
		dest[ob.NodeName] = ob.DestNode
	}
	if dest["SMKT"] != "LINE" {
		t.Errorf("SMKT bin destination = %q, want LINE", dest["SMKT"])
	}
	if dest["DEPOT"] != "STAGE" {
		t.Errorf("DEPOT bin destination = %q, want STAGE", dest["DEPOT"])
	}
}
