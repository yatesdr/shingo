//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestHoldsAtRest_ComplexLaneWaitParksHoldingItsBins — red at the base, green
// with the one-step acquire.
//
// At the base, acquireComplexPhases reserved AND confirmed the order's source
// bins before it asked the lane, and the note above admitComplexLanes said the
// order kept what it had while the lane refused, on purpose. So a complex order
// refused at the lane waited holding bins.claimed_by on every bin it would pick
// up. Now the solo order confirms nothing until its hand (prepareComplexHand):
// the lane verdict refuses it holding its bins SOFT, which is what a wait keeps.
//
// The fixture: the order picks up at the mouth of a lane another order is inside
// (an occupancy row, as the create seam leaves for a robot in the lane), and
// drops at a line node, so the dropoff gate has nothing to refuse and the lane
// admit is the phase that says no.
//
// Flat bug, SYNTH §0 row 2, under the v4 §2.6 ruling that a wait keeps SOFT
// reservations only.
func TestHoldsAtRest_ComplexLaneWaitParksHoldingItsBins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	laneID, mouth := seamLane(t, db, "HAR-CPLX")
	line := lineNode(t, db, "HAR-CPLX-LINE")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	bin := testdb.CreateBinAtNode(t, db, bp.Code, mouth.ID, "HAR-CPLX-BIN")

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
	if after.VendorOrderID != "" || after.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("fixture: the complex order is %q under %q (vendor %q), want it held at the lane "+
			"under %s", after.Status, after.QueueCause, after.VendorOrderID, CauseLaneOccupied)
	}
	testdb.AssertNoHoldsAtRest(t, db, "a complex order refused at the lane")
	var state string
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT state FROM reservations WHERE order_id = $1 AND resource_kind = 'bin' AND bin_id = $2`,
		cplx.ID, bin.ID).Scan(&state), "the waiting order's reservation on the mouth bin")
	if state != "pending" {
		t.Errorf("the waiting order's reservation on bin %d is %q, want pending: it waits holding the bin SOFT", bin.ID, state)
	}
}

// TestDropoffGate_ALaneWaiterNoLongerCountsAtItsDestination pins the ruling the
// one-step acquire carries (v4 Rule 1 / §2.6): the dropoff gate admits more at a
// node once a lane-waiter holds its bin soft instead of hard.
//
// The gate's in-flight count reads HOLDERS (orders.InFlightForDropoffSQL: an
// order counts when it hard-claims a bin bound for the node). At the base, a
// complex order refused at the lane had already hard-claimed its source bin, so
// it counted at its destination while it waited, and a second order to that
// node was refused dropoff-inflight behind an order that was not moving. Now it
// holds the bin soft and counts for nothing until its hand commits.
//
// Same fixture as the lane-wait pin above: the order picks up at the mouth of an
// occupied lane and drops at a line node.
func TestDropoffGate_ALaneWaiterNoLongerCountsAtItsDestination(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	_, mouth := seamLane(t, db, "HAR-GATE")
	line := lineNode(t, db, "HAR-GATE-LINE")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	testdb.CreateBinAtNode(t, db, bp.Code, mouth.ID, "HAR-GATE-BIN")
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "har-gate-inside"
		o.OrderType = OrderTypeMove
		o.Status = StatusDispatched
		o.VendorOrderID = "vendor-har-gate-inside"
	})
	mustTake(t, d, inside.ID, mouth)

	waiter := &orders.Order{
		EdgeUUID: "har-gate-waiter", StationID: "line-1", OrderType: OrderTypeComplex,
		Status: StatusPending, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: mouth.Name, DeliveryNode: line.Name, ProcessNode: line.Name,
		StepsJSON: `[{"action":"pickup","node":"` + mouth.Name + `"},` +
			`{"action":"dropoff","node":"` + line.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(waiter), "create the waiter")
	waiterRaw, err := db.GetOrder(waiter.ID)
	waiter = testutil.Must(t, waiterRaw, err, "db.GetOrder(waiter.ID)")
	_ = d.DispatchPreparedComplex(waiter)
	afterRaw, err := db.GetOrder(waiter.ID)
	after := testutil.Must(t, afterRaw, err, "db.GetOrder(waiter.ID)")
	if after.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("fixture: the waiter is not held at the lane (%+v)", after)
	}

	n, err := db.CountInFlightOrdersByDeliveryNodeExcluding(line.Name, 0)
	testutil.MustNoErr(t, err, "count in-flight to the line")
	if n != 0 {
		t.Fatalf("in-flight to %s = %d while the only order bound there waits at a lane, want 0 "+
			"(the ruling: a waiter holds its bin soft and counts for nothing at the gate)", line.Name, n)
	}
	if blocked, block := CheckDropoffCapacity(d.db, line.Name, 0); blocked {
		t.Errorf("a second order to %s is refused (%s) behind an order that is waiting at a lane, "+
			"not moving", line.Name, block.Cause)
	}
}
