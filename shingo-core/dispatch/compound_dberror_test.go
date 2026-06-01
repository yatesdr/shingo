//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// R04-1: a real DB error from GetNextChildOrder (not sql.ErrNoRows) must be
// surfaced, not collapsed into the "no more pending children" completion path —
// which would prematurely complete/fail/resume the parent and unlock the lane
// while child reshuffle steps are still queued (the 2026-05-27 three-robots-in-
// one-corridor failure class).
func TestAdvanceCompoundOrder_SurfacesDBError(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	parent := &orders.Order{
		EdgeUUID:     "parent-dberr",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusReshuffling,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	pendingChild := &orders.Order{
		EdgeUUID:      "child-dberr",
		StationID:     parent.StationID,
		OrderType:     OrderTypeMove,
		Status:        StatusPending,
		ParentOrderID: &parent.ID,
		Sequence:      1,
		SourceNode:    lineNode.Name,
		DeliveryNode:  lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(pendingChild), "create pending child")

	// Break the next-pending-child query so GetNextChildOrder returns a real DB
	// error rather than sql.ErrNoRows. (ListChildOrders also fails here, but its
	// error is intentionally swallowed by the sibling-guard, so execution still
	// reaches GetNextChildOrder.)
	_, err := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN status TO status_renamed`)
	testutil.MustNoErr(t, err, "rename status column")

	if err := d.AdvanceCompoundOrder(parent.ID); err == nil {
		t.Fatal("AdvanceCompoundOrder: expected the DB error to be surfaced, got nil (swallowed into the completion path)")
	}
}
