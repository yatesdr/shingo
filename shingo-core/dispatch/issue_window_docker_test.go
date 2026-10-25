//go:build docker

package dispatch

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
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

	if _, _, err := d.IssueOrder(windowNeed("iw-lost", w), plan.Plan{}); !errors.Is(err, ErrWindowTaken) {
		t.Fatalf("issue into a held window: %v, want ErrWindowTaken", err)
	}
	if o, err := db.GetOrderByUUID("iw-lost"); err == nil && o != nil {
		t.Errorf("the refused issue left order %d behind", o.ID)
	}
}

// windowHolder is the order whose pending slot row is on node, or 0.
func windowHolder(t *testing.T, db *store.DB, nodeID int64) int64 {
	t.Helper()
	var holder int64
	err := db.QueryRow(`SELECT order_id FROM reservations WHERE node_id = $1 AND resource_kind = 'slot'
		AND state = 'pending'`, nodeID).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	testutil.MustNoErr(t, err, "the window's hold")
	return holder
}

// TestCoreAsk_HoldsItsWindowAtIssue: a Core ask holds the window it chose from
// its issue, a pending slot row at the commit that writes its row (the census's
// at-commit record), for an ask of each caller: the loader loop's empty and its
// full (ReplenishLoader, no acquirer wired), and the level keeper's ask at the
// slot it pre-resolved (its spec, through AdmitCoreAsk; engine's census drives
// the keeper's own tick).
func TestCoreAsk_HoldsItsWindowAtIssue(t *testing.T) {
	t.Parallel()
	held := func(t *testing.T, db *store.DB, o *orders.Order, w *nodes.Node) {
		t.Helper()
		at, _, ok := testdb.IssuedAtCommit(t, db, o.ID)
		if !ok || !at.SlotRow {
			t.Errorf("the ask (order %d) held %s at the commit that issued it, want its window %s's slot row",
				o.ID, at, w.Name)
		}
		if h := windowHolder(t, db, w.ID); h != o.ID {
			t.Errorf("window %s is held by order %d, want the ask %d", w.Name, h, o.ID)
		}
	}
	for _, pull := range []protocol.OrderType{OrderTypeRetrieveEmpty, OrderTypeRetrieve} {
		t.Run("loader loop "+string(pull), func(t *testing.T) {
			t.Parallel()
			db := testdb.IssueCensusDB(t)
			d, _ := newTestDispatcher(t, db, nil)
			req, cfg, w := oneWindowLoader(t, db, "CAH")
			req.OrderType = pull
			res, err := d.ReplenishLoader(req, cfg)
			testutil.MustNoErr(t, err, "ReplenishLoader")
			if len(res.Created) != 1 {
				t.Fatalf("the loop asked %d carrier(s) (held %v, skipped %q), want 1", len(res.Created), res.HeldBy, res.Skipped)
			}
			held(t, db, res.Created[0], w)
		})
	}
	t.Run("level keeper", func(t *testing.T) {
		t.Parallel()
		db := testdb.IssueCensusDB(t)
		d, _ := newTestDispatcher(t, db, nil)
		grp, err := db.CreateNodeGroup("CAH-MNT")
		testutil.MustNoErr(t, err, "the maintained group")
		slot := &nodes.Node{Name: "CAH-MNT-PA", Enabled: true, ParentID: &grp}
		testutil.MustNoErr(t, db.CreateNode(slot), "its position")
		o, err := d.AdmitCoreAsk(CoreAskSpec{UUIDPrefix: "core-mnt-", OrderType: OrderTypeRetrieveEmpty,
			StationID: "test-core", DeliveryNode: slot.Name, PayloadDesc: "empty CAH → CAH-MNT",
			OriginClass: protocol.OriginClassOrphan})
		testutil.MustNoErr(t, err, "the keeper's ask")
		held(t, db, o, slot)
	})
}

// TestCoreAsk_ASecondAskForOneWindowIsRefusedAtIssue: two asks for one window,
// an empty and then a full (the window choice's open-ask arm counts only its
// own intent, so only the window's row can tell them apart): the first holds
// the window from its issue and the second is refused at its issue, with
// nothing written.
func TestCoreAsk_ASecondAskForOneWindowIsRefusedAtIssue(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	src := &nodes.Node{Name: "CA2-MARKET", Enabled: true}
	w := &nodes.Node{Name: "CA2-W", Enabled: true}
	for _, n := range []*nodes.Node{src, w} {
		testutil.MustNoErr(t, db.CreateNode(n), n.Name)
	}
	spec := CoreAskSpec{UUIDPrefix: "core-l1-", OrderType: OrderTypeRetrieveEmpty, StationID: "edge.line1",
		SourceNode: src.Name, DeliveryNode: w.Name, OriginClass: protocol.OriginClassOrphan}
	first, err := d.AdmitCoreAsk(spec)
	testutil.MustNoErr(t, err, "the first ask")
	spec.UUIDPrefix, spec.OrderType = "core-u1-", OrderTypeRetrieve
	second, err := d.AdmitCoreAsk(spec)
	if err == nil {
		t.Fatalf("a second ask for %s was issued (order %d) while order %d holds it", w.Name, second.ID, first.ID)
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM orders WHERE delivery_node = $1`, w.Name).Scan(&n), "orders to the window")
	if n != 1 {
		t.Errorf("%d order(s) deliver to %s, want the first ask alone: a refused issue writes nothing", n, w.Name)
	}
	if h := windowHolder(t, db, w.ID); h != first.ID {
		t.Errorf("window %s is held by order %d, want the first ask %d", w.Name, h, first.ID)
	}
}
