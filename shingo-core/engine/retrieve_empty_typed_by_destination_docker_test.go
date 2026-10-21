//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestPlainRetrieveEmpty_TypedByItsDestinationsOneAdmittedType: a plain
// retrieve of an empty, as a station sends it (no process node on the wire),
// to a node whose effective bin types are exactly one, is sourced as that type.
//
// The type decider for an empty (plan.Finder.typeFromDestinationPosition) asks
// what fits where the carrier is set down next, need.SetDownNode. It used to
// read need.ProcessNode, which a plain retrieve never carries (OrderRequest has
// no such field), so the destination's one type was never read and the
// retrieve took whatever empty ranked first. Here that is the other type: its
// carrier has the lower id, and nothing else separates the two.
func TestPlainRetrieveEmpty_TypedByItsDestinationsOneAdmittedType(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), name)
		return n
	}
	mkType := func(code string) *bins.BinType {
		bt := &bins.BinType{Code: code}
		testutil.MustNoErr(t, db.CreateBinType(bt), "bin type "+code)
		return bt
	}
	dest, otherAt, rightAt := mk("RTE-DEST"), mk("RTE-SPOT-1"), mk("RTE-SPOT-2")
	kd, tote := mkType("RTE-KD-1"), mkType("RTE-TOTE-1")
	testutil.MustNoErr(t, db.SetNodeBinTypes(dest.ID, []int64{tote.ID}), "the destination admits one type")

	// The wrong type first, so it has the lower id and wins an untyped pick.
	wrong := &bins.Bin{BinTypeID: kd.ID, Label: "RTE-KD-CART", NodeID: &otherAt.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(wrong), "the other type's empty")
	right := &bins.Bin{BinTypeID: tote.ID, Label: "RTE-TOTE-CART", NodeID: &rightAt.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(right), "the admitted type's empty")

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "rte-retrieve", OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true,
		DeliveryNode: dest.Name, Quantity: 1,
	})
	o := testdb.RequireOrder(t, db, "rte-retrieve")
	if o.BinID == nil {
		t.Fatalf("the retrieve sourced nothing (status %s, cause %q)", o.Status, o.QueueCause)
	}
	if *o.BinID != right.ID {
		t.Errorf("the retrieve took bin %d, want %d (%s): %s admits only %s, and a plain "+
			"retrieve's type is read from where the carrier is set down",
			*o.BinID, right.ID, tote.Code, dest.Name, tote.Code)
	}
}
