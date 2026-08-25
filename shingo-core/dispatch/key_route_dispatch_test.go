//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// THE OTHER END OF THE CONDUIT. The adapter has always threaded
// CreateOrderRequest.KeyRoute into rds.SetOrderRequest; what did not exist was
// anything upstream that populated it. This is the join: an order carrying a
// route from its Edge claim hands that route to the fleet.
//
// Asserted as a SEQUENCE. SEER walks the points in order and shingo stores
// them in one JSON column, so a store that round-tripped the route as a set
// would be a different route with the same elements.
func TestDispatchDirect_CarriesTheOrdersKeyRoute(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	storageNode, lineNode, payload := setupTestData(t, db)

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := &orders.Order{
		EdgeUUID:     "routed-1",
		StationID:    "edge.line1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusPending,
		Quantity:     1,
		PayloadCode:  payload.Code,
		DeliveryNode: lineNode.Name,
		KeyRoute:     []string{"AISLE_B", "AISLE_A"},
		KeyTask:      "load",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusPending), "fixture"), "pending")
	order.Status = StatusPending

	// Read the order BACK before dispatching. The in-memory struct would carry
	// the route whether or not the column exists, so a test that dispatched
	// the struct it just built would pass against a migration that never ran.
	stored, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "get order")
	if len(stored.KeyRoute) != 2 || stored.KeyRoute[0] != "AISLE_B" || stored.KeyRoute[1] != "AISLE_A" {
		t.Fatalf("key_route did not survive the round trip in order: %v", stored.KeyRoute)
	}
	if stored.KeyTask != "load" {
		t.Fatalf("key_task did not survive the round trip: %q", stored.KeyTask)
	}

	if _, err := d.DispatchDirect(stored, storageNode, lineNode); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("create requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if len(req.KeyRoute) != 2 || req.KeyRoute[0] != "AISLE_B" || req.KeyRoute[1] != "AISLE_A" {
		t.Errorf("CreateOrderRequest.KeyRoute = %v, want [AISLE_B AISLE_A] in that order", req.KeyRoute)
	}
	if req.KeyTask != "load" {
		t.Errorf("CreateOrderRequest.KeyTask = %q, want \"load\"", req.KeyTask)
	}
}

// AN ORDER THAT ARRIVES WITH NO ROUTE STILL GOES OUT WITH NONE, and the column
// reads back as nil rather than an empty-but-present slice. Empty is what makes
// SEER auto-pick, and it is what every order in the plant does today.
func TestDispatchDirect_NoRouteStaysEmpty(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	storageNode, lineNode, payload := setupTestData(t, db)

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := &orders.Order{
		EdgeUUID:     "unrouted-1",
		StationID:    "edge.line1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusPending,
		Quantity:     1,
		PayloadCode:  payload.Code,
		DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusPending), "fixture"), "pending")

	stored, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "get order")
	if stored.KeyRoute != nil {
		t.Errorf("an order with no route reads back as %v, want nil", stored.KeyRoute)
	}

	if _, err := d.DispatchDirect(stored, storageNode, lineNode); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	if reqs := backend.CreateRequests(); len(reqs) != 1 || len(reqs[0].KeyRoute) != 0 || reqs[0].KeyTask != "" {
		t.Errorf("an unrouted order must reach the fleet unrouted; got %+v", reqs)
	}
}
