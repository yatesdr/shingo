//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestAdmitLaneEntry_ParksDeeperPending is the end-to-end ON test: with the gate
// enabled (lane_enforcement=mouth), a shallow store parks behind a deeper active
// cross-origin store in the same lane — against REAL DB rows, not a fake. It runs
// twice: once with a BARE delivery_node and once with a DOT-qualified one
// ("LANE.SLOT"), because a dotted row invisible to the active-set query would make
// the gate silently admit (the F1 fail-open). Both must park.
func TestAdmitLaneEntry_ParksDeeperPending(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	check := func(prefix string, deliveryFor func(lane, slot string) string) {
		_, laneID, s0 := gatedLane(t, db, prefix, "mouth") // S0 depth0, S1 depth1, mouth-enforced
		laneNode, err := db.GetNode(laneID)
		if err != nil {
			t.Fatalf("[%s] get lane: %v", prefix, err)
		}
		slots, err := db.ListLaneSlots(laneID)
		if err != nil {
			t.Fatalf("[%s] list slots: %v", prefix, err)
		}
		var s1 *nodes.Node
		for _, s := range slots {
			if dpt, _ := db.GetSlotDepth(s.ID); dpt == 1 {
				s1 = s
			}
		}
		if s1 == nil {
			t.Fatalf("[%s] fixture should have a depth-1 slot", prefix)
		}

		// A shallow store queued for S0, and a deeper store active in S1 (its
		// delivery_node written in the format under test). Different origins: both
		// resolve to no style claim → cross-origin (Tier 2).
		shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.DeliveryNode = s0.Name
			o.Status = "queued"
		})
		_ = testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.DeliveryNode = deliveryFor(laneNode.Name, s1.Name)
			o.Status = "in_transit"
		})

		park, cause, err := d.AdmitLaneEntry(shallow, s0)
		if err != nil || !park || cause != causeLaneDeeperPending {
			t.Fatalf("[%s] deep active: park=%v cause=%q err=%v, want park %q", prefix, park, cause, err, causeLaneDeeperPending)
		}
	}

	check("TEBARE", func(lane, slot string) string { return slot })        // bare delivery_node
	check("TEDOT", func(lane, slot string) string { return lane + "." + slot }) // dot-qualified
}

// TestAdmitLaneEntry_OffIsAdmit confirms a non-mouth lane group is byte-identical:
// the gate admits regardless of a deeper active store.
func TestAdmitLaneEntry_OffIsAdmit(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, s0 := gatedLane(t, db, "TEOFF", "") // NOT mouth-enforced
	slots, _ := db.ListLaneSlots(laneID)
	var s1 *nodes.Node
	for _, s := range slots {
		if dpt, _ := db.GetSlotDepth(s.ID); dpt == 1 {
			s1 = s
		}
	}
	shallow := testdb.CreateOrder(t, db, func(o *orders.Order) { o.DeliveryNode = s0.Name; o.Status = "queued" })
	_ = testdb.CreateOrder(t, db, func(o *orders.Order) { o.DeliveryNode = s1.Name; o.Status = "in_transit" })

	if park, _, err := d.AdmitLaneEntry(shallow, s0); err != nil || park {
		t.Fatalf("non-mouth group must admit (byte-identical); park=%v err=%v", park, err)
	}
}
