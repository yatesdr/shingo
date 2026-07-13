//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// setupTwoLanesOneShuffle builds an NGRP with TWO buried lanes and exactly ONE
// shuffle slot between them:
//
//	GRP-2L
//	├── LANE-A: A1 (depth 1, blocker) · A2 (depth 2, target)
//	├── LANE-B: B1 (depth 1, blocker) · B2 (depth 2, target)
//	└── SHUF-1 (the only place a blocker can go)
//
// Every lane slot is occupied, so SHUF-1 is the only free node in the group --
// which is the point: the two digs are forced to want the same one.
func setupTwoLanesOneShuffle(t *testing.T, db *store.DB) (grp *nodes.Node, laneA, laneB *nodes.Node, slotsA, slotsB []*nodes.Node, shuf *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: "P2L"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: "GRP-2L", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mkLane := func(name string) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)
		var slots []*nodes.Node
		for d := 1; d <= 2; d++ {
			depth := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, _ := db.GetNode(lane.ID)
		return reloaded, slots
	}
	laneA, slotsA = mkLane("LANE-A")
	laneB, slotsB = mkLane("LANE-B")

	shuf = &nodes.Node{Name: "SHUF-1", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(shuf), "create shuffle slot")

	grp, _ = db.GetNode(grp.ID)
	return
}

// TestFindShuffleSlots_TwoDigsMustNotShareASlot pins the bug the houseserver sim
// exposed on 2026-07-13 (D83a).
//
// Shuffle slots are a GROUP-scoped shared resource, but the lane lock is keyed on
// the lane being dug -- so two digs in DIFFERENT lanes take different locks and
// both proceed. findShuffleSlots used to ask only "is this node empty RIGHT NOW",
// which is true of a slot that another dig already has a blocker in flight to. So
// both digs picked the same slot, the second blocker landed on the first, and
// EvictStaleGhostsTx threw the first bin to _TRANSIT. On the sim, lane 1 and lane
// 2 each unburied into SMN_008 + SMN_009 three seconds apart: two bins orphaned,
// and lane 1's restore compound left with nothing to restock.
//
// The dig legs carry delivery_node, so CheckDropoffCapacity -- the gate every
// other dropoff in the system passes -- already counted them. findShuffleSlots
// just never asked.
func TestFindShuffleSlots_TwoDigsMustNotShareASlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, laneA, laneB, slotsA, slotsB, shuf, bp := setupTwoLanesOneShuffle(t, db)

	// Both lanes buried: a blocker at the mouth, the target behind it.
	createTestBinAtNode(t, db, bp.Code, slotsA[0].ID, "BIN-A-BLK")
	targetA := createTestBinAtNode(t, db, bp.Code, slotsA[1].ID, "BIN-A-TGT")
	createTestBinAtNode(t, db, bp.Code, slotsB[0].ID, "BIN-B-BLK")
	targetB := createTestBinAtNode(t, db, bp.Code, slotsB[1].ID, "BIN-B-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Dig 1 (lane A) plans first and takes the only shuffle slot.
	orderA := &orders.Order{EdgeUUID: "dig-a", StationID: "line-1", OrderType: OrderTypeRetrieve, Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-2L"}
	testutil.MustNoErr(t, db.CreateOrder(orderA), "create order A")
	_, peA := d.planner.planBuriedReshuffle(orderA, &BuriedError{Bin: targetA, Slot: slotsA[1], LaneID: laneA.ID})
	if peA != nil {
		t.Fatalf("dig A should have planned (the shuffle slot was free): %s: %s", peA.Code, peA.Detail)
	}

	// Dig 2 (lane B) plans while dig A's blocker is in flight to SHUF-1. It must
	// NOT take that slot. Nothing physically occupies SHUF-1 yet -- that is exactly
	// the trap.
	orderB := &orders.Order{EdgeUUID: "dig-b", StationID: "line-1", OrderType: OrderTypeRetrieve, Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-2L"}
	testutil.MustNoErr(t, db.CreateOrder(orderB), "create order B")
	_, peB := d.planner.planBuriedReshuffle(orderB, &BuriedError{Bin: targetB, Slot: slotsB[1], LaneID: laneB.ID})

	// THE INVARIANT: exactly one dig may be inbound to SHUF-1.
	var inbound int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM orders WHERE delivery_node = $1 AND parent_order_id IS NOT NULL`, shuf.Name,
	).Scan(&inbound), "count legs inbound to the shuffle slot")
	if inbound != 1 {
		t.Fatalf("%d dig legs are inbound to %s, want exactly 1.\n"+
			"Two digs in different lanes booked the same shuffle slot: the second blocker will land on "+
			"the first, and ApplyArrival evicts the incumbent to _TRANSIT. This is the sim's SMN_008/"+
			"SMN_009 orphaning (D83a).", inbound, shuf.Name)
	}

	// And dig B must WAIT, not die: "no free shuffle slot" is congestion.
	if peB == nil {
		t.Fatalf("dig B planned a compound with no shuffle slot available — it must be refused")
	}
	if peB.Code != codeNoShuffleSlot {
		t.Fatalf("dig B refused with code %q (%s), want %q — a crowded group is not a broken lane; "+
			"it must WAIT for a slot (D18-Q4 wait-not-fail)", peB.Code, peB.Detail, codeNoShuffleSlot)
	}
	if !peB.Transient() {
		t.Errorf("dig B's refusal is not Transient() — it will terminally fail the order instead of retrying")
	}
	_ = grp
	_ = laneB
}
