//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// NOTHING IS HELD FOR AN ORDER THAT HAS ENDED.
//
// The scanner reserves for an order from the row it read; a cancel can land in
// between. TerminalizeOrder deletes the order's reservations in its own
// transaction, so a reservation written after that, or alongside it, outlives
// the order: the next order on the same bin and slots waits behind holds nobody
// will ever use (the recovery scenario, single-robot case c: three pending
// reservations written 14-36 ms after the order was cancelled).

type endedOwnerFixture struct {
	db      *store.DB
	d       *Dispatcher
	backend *testdb.MockTrackingBackend
	src     *nodes.Node
	slot    *nodes.Node
	payload string
	bin     int64 // the bin seeded at src
}

func (f *endedOwnerFixture) binID(t *testing.T) int64 { t.Helper(); return f.bin }

func seedEndedOwner(t *testing.T, prefix string) *endedOwnerFixture {
	t.Helper()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: prefix + "-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "NGRP")
	slot := &nodes.Node{Name: prefix + "-SLOT", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "slot")
	src := &nodes.Node{Name: prefix + "-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "src")
	b := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, prefix+"-BIN")
	return &endedOwnerFixture{db: db, d: d, backend: backend, src: src, slot: slot, payload: bp.Code, bin: b.ID}
}

func (f *endedOwnerFixture) order(t *testing.T, uuid string) *orders.Order {
	t.Helper()
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: f.src.Name},
		{Action: protocol.ActionDropoff, Node: f.slot.Name},
	}
	return mkComplexOrder(t, f.db, uuid, f.src.Name, f.src.Name, f.slot.Name, f.payload, steps)
}

// holdsNothing: no reservation, bin claim, node claim or order_bins row is left
// for the order.
func (f *endedOwnerFixture) holdsNothing(t *testing.T, orderID int64) {
	t.Helper()
	res, err := f.db.ListReservationsByOrder(orderID)
	testutil.MustNoErr(t, err, "reservations")
	if len(res) != 0 {
		t.Errorf("the ended order holds %d reservation(s): %+v", len(res), res)
	}
	var claimedBins, claimedNodes, junction int
	testutil.MustNoErr(t, f.db.DB.QueryRow(`SELECT count(*) FROM bins WHERE claimed_by=$1`, orderID).Scan(&claimedBins), "bins")
	testutil.MustNoErr(t, f.db.DB.QueryRow(`SELECT count(*) FROM nodes WHERE claimed_by=$1`, orderID).Scan(&claimedNodes), "nodes")
	testutil.MustNoErr(t, f.db.DB.QueryRow(`SELECT count(*) FROM order_bins WHERE order_id=$1`, orderID).Scan(&junction), "order_bins")
	if claimedBins+claimedNodes+junction != 0 {
		t.Errorf("the ended order holds bins=%d nodes=%d order_bins=%d", claimedBins, claimedNodes, junction)
	}
}

// Sequential: the cancel committed before the scanner's pass reserved from the
// row it had read.
func TestReservations_NoneWrittenForAnOrderEndedBeforeThePass(t *testing.T) {
	t.Parallel()
	f := seedEndedOwner(t, "ROE1")
	// The source bin is taken by another order, so the pass reserves the slot,
	// finds its bin missing and HOLDS its partial set: the shape of case c, and
	// the one no fail path cleans up after.
	bins, err := f.db.ListBinsByNode(f.src.ID)
	testutil.MustNoErr(t, err, "bins")
	holder := testdb.CreateOrder(t, f.db)
	testdb.ClaimBinForTest(t, f.db, bins[0].ID, holder.ID)
	stale := f.order(t, "roe1-dead")
	_, err = f.db.TerminalizeOrder(stale.ID, protocol.StatusCancelled, "test: cancelled by the operator")
	testutil.MustNoErr(t, err, "cancel")
	ended, err := f.db.GetOrder(stale.ID)
	testutil.MustNoErr(t, err, "reload")
	hist, err := f.db.ListOrderHistory(stale.ID)
	testutil.MustNoErr(t, err, "history")

	_ = f.d.DispatchPreparedComplex(stale) // the scanner's row, read before the cancel

	f.holdsNothing(t, stale.ID)
	after, err := f.db.GetOrder(stale.ID)
	testutil.MustNoErr(t, err, "reload")
	if after.Status != ended.Status || after.QueueCause != ended.QueueCause || after.QueueReason != ended.QueueReason {
		t.Errorf("the refused pass rewrote the ended order: %s/%q/%q -> %s/%q/%q", ended.Status, ended.QueueCause,
			ended.QueueReason, after.Status, after.QueueCause, after.QueueReason)
	}
	if hist2, err := f.db.ListOrderHistory(stale.ID); err != nil || len(hist2) != len(hist) {
		t.Errorf("the refused pass wrote history: %d -> %d rows (%v)", len(hist), len(hist2), err)
	}
	if n := len(f.backend.Orders()); n != 0 {
		t.Fatalf("%d fleet order(s) for an ended order", n)
	}

	// The holder lets the bin go; the next order on the same bin and slot goes.
	_, err = f.db.TerminalizeOrder(holder.ID, protocol.StatusCancelled, "test: the holder is done")
	testutil.MustNoErr(t, err, "release the bin")
	next := f.order(t, "roe1-next")
	if err := f.d.DispatchPreparedComplex(next); err != nil {
		t.Fatalf("the next order on the same bin and slot did not dispatch: %v", err)
	}
	if n := len(f.backend.Orders()); n != 1 {
		t.Fatalf("fleet orders = %d, want the next order's 1", n)
	}
}

