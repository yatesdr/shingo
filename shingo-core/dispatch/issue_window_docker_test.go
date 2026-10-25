//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// issue_window_docker_test.go — an order issued into a window (Need.Window):
// the window is its delivery, held from its issue, and a window another order
// holds refuses the issue with nothing written.

func windowNeed(ref string, w *nodes.Node) Need {
	return Need{Type: OrderTypeMove, Station: "test", Ref: ref, Quantity: 1,
		OriginClass: protocol.OriginClassNoDemand, Window: w}
}

func TestIssue_AWindowIsTheDeliveryAndHeldFromTheIssue(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	w := &nodes.Node{Name: "IW-W1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(w), "window")

	both := windowNeed("iw-both", w)
	both.Delivery = "IW-ELSEWHERE"
	if _, _, err := d.IssueOrder(both, plan.Plan{}); err == nil {
		t.Fatal("a need naming a window and a delivery was issued: a declared window is the delivery")
	}
	if o, err := db.GetOrderByUUID("iw-both"); err == nil && o != nil {
		t.Errorf("the refused need left order %d behind", o.ID)
	}

	o, _, err := d.IssueOrder(windowNeed("iw-one", w), plan.Plan{})
	testutil.MustNoErr(t, err, "issue into the window")
	if o.DeliveryNode != w.Name {
		t.Errorf("delivery = %q, want the window %s", o.DeliveryNode, w.Name)
	}
	var holder int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT order_id FROM reservations WHERE node_id = $1 AND state = 'pending'`,
		w.ID).Scan(&holder), "the window's hold")
	if holder != o.ID {
		t.Errorf("the window is held by order %d, want the issued order %d", holder, o.ID)
	}
}

func TestIssue_AWindowAnotherOrderHoldsRefusesTheIssue(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	w := &nodes.Node{Name: "IW-W2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(w), "window")
	stranger := testdb.CreateOrder(t, db)
	testutil.MustNoErr(t, db.ReserveSlot(w.ID, stranger.ID), "the stranger holds the window")

	if _, _, err := d.IssueOrder(windowNeed("iw-lost", w), plan.Plan{}); !errors.Is(err, errWindowTaken) {
		t.Fatalf("issue into a held window: %v, want errWindowTaken", err)
	}
	if o, err := db.GetOrderByUUID("iw-lost"); err == nil && o != nil {
		t.Errorf("the refused issue left order %d behind", o.ID)
	}
}
