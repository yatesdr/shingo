//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// c2b_complex_refusal_pins_docker_test.go — C2b A.2 and Q6: the complex door's
// failures go through the one door the plain path has (DemoteAfterFleetRefusal),
// and `fleet_failed` is written for nothing the fleet never saw.
//
// Source: RULINGS-C2b-P0-2026-10-06.md §3 Q6 (flat bug, ships with A.2); the
// fleet refusal itself is TestC2bPin_AComplexOrderTheFleetRefusesRequeues in
// complex_dispatch_char_test.go.

// c2bComplexOrder is a fully sourceable solo complex order: a pickup at storage
// and a drop at a line, nothing gated.
func c2bComplexOrder(t *testing.T, uuid string) (*Dispatcher, *mockEmitter, *orders.Order, func() *orders.Order) {
	t.Helper()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-"+uuid)
	order := &orders.Order{
		EdgeUUID:     uuid,
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  bp.Code,
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		ProcessNode:  storageNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + storageNode.Name + `"},` +
			`{"action":"dropoff","node":"` + lineNode.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	reload := func() *orders.Order {
		o, err := db.GetOrder(order.ID)
		return testutil.Must(t, o, err, "reload the complex order")
	}
	return d, emitter, order, reload
}

// A LOST CAS IS ANOTHER CALLER'S ORDER. Between this pass's read and its hand,
// a second dispatcher takes the order to `dispatched` (the pre-hand seam stands
// in for it). The hand's status CAS loses and returns ConcurrentTransition:
// nothing held, nothing sent. The plain path yields (Scanner.yieldLostHand).
//
// It was red at the base: the complex door did not ask, fell through to
// failOrderInternal("fleet_failed"), and emitted a failure for an order the
// fleet never saw and another caller now owned (refuseComplexHandover now).
func TestC2bPin_AComplexOrderThatLosesItsCASIsLeftToItsOwner(t *testing.T) {
	t.Parallel()
	d, emitter, order, reload := c2bComplexOrder(t, "c2b-lost-cas")
	d.SetPreHandHook(func(orderID int64) {
		if orderID == order.ID {
			// The winner's hand, as it commits: the status, the bin's claim and
			// its confirmed row. A bare status write would leave a `dispatched`
			// order unarmored, which the teardown's assertion rightly calls a bug.
			testdb.SeedOrderStatus(t, d.db, orderID, string(StatusDispatched), "another dispatcher won")
			_, err := d.db.DB.Exec(`UPDATE bins SET claimed_by = $1 WHERE label = $2`, orderID, "BIN-c2b-lost-cas")
			testutil.MustNoErr(t, err, "the winner claims the bin")
			_, err = d.db.DB.Exec(`UPDATE reservations SET state = 'confirmed' WHERE order_id = $1 AND resource_kind = 'bin'`, orderID)
			testutil.MustNoErr(t, err, "the winner confirms its row")
		}
	})

	_ = d.DispatchPreparedComplex(order)

	var violations []string
	for _, f := range emitter.failed {
		if f.orderID == order.ID {
			violations = append(violations, fmt.Sprintf("a failure (%s) was emitted for order %d, which "+
				"another caller had already dispatched", f.errorCode, f.orderID))
		}
	}
	if after := reload(); after.Status != StatusDispatched {
		violations = append(violations, fmt.Sprintf("the winner's row reads %s, not %s", after.Status, StatusDispatched))
	}
	if len(violations) != 0 {
		t.Fatalf("the loser touched the winner's order: %v", violations)
	}
}
