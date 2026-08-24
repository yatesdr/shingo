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
	// error rather than sql.ErrNoRows. GetNextChildOrder is the FIRST read in
	// AdvanceCompoundOrder, so it fails before anything else runs and this test
	// never reaches the chapter-close half. The unreadable-child-list case that
	// lives there is pinned separately, by
	// TestAdvanceCompoundChapterEnd_UnreadableChildListDoesNotComplete.
	_, err := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN status TO status_renamed`)
	testutil.MustNoErr(t, err, "rename status column")

	if err := d.AdvanceCompoundOrder(parent.ID); err == nil {
		t.Fatal("AdvanceCompoundOrder: expected the DB error to be surfaced, got nil (swallowed into the completion path)")
	}
}

// The chapter-close twin of the test above. AdvanceCompoundOrder fails closed
// when its next-pending-child read errors; advanceCompoundChapterEnd used to
// log and carry on when ITS child-list read errored, which is strictly worse,
// because that is the half that completes the parent and releases its lanes.
//
// An unreadable list is not an empty one, but every gate downstream reads it
// that way: digWasDissolved(nil) is false, hasFailedOrCancelled stays false
// over an empty set, and allTerminal keeps its `true` initialiser because the
// loop never runs. Execution fell past the stopped-short arm and the in-flight
// guard to the completion fork, confirming or resuming the parent and releasing
// its held lanes with legs potentially still flying — the 2026-05-27
// three-robots-in-one-corridor failure class.
//
// This calls advanceCompoundChapterEnd directly rather than through
// AdvanceCompoundOrder because both reads run off the same SelectCols against
// the same table: any break that reaches the second one also fails the first,
// and the parent would never enter the chapter-close half at all.
func TestAdvanceCompoundChapterEnd_UnreadableChildListDoesNotComplete(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	parent := &orders.Order{
		EdgeUUID:     "parent-chapter-dberr",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusReshuffling,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	// A child that has NOT reached a terminal status. If the child list were
	// readable, allTerminal would be false and the parent would be left alone
	// for that reason — so a passing test here has to come from the read
	// failure, not from the in-flight guard doing the work.
	child := &orders.Order{
		EdgeUUID:      "child-chapter-dberr",
		StationID:     parent.StationID,
		OrderType:     OrderTypeMove,
		Status:        StatusDispatched,
		ParentOrderID: &parent.ID,
		Sequence:      1,
		SourceNode:    lineNode.Name,
		DeliveryNode:  lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create child")

	_, err := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN status TO status_renamed`)
	testutil.MustNoErr(t, err, "rename status column")

	chapterErr := d.advanceCompoundChapterEnd(parent.ID)

	// Restore before asserting on stored state, so the reads below work.
	_, rerr := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN status_renamed TO status`)
	testutil.MustNoErr(t, rerr, "restore status column")

	if chapterErr == nil {
		t.Fatal("advanceCompoundChapterEnd: expected the child-list read error to be surfaced, got nil (swallowed into the completion path)")
	}

	got, gerr := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, gerr, "reload parent")
	if got.Status != StatusReshuffling {
		t.Fatalf("parent status = %q, want %q: an unreadable child list decided the compound's fate", got.Status, StatusReshuffling)
	}
}
