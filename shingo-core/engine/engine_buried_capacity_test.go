//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// TestBuriedBin_MustNotBypassDropoffCapacity pins the gate a buried retrieve
// shares with a plain one: a retrieve to an OCCUPIED line must be queued, never
// dispatched — burial does not earn an exemption.
//
// The subtlety is that a simple-retrieve reshuffle compound IS the delivery.
// PlanReshuffle's step 2 is "Retrieve the target (this is the actual order
// delivery)" and leaves ToNode nil so compound.go defaults it to the parent's
// DeliveryNode. Compound children are dispatched by AdvanceCompoundOrder, which
// never consults CheckDropoffCapacity (the scanner skips them — it only gates
// orders with no ParentOrderID). So the moment a reshuffle is planned, its
// delivery leg is committed to the destination with no capacity check anywhere
// in the path.
//
// That makes the ORDER of the two gates in planTransport load-bearing: the
// dropoff gate must run BEFORE source resolution, or a buried source plans a
// compound that delivers into a line another order is (correctly) queued behind
// — the deadlock_gate_test invariant, laundered through a compound.
func TestBuriedBin_MustNotBypassDropoffCapacity(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Target buried at depth 2 behind a blocker at the mouth.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:     "BCAP",
		NumSlots:   2,
		TargetSlot: 2,
		TargetAge:  2 * time.Hour,
	})

	// The delivery line is OCCUPIED — a plain retrieve here is gated (see
	// dispatch/deadlock_gate_test.go). A buried one must be gated identically.
	lineBin := &bins.Bin{BinTypeID: sc.BinType.ID, Label: "BCAP-LINE-OCC", NodeID: &sc.LineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "occupy the line")

	eng := newTestEngine(t, db, simulator.New())
	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "buried-occupied-line",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "buried-occupied-line")
	children, err := db.ListChildOrders(order.ID)
	testutil.MustNoErr(t, err, "list child orders")

	if order.Status == dispatch.StatusReshuffling || len(children) > 0 {
		t.Fatalf("buried retrieve BYPASSED the dropoff gate: status = %q with %d compound children, "+
			"but the delivery line still holds bin %d.\n"+
			"The compound's retrieve leg delivers to the parent's DeliveryNode and is dispatched by "+
			"AdvanceCompoundOrder, which never checks CheckDropoffCapacity — so this order is now "+
			"committed to driving a bin into an occupied line.",
			order.Status, len(children), lineBin.ID)
	}
	if order.Status != dispatch.StatusQueued {
		t.Fatalf("status = %q, want %q (gated on the occupied line)", order.Status, dispatch.StatusQueued)
	}
}
