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
// a bin through findAvailableForNeed, which takes an empty carrier for a payload
// pickup (an empty bin carries no payload to mismatch) and any resident bin for
// the removal of the order's own line position. Re-reserving the same plan must
// recognise those holds as its own. Matched only by (node, empty-status), an
// empty bin held for a non-empty step read as someone else's: "bins present but
// none available", every pass, for a bin the order itself held. A pass that
// prepares twice (the re-make on the pass that reaches the hand) parked on it
// for ever: the keep-staged cell's level-keeper and REQUEST swaps after a
// cancelled changeover.
func TestReserveReconcile_AHeldBinIsKeptByTheRuleThatAcquiredIt(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	spot := &nodes.Node{Name: "HM-SPOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spot), "spot node")
	carrier := testdb.CreateBinAtNode(t, db, "", spot.ID, "HM-CARRIER")
	lineBin := testdb.CreateBinAtNode(t, db, "", lineNode.ID, "HM-LINE")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: lineNode.Name}, // removal: any resident bin
		{Action: protocol.ActionDropoff, Node: spot.Name + "-OUT"},
		{Action: protocol.ActionPickup, Node: spot.Name, PayloadCode: bp.Code}, // payload pickup: an empty passes
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
