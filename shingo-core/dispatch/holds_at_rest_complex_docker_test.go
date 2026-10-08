//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestHoldsAtRest_ComplexLaneWaitIsHeldAtTheMarkWithItsCause — a complex order
// whose pickup is in a lane another order is inside is sent to the lane's mark
// and held there, and its row says why.
//
// It used to pin the PRE-dispatch shape: refused at the lane, holding its bins
// soft. Every lane is marked now, so the wait is the gate's, after dispatch: the
// robot is committed to the mark and the order's bins are its hand's. What stays
// is the half an operator reads: the valve's refusal is on the row (F2). It was
// not, for complex orders: the create cleared the cause after the valve wrote it.
//
// The fixture: the order picks up at the mouth of a lane another order is inside
// (an occupancy row, as the create seam leaves for a robot in the lane), and
// drops at a line node.
func TestHoldsAtRest_ComplexLaneWaitIsHeldAtTheMarkWithItsCause(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	laneID, mouth := seamLane(t, db, "HAR-CPLX")
	line := lineNode(t, db, "HAR-CPLX-LINE")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	testdb.CreateBinAtNode(t, db, bp.Code, mouth.ID, "HAR-CPLX-BIN")

	// Another order is inside the lane.
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "har-cplx-inside"
		o.OrderType = OrderTypeMove
		o.Status = StatusDispatched
		o.VendorOrderID = "vendor-har-cplx-inside"
	})
	mustTake(t, d, inside.ID, mouth)
	if got := occupants(t, db, laneID); len(got) != 1 || got[0] != inside.ID {
		t.Fatalf("fixture: occupants of the lane = %v, want [%d]", got, inside.ID)
	}

	cplx := &orders.Order{
		EdgeUUID: "har-cplx", StationID: "line-1", OrderType: OrderTypeComplex,
		Status: StatusPending, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: mouth.Name, DeliveryNode: line.Name, ProcessNode: line.Name,
		StepsJSON: `[{"action":"pickup","node":"` + mouth.Name + `"},` +
			`{"action":"dropoff","node":"` + line.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(cplx), "create the complex order")
	cplxRaw, err := db.GetOrder(cplx.ID)
	cplx = testutil.Must(t, cplxRaw, err, "db.GetOrder(cplx.ID)")
	_ = d.DispatchPreparedComplex(cplx)

	after, err := db.GetOrder(cplx.ID)
	testutil.MustNoErr(t, err, "db.GetOrder(cplx.ID)")
	// Every lane is marked: the order is sent to the mark and held there, a
	// dispatched order whose robot is committed, so its bins are its hand's.
	if !IsGateStaged(after) || after.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("the complex order is %q under %q (vendor %q, gate-staged %v), want it held at the "+
			"lane's mark under %s", after.Status, after.QueueCause, after.VendorOrderID, IsGateStaged(after),
			CauseLaneOccupied)
	}
	testdb.AssertNoHoldsAtRest(t, db, "a complex order held at the lane's mark")
}