// Overlapping: the terminalize transaction is open (status written, holds
// deleted, not yet committed) while the insert runs. Every reservation insert
// form, against an owner the open transaction is ending.
func TestReservations_NoneWrittenAlongsideAnOpenTerminalize(t *testing.T) {
	t.Parallel()
	f := seedEndedOwner(t, "ROE2")
	other := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = StatusSourcing })
	bins, err := f.db.ListBinsByNode(f.src.ID)
	testutil.MustNoErr(t, err, "bins")
	binID := bins[0].ID

	// Each form gets its own lane, so a row one form leaks cannot refuse the next.
	lanes := map[string]*nodes.Node{}
	for _, name := range []string{"mouth", "occupancy", "hand-off"} {
		n := &nodes.Node{Name: "ROE2-LANE-" + name, Enabled: true}
		testutil.MustNoErr(t, f.db.CreateNode(n), "lane "+name)
		lanes[name] = n
	}
	forms := []struct {
		name   string
		setup  func() // before the terminalize opens
		insert func(owner int64) error
	}{
		{"bin", nil, func(owner int64) error { return reservations.Acquire(f.db.DB, owner, owner, binID, "test") }},
		{"slot", nil, func(owner int64) error { return reservations.AcquireSlot(f.db.DB, owner, f.slot.ID, "test") }},
		{"mouth", nil, func(owner int64) error {
			return reservations.AcquireLanes(f.db.DB, owner, reservations.ModeOutbound, "test", lanes["mouth"].ID)
		}},
		{"occupancy", nil, func(owner int64) error {
			_, err := reservations.AcquireOccupancy(f.db.DB, owner, lanes["occupancy"].ID)
			return err
		}},
		{"hand-off", func() {
			testutil.MustNoErr(t, reservations.AcquireLanes(f.db.DB, other.ID, reservations.ModeDig, "test", lanes["hand-off"].ID), "dig row")
		}, func(owner int64) error {
			_, err := reservations.HandOffLaneToPicker(f.db.DB, lanes["hand-off"].ID, other.ID, owner, "test")
			return err
		}},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			if form.setup != nil {
				form.setup()
			}
			owner := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = StatusSourcing })
			tx, err := f.db.DB.Begin()
			testutil.MustNoErr(t, err, "begin the terminalize")
			defer tx.Rollback()
			_, err = tx.Exec(`UPDATE orders SET status='cancelled' WHERE id=$1`, owner.ID)
			testutil.MustNoErr(t, err, "status")
			_, err = tx.Exec(`DELETE FROM reservations WHERE order_id=$1`, owner.ID)
			testutil.MustNoErr(t, err, "release")

			done := make(chan error, 1)
			go func() { done <- form.insert(owner.ID) }()
			time.Sleep(300 * time.Millisecond) // the insert runs while the terminalize is open
			testutil.MustNoErr(t, tx.Commit(), "commit the terminalize")
			select {
			case ierr := <-done:
				t.Logf("insert returned %v", ierr)
			case <-time.After(10 * time.Second):
				t.Fatal("the insert never returned")
			}
			res, err := f.db.ListReservationsByOrder(owner.ID)
			testutil.MustNoErr(t, err, "reservations")
			if len(res) != 0 {
				t.Fatalf("%d reservation(s) left for the order the open terminalize ended: %+v", len(res), res)
			}
			testutil.MustNoErr(t, reservations.ReleaseByOrder(f.db.DB, other.ID), "clear the dig row")
		})
	}
}
