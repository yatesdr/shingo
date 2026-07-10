//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestAdvanceCompoundOrder_CancelledChildFailsParent pins the 2026-07-09 decision:
// a compound (reshuffle) whose children are ALL terminal but include a CANCELLED
// child fails the parent — it does NOT take the success (resume/complete) branch.
// A cancelled reshuffle leg means the housekeeping didn't complete, so a plain
// parent must not be marked Confirmed with its retrieve never run (and a coordinated
// parent must not resume against a still-buried bin).
func TestAdvanceCompoundOrder_CancelledChildFailsParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := &orders.Order{
		EdgeUUID:     "parent-cancel-leg",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve, // plain-retrieve compound
		Status:       StatusReshuffling,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	// One completed child + one cancelled child — all terminal, one cancelled.
	doneChild := &orders.Order{
		EdgeUUID: "child-done", StationID: parent.StationID, OrderType: OrderTypeMove,
		Status: StatusConfirmed, ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(doneChild), "create done child")
	cancelledChild := &orders.Order{
		EdgeUUID: "child-cancelled", StationID: parent.StationID, OrderType: OrderTypeMove,
		Status: StatusCancelled, ParentOrderID: &parent.ID, Sequence: 2,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(cancelledChild), "create cancelled child")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "AdvanceCompoundOrder")

	got, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if got.Status != StatusFailed {
		t.Fatalf("parent status = %q, want %q (a cancelled reshuffle leg must fail the parent, not complete it)",
			got.Status, StatusFailed)
	}
}

// TestAdvanceCompoundOrder_AllChildrenConfirmed_Completes is the positive twin: a
// plain-retrieve compound whose children all CONFIRMED cleanly completes the parent
// (Reshuffling → Confirmed). Guards against the cancelled-fails-parent change
// over-firing on a clean compound.
func TestAdvanceCompoundOrder_AllChildrenConfirmed_Completes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := &orders.Order{
		EdgeUUID:     "parent-clean",
		StationID:    "line-1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusReshuffling,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	child := &orders.Order{
		EdgeUUID: "child-clean", StationID: parent.StationID, OrderType: OrderTypeMove,
		Status: StatusConfirmed, ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create child")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "AdvanceCompoundOrder")

	got, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if got.Status != StatusConfirmed {
		t.Fatalf("parent status = %q, want %q (a clean all-confirmed plain compound completes)", got.Status, StatusConfirmed)
	}
}
