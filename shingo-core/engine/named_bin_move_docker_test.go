//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// A MOVE THAT NAMES ITS BIN LIFTS THAT BIN OR NOTHING.
//
// The keep-staged sequence from the sequential sim: a station decides the
// spare on its spot is wrong and sends it back by a move; the changeover's own
// leg lifts that spare first; the refill lands on the spot; the move, still
// waiting, must not carry the fresh refill away. It names the bin it was for,
// and once that bin has left the spot it ends skipped.
func TestNamedBinMove_LiftsNothingOnceItsBinHasLeft(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	spot := &nodes.Node{Name: "NB-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NB-BACK", Enabled: true}
	market := &nodes.Node{Name: "NB-MARKET", Enabled: true}
	for _, n := range []*nodes.Node{spot, back, market} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	spare := createTestBinAtNode(t, db, "", spot.ID, "NB-SPARE")
	refill := createTestBinAtNode(t, db, "", market.ID, "NB-REFILL")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// The changeover's leg lifts the spare off the spot.
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID: "nb-leg", Quantity: 1,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: spot.Name},
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
		},
	})
	leg := testdb.RequireOrder(t, db, "nb-leg")
	if leg.BinID == nil || *leg.BinID != spare.ID {
		t.Fatalf("fixture: the leg claimed %v, want the spare %d", leg.BinID, spare.ID)
	}
	sim.DriveState(leg.VendorOrderID, "RUNNING")

	// The return for that spare arrives while the leg holds it.
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID: "nb-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
		SourceNode: spot.Name, DeliveryNode: back.Name, BinID: spare.ID,
	})
	if o := testdb.RequireOrder(t, db, "nb-return"); o.BinID != nil {
		t.Fatalf("fixture: the return holds bin %d while the leg carries the spare", *o.BinID)
	}

	// The leg delivers the spare to the line, and the refill lands on the spot.
	sim.DriveState(leg.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "nb-leg", ReceiptType: "confirmed", FinalCount: 1})
	testdb.RequireBinAtNode(t, db, spare.ID, lineNode.ID)
	_, err := db.Exec(`UPDATE bins SET node_id=$1 WHERE id=$2`, spot.ID, refill.ID)
	testutil.MustNoErr(t, err, "the refill lands on the spot")

	eng.RunFulfillmentScan()

	ret := testdb.RequireOrder(t, db, "nb-return")
	if ret.Status != protocol.StatusSkipped {
		t.Errorf("the return is %q, want skipped: the bin it was for has left the spot", ret.Status)
	}
	if ret.BinID != nil {
		t.Errorf("the return holds bin %d, want none", *ret.BinID)
	}
	got, err := db.GetBin(refill.ID)
	testutil.MustNoErr(t, err, "refill")
	if got.ClaimedBy != nil || got.NodeID == nil || *got.NodeID != spot.ID {
		t.Errorf("the refill is claimed by %v at node %v, want unclaimed on the spot", got.ClaimedBy, got.NodeID)
	}
	var held int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations WHERE order_id=$1`, ret.ID).Scan(&held), "holds")
	if held != 0 {
		t.Errorf("the return holds %d reservation(s), want none", held)
	}
}

// A move that names its bin, with the bin standing on its source, lifts it.
func TestNamedBinMove_LiftsItsBinWhenItStands(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	spot := &nodes.Node{Name: "NB2-SPOT", Enabled: true}
	back := &nodes.Node{Name: "NB2-BACK", Enabled: true}
	for _, n := range []*nodes.Node{spot, back} {
		testutil.MustNoErr(t, db.CreateNode(n), "node "+n.Name)
	}
	spare := createTestBinAtNode(t, db, "", spot.ID, "NB2-SPARE")
	eng := newTestEngine(t, db, simulator.New())

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "nb2-return", OrderType: protocol.OrderTypeMove, Quantity: 1,
		SourceNode: spot.Name, DeliveryNode: back.Name, BinID: spare.ID,
	})

	ret := testdb.RequireOrder(t, db, "nb2-return")
	if ret.BinID == nil || *ret.BinID != spare.ID || ret.VendorOrderID == "" {
		t.Fatalf("the return = status %q bin %v vendor %q, want dispatched with the spare %d",
			ret.Status, ret.BinID, ret.VendorOrderID, spare.ID)
	}
}
