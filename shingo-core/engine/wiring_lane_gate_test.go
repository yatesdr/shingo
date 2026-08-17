//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// laneWithSlots creates a LANE node with depth-ordered slots (a real node, so the
// reservation rows' node_id FK is satisfied).
func laneWithSlots(t *testing.T, db *store.DB, name string, slotCount int) (int64, []*nodes.Node) {
	t.Helper()
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("get LANE type: %v", err)
	}
	lane := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	for i := 0; i < slotCount; i++ {
		d := i
		slot := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), Enabled: true, ParentID: &lane.ID, Depth: &d}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot: %v", err)
		}
	}
	slots, err := db.ListLaneSlots(lane.ID)
	if err != nil {
		t.Fatalf("list lane slots: %v", err)
	}
	return lane.ID, slots
}

// TestLaneGateWiring_HeldCompoundLegResumesOnLaneClearingEvent is the test the
// dispatch-level coverage cannot be a substitute for.
//
// WHY IT HAS TO LIVE HERE. RedriveHeldCompoundLegs works when it is called; the
// dispatch tests prove that by calling it. What makes the fix real at a plant is
// the SUBSCRIPTION — that the lane-clearing events are actually wired to it. Wire
// it to the wrong event set, or delete the call, and every dispatch-level test
// still passes, the package still compiles, and the plant stays wedged. That is
// DESIGN §16 rule 9 one level up: green while wedged.
//
// So this drives the REAL engine — newTestEngine runs wireEventHandlers, which
// registers the real trigger set — and publishes a real event on the real bus.
// The bus dispatches synchronously on the emitting goroutine, so no polling and
// no sleep: when Emit returns, the handlers have run.
//
// The pre-assertion is load-bearing. Asserting only "dispatched after the event"
// would pass if a background ticker had dispatched the leg on its own, proving
// nothing about the wiring. Asserting it is STILL HELD immediately before the
// event makes the event the only thing that can have released it.
//
// MUTATION: remove the RedriveHeldCompoundLegs line from wireLaneGateHandlers, or
// change its event set, and this fails at the final assertion while every other
// test in the repo stays green.
func TestLaneGateWiring_HeldCompoundLegResumesOnLaneClearingEvent(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	lane, slots := laneWithSlots(t, db, "WIRED-LANE", 3)

	parent := &orders.Order{
		EdgeUUID: "WIRED-parent", StationID: "line-1",
		OrderType: "retrieve", Status: protocol.StatusReshuffling, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	child := &orders.Order{
		EdgeUUID: "WIRED-child", StationID: "line-1",
		OrderType: "move", Status: protocol.StatusPending, Quantity: 1,
		ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: slots[0].Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create child leg")

	// A FOREIGN order is inside the lane, so the leg is refused and no sibling
	// exists whose completion would bring the dispatcher back.
	foreign := &orders.Order{
		EdgeUUID: "WIRED-foreign", StationID: "line-1",
		OrderType: "retrieve", Status: protocol.StatusReshuffling, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(foreign), "create foreign occupant")
	if _, err := reservations.AcquireOccupancy(db.DB, foreign.ID, lane); err != nil {
		t.Fatalf("foreign occupancy: %v", err)
	}

	eng := newTestEngine(t, db, testdb.NewSuccessBackend())

	if err := eng.dispatcher.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (held): %v", err)
	}

	dispatched := func() bool {
		o, err := db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child: %v", err)
		}
		return o.VendorOrderID != ""
	}

	// The lane clears — but nothing has told anyone yet.
	if err := reservations.ReleaseAllOccupancy(db.DB, foreign.ID); err != nil {
		t.Fatalf("release foreign occupancy: %v", err)
	}
	if dispatched() {
		t.Fatal("the leg dispatched before the lane-clearing event was published — something else " +
			"re-drove it, so this test cannot attribute the resumption to the wiring")
	}

	// THE EVENT. A bin left a slot in this lane: the pickout trigger.
	eng.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
		BinID:      1,
		OrderID:    foreign.ID,
		FromNodeID: slots[0].ID,
	}})

	if !dispatched() {
		o, _ := db.GetOrder(child.ID)
		t.Fatalf("a lane-clearing event did not resume the held leg — status %s, vendor %q. "+
			"RedriveHeldCompoundLegs is only real if it is SUBSCRIBED; without the wiring the "+
			"dispatch-level tests still pass and the plant still wedges", o.Status, o.VendorOrderID)
	}
}
