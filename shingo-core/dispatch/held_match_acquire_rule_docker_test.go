//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestReserveReconcile_AHeldBinIsKeptByTheRuleThatAcquiredIt: a pickup acquires
// a bin through findAvailableForNeed, which takes stock of the part for a
// payload pickup and any resident bin, an empty included, for the removal of
// the order's own line position. Re-reserving the same plan must recognise
// those holds as its own. Matched only by (node, empty-status), an empty bin
// held for a non-empty removal read as someone else's: "bins present but none
// available", every pass, for a bin the order itself held. A pass that prepares
// twice (the re-make on the pass that reaches the hand) parked on it for ever:
// the keep-staged cell's level-keeper and REQUEST swaps after a cancelled
// changeover.
func TestReserveReconcile_AHeldBinIsKeptByTheRuleThatAcquiredIt(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	spot := &nodes.Node{Name: "HM-SPOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spot), "spot node")
	carrier := testdb.CreateBinAtNode(t, db, bp.Code, spot.ID, "HM-CARRIER")
	lineBin := testdb.CreateBinAtNode(t, db, "", lineNode.ID, "HM-LINE")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: lineNode.Name}, // removal: any resident bin, this empty too
		{Action: protocol.ActionDropoff, Node: spot.Name + "-OUT"},
		{Action: protocol.ActionPickup, Node: spot.Name, PayloadCode: bp.Code}, // payload pickup: stock of the part
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "held-match-1", spot.Name, lineNode.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps)

	_, first, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "first reserve")
	if first != reserveComplete {
		t.Fatalf("first reserve = %v, want reserveComplete (the empty carrier and the line bin are both takeable)", first)
	}
	_, again, err := d.allocator.reserveComplexPlan(order, plan)
	testutil.MustNoErr(t, err, "second reserve")
	if again != reserveComplete {
		t.Fatalf("second reserve of the same plan = %v, want reserveComplete: the order's own holds read as taken", again)
	}
	res, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list reservations")
	if !reservesExactly(res, carrier.ID, lineBin.ID) {
		t.Fatalf("held = %+v, want exactly the carrier %d and the line bin %d", res, carrier.ID, lineBin.ID)
	}
}

// TestReserveReconcile_AStockPickupKeepsNoHeldEmpty: an empty the order holds at
// a payload pickup's node (held before the bind took stock of the part alone,
// or by a hand) is not that pickup's: the bind would never take it for it
// (rule 5), so the reconcile does not keep it either. The pickup misses, the
// order holds, and the stray hold is released. A removal and an empty leg still
// keep a hold of the other empty-status: whatever stands at the line, and the
// partial an empty leg finishes.
func TestReserveReconcile_AStockPickupKeepsNoHeldEmpty(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	spot := &nodes.Node{Name: "HM2-SPOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spot), "spot node")
	empty := testdb.CreateBinAtNode(t, db, "", spot.ID, "HM2-EMPTY")
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: spot.Name, PayloadCode: bp.Code},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "held-match-2", spot.Name, lineNode.Name, lineNode.Name, bp.Code, steps)
	testdb.ReserveBin(t, db, order.ID, empty.ID)

	_, out, err := d.allocator.reserveComplexPlan(order, BuildComplexPlan(steps))
	testutil.MustNoErr(t, err, "reserve")
	if out == reserveComplete {
		t.Fatalf("reserve = reserveComplete: the payload pickup kept the empty it holds as stock of %s", bp.Code)
	}
	res, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list reservations")
	if len(res) != 0 {
		t.Errorf("held = %+v, want none: the stray hold on the empty %d is released", res, empty.ID)
	}
}
