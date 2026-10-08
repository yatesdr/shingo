//go:build docker

package nodes_test

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// spokenForLane builds a marked group with one two-deep lane and returns the
// group and the lane's slots, mouth first.
func spokenForLane(t *testing.T, db *store.DB, prefix string) (int64, *nodes.Node, *nodes.Node) {
	t.Helper()
	grp := testdb.MarkedGroup(t, db, prefix+"-GRP")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("LANE type: %v", err)
	}
	lane := &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &laneType.ID, ParentID: grp, Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	var slots []*nodes.Node
	for d := 1; d <= 2; d++ {
		depth := d
		s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", prefix, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
		if err := db.CreateNode(s); err != nil {
			t.Fatalf("create slot: %v", err)
		}
		slots = append(slots, s)
	}
	return *grp, slots[0], slots[1]
}

// namingOrder is an order at status whose delivery_node names slot and that
// holds no row for it.
func namingOrder(t *testing.T, db *store.DB, uuid string, status protocol.Status, slot *nodes.Node) int64 {
	t.Helper()
	o := &orders.Order{EdgeUUID: uuid, StationID: "edge.1", OrderType: "complex", Status: status, Quantity: 1,
		DeliveryNode: slot.Name}
	if err := orders.Create(db.DB, o); err != nil {
		t.Fatalf("create order %s: %v", uuid, err)
	}
	return o.ID
}

// TestSlotSpokenFor_AParkedOrderThatOnlyNamesASlotDoesNotHoldIt is the demo
// wedge's read (C2b, 2026-10-07 runs on 62666564).
//
// A parked swap return holds nothing: parkPair released its slot with every
// other holding. Its delivery_node still names the empty mouth intake chose, and
// asIntent does not change that: it re-chooses the step, lands on the slot the
// order already names, and writes nothing. Counted as spoken for, that slot was
// missing from a dwelling dig's parking (SlotTakeableSQL, through
// TakeableSlotsInGroup), and so were the slots in front of it
// (EntombsASpokenForSlotSQL, through SlotsThatWouldEntombASpokenForSlot): the dig
// stood under no-shuffle-slot and the market could not drain.
//
// A pre-dispatch order counts only through the rows it holds. One that holds the
// slot soft-reserves it, and the reservation clause counts that. Once dispatched,
// naming the slot still counts.
func TestSlotSpokenFor_AParkedOrderThatOnlyNamesASlotDoesNotHoldIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	grp, mouth, deep := spokenForLane(t, db, "SFP-Q")
	namingOrder(t, db, "sfp-parked", protocol.StatusQueued, deep)
	take, err := nodes.TakeableSlotsInGroup(db.DB, grp, 0)
	if err != nil {
		t.Fatalf("takeable slots: %v", err)
	}
	if !take[deep.ID].Free {
		t.Errorf("%s is not free to a dig while the only thing on it is a queued order that names it and "+
			"holds no row: that order holds nothing, and the slot is the parking the dig waits for", deep.Name)
	}
	entomb, err := db.SlotsThatWouldEntombASpokenForSlot(grp)
	if err != nil {
		t.Fatalf("entombing slots: %v", err)
	}
	if entomb[mouth.ID] {
		t.Errorf("%s is refused as parking for entombing %s, which only a queued order holding nothing names",
			mouth.Name, deep.Name)
	}

	// Control: a dispatched order naming the slot still holds it.
	grp2, mouth2, deep2 := spokenForLane(t, db, "SFP-D")
	namingOrder(t, db, "sfp-dispatched", protocol.StatusDispatched, deep2)
	take2, err := nodes.TakeableSlotsInGroup(db.DB, grp2, 0)
	if err != nil {
		t.Fatalf("takeable slots: %v", err)
	}
	if take2[deep2.ID].Free {
		t.Errorf("%s reads free while a dispatched order is bound for it", deep2.Name)
	}
	if entomb2, err := db.SlotsThatWouldEntombASpokenForSlot(grp2); err != nil || !entomb2[mouth2.ID] {
		t.Errorf("%s is not refused in front of a slot a dispatched order is bound for (err %v)", mouth2.Name, err)
	}

	// Control: a queued order holding a reservation on the slot still holds it.
	grp3, _, deep3 := spokenForLane(t, db, "SFP-R")
	holder := namingOrder(t, db, "sfp-reserving", protocol.StatusQueued, deep3)
	if err := reservations.AcquireSlot(db.DB, holder, deep3.ID, "test"); err != nil {
		t.Fatalf("reserve the slot: %v", err)
	}
	take3, err := nodes.TakeableSlotsInGroup(db.DB, grp3, 0)
	if err != nil {
		t.Fatalf("takeable slots: %v", err)
	}
	if take3[deep3.ID].Free {
		t.Errorf("%s reads free while a queued order holds its reservation", deep3.Name)
	}
}
