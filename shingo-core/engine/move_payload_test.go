//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/orders"
)

// emptyIntentMoveOrder builds the "fetch me an empty carrier" shape: it names
// the bin it will take, and its source intent says the bin is wanted AS an
// empty, whatever tag it still carries.
func emptyIntentMoveOrder(t *testing.T, db *store.DB, binID int64, source, delivery string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:     "empty-intent-" + source + "-" + delivery,
		StationID:    "test-station",
		OrderType:    protocol.OrderTypeRetrieveEmpty,
		Status:       protocol.StatusPending,
		Quantity:     1,
		SourceNode:   source,
		DeliveryNode: delivery,
		SourceIntent: dispatch.SourceIntentEmpty,
		BinID:        &binID,
		OriginClass:  protocol.OriginClassNoDemand,
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create empty-intent order: %v", err)
	}
	return o
}

// TestMove_DispatchesAgainstTheBinsPayload is the capability guard.
//
// A move order names no payload — the operator is not asked for one, and the
// planner resolves the source bin without writing its payload back. So every
// move went to the fleet with no robot group at all, which means the vendor's
// default: whichever robot is free.
//
// That is not a labelling problem. The robot group is a CAPABILITY constraint —
// a payload configured for a 1500 kg group could be handed to a 600 kg robot,
// and nothing in the system would have said so. Dispatch also picks the advanced
// load sequence from the same field, so a payload with a child-cart interlock
// would get the plain single-block load.
//
// The order does know which bin it is moving. This asserts the fleet request
// carries the group that bin's payload asks for, and that the order row records
// what it moved.
func TestMove_DispatchesAgainstTheBinsPayload(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, payload := setupTestData(t, db)

	// The part in the bin needs a heavy robot.
	payload.RobotGroup = "HEAVY-1500"
	if err := db.UpdatePayload(payload); err != nil {
		t.Fatalf("set robot group on %s: %v", payload.Code, err)
	}

	createTestBinAtNode(t, db, payload.Code, storageNode.ID, "BIN-MOVE-PAYLOAD")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection:    BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "test-station",
		Desc:         "move a heavy bin",
	})
	if err != nil {
		t.Fatalf("CreateBinMove: %v", err)
	}
	if res.VendorOrderID == "" {
		t.Fatal("no vendor order id — nothing reached the fleet, so this proves nothing")
	}

	group, known := sim.RobotGroupFor(res.VendorOrderID)
	if !known {
		t.Fatalf("fleet has no order %s", res.VendorOrderID)
	}
	if group != "HEAVY-1500" {
		t.Errorf("fleet order asked for robot group %q, want %q.\n"+
			"The bin holds %s, which is configured for a heavy robot. An empty group is the "+
			"vendor default — any robot — which is how a 1500 kg load gets a 600 kg robot.",
			group, "HEAVY-1500", payload.Code)
	}

	got, err := db.GetOrder(res.OrderID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.PayloadCode != payload.Code {
		t.Errorf("order records payload %q, want %q — the orders table is where "+
			"'how much of payload X moves' gets answered, and a move answered blank",
			got.PayloadCode, payload.Code)
	}
}

// TestRetrieveEmpty_StaysPayloadAgnostic is the exclusion, and the reason the
// rule above is not "always read the bin".
//
// An empty carrier is generic on purpose: Edge ships a blank payload code so the
// bin is not pre-tagged, and the real payload binds when the operator loads it
// (lookupPayloadMeta in shingo-edge documents the same rule from its side). A
// carrier that still carries a stale code from its last life must not have that
// code re-applied here — it would pick the robot for a part the bin is not
// carrying.
func TestRetrieveEmpty_StaysPayloadAgnostic(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, payload := setupTestData(t, db)

	payload.RobotGroup = "HEAVY-1500"
	if err := db.UpdatePayload(payload); err != nil {
		t.Fatalf("set robot group: %v", err)
	}

	// A carrier still tagged with its previous contents, being fetched AS AN
	// EMPTY.
	bin := createTestBinAtNode(t, db, payload.Code, storageNode.ID, "BIN-STALE-TAG")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	order := emptyIntentMoveOrder(t, db, bin.ID, storageNode.Name, lineNode.Name)
	vendorID, err := eng.Dispatcher().DispatchDirect(order, storageNode, lineNode)
	if err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	group, known := sim.RobotGroupFor(vendorID)
	if !known {
		t.Fatalf("fleet has no order %s", vendorID)
	}
	if group != "" {
		t.Errorf("an empty-carrier fetch asked for robot group %q, want none — "+
			"the bin's leftover tag is not what it is carrying", group)
	}

	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.PayloadCode != "" {
		t.Errorf("an empty-carrier fetch recorded payload %q; re-tagging an "+
			"intentionally generic empty is the thing this exclusion exists to prevent",
			got.PayloadCode)
	}
}
