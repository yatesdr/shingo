//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestRedirect_ReleasesTheOldDestinationsSlot is round-5 defect 4.
//
// A redirect re-aims delivery_node and left the OLD destination's slot claim and
// reservation standing. Nothing ever released them: the order is no longer going
// there, so no arrival clears them, and the terminal release fires against
// whatever the order holds at the END — which by then is the NEW slot. The old
// one is held by a live order that has no intention of visiting it, forever, and
// every demand that would have used it is refused by a hold nobody can trace to
// a robot.
//
// The interim is cheap because the primitive already exists and its own doc
// names this case: ReleaseSlotClaim is "the coupled inverse of ConfirmSlotClaim
// ... for a CONFIRMED slot the reserve reconcile abandons (a re-resolution moved
// the dropoff)". A redirect IS a re-resolution that moves the dropoff.
func TestRedirect_ReleasesTheOldDestinationsSlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	oldDest := &nodes.Node{Name: "REDIR-OLD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(oldDest), "create the old destination")
	newDest := &nodes.Node{Name: "REDIR-NEW", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(newDest), "create the new destination")

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "REDIR-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "redir-slot-leak"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = oldDest.Name
		o.Status = StatusDispatched
	})
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	// The destination slot, held the way a dispatched order holds one: reserved,
	// then confirmed into a hard claim.
	testutil.MustNoErr(t, db.ReserveSlot(oldDest.ID, order.ID), "reserve the old destination")
	testutil.MustNoErr(t, db.ConfirmSlotClaim(oldDest.ID, order.ID), "claim the old destination")
	order, _ = db.GetOrder(order.ID)

	_, _, err := d.lifecycle.PrepareRedirect(order, newDest.Name)
	testutil.MustNoErr(t, err, "prepare the redirect")

	var claimedBy *int64
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT claimed_by FROM nodes WHERE id=$1`, oldDest.ID).Scan(&claimedBy), "read the old slot's claim")
	if claimedBy != nil {
		t.Errorf("the OLD destination is still hard-claimed by order %d after the redirect.\n"+
			"The order is going somewhere else. Nothing will ever arrive there to clear this, and "+
			"the terminal release fires against the NEW slot — so a live order holds a slot it has "+
			"no intention of visiting, and every demand that wanted it is refused by a hold nobody "+
			"can trace to a robot.", *claimedBy)
	}
	var res int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM reservations WHERE node_id=$1 AND order_id=$2`,
		oldDest.ID, order.ID).Scan(&res), "read the old slot's paper")
	if res != 0 {
		t.Errorf("the OLD destination still carries %d reservation(s) for order %d. The claim and "+
			"the paper are released together or the books come apart — that is what makes "+
			"ReleaseSlotClaim coupled.", res, order.ID)
	}
}

// TestRedirect_ToTheSameNodeKeepsItsSlot is the selectivity half. A redirect
// that names the destination the order already has must not drop the hold it is
// still going to use — the release is scoped to a destination being LEFT.
func TestRedirect_ToTheSameNodeKeepsItsSlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	dest := &nodes.Node{Name: "REDIR-SAME", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create the destination")
	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "REDIR-SAME-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "redir-same-node"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = dest.Name
		o.Status = StatusDispatched
	})
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	testutil.MustNoErr(t, db.ReserveSlot(dest.ID, order.ID), "reserve the destination")
	testutil.MustNoErr(t, db.ConfirmSlotClaim(dest.ID, order.ID), "claim the destination")
	order, _ = db.GetOrder(order.ID)

	_, _, err := d.lifecycle.PrepareRedirect(order, dest.Name)
	testutil.MustNoErr(t, err, "prepare the no-op redirect")

	var claimedBy *int64
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT claimed_by FROM nodes WHERE id=$1`, dest.ID).Scan(&claimedBy), "read the slot's claim")
	if claimedBy == nil || *claimedBy != order.ID {
		t.Errorf("the destination's claim = %v, want order %d kept — a redirect to the node the "+
			"order already has leaves nothing, so there is nothing to release", claimedBy, order.ID)
	}
}
