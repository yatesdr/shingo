//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestLaneEntry_ADeeperStoreThatHoldsNothingIsAPlanNotAPresence: Tier 2's
// never-dispatched arm read a pre-dispatch order's delivery_node as "still
// coming", so a queued order that had only NAMED a deeper slot walled the lane
// to a shallower store (lane-deeper-pending). Packed 477 waited 14 s behind
// order 481, queued behind its pair leader and holding nothing, whose drop was
// then re-chosen into another lane. The same rule as the take (35bc1fff): a
// parked order that only names a slot is a plan, not a presence. A deeper store
// is counted only when it holds a row there, or is dispatched.
func TestLaneEntry_ADeeperStoreThatHoldsNothingIsAPlanNotAPresence(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneID, s0 := gatedLane(t, db, "TEPLAN", "")
	laneNode, err := db.GetNode(laneID)
	testutil.MustNoErr(t, err, "get lane")
	slots, err := db.ListLaneSlots(laneID)
	testutil.MustNoErr(t, err, "list slots")
	var s1 *nodes.Node
	for _, s := range slots {
		if s.ID != s0.ID {
			s1 = s
		}
	}
	shallow := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s0.Name
		o.Status = "queued"
	})
	deeper := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "queued"
	})

	v, err := d.laneEntryCause(laneNode, shallow, s0)
	testutil.MustNoErr(t, err, "naming only")
	if !v.Admitted() {
		t.Fatalf("a queued deeper order that only names %s walls the lane (%q): it holds nothing there, so "+
			"its drop can be re-chosen anywhere, and nothing it does releases this wait", s1.Name, v.Cause())
	}

	// The same order holding its slot is coming there, and still walls it.
	testutil.MustNoErr(t, db.ReserveSlot(s1.ID, deeper.ID), "the deeper order holds its slot")
	v, err = d.laneEntryCause(laneNode, shallow, s0)
	testutil.MustNoErr(t, err, "holding its slot")
	if v.Admitted() || v.Cause() != CauseLaneDeeperPending {
		t.Fatalf("a queued deeper order holding %s does not wall the lane (admitted=%v cause=%q)",
			s1.Name, v.Admitted(), v.Cause())
	}
}
