//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// An arrival frees its destination slot: the placer's dispatch-time claim on
// the node and its slot reservation (PlaceBinTx step 5). It released whatever
// was there, any order's: `nodes.claimed_by=NULL WHERE id=` and every
// reservation row on the node. Whether a bin can be set down at a slot another
// order holds rests on every storage drop holding its own slot row first, which
// no one place enforces, so the release is scoped as the bin's claim release is:
// the placer's own, or its compound family's (its parent and its siblings).
//
// RED at dbcc2bb2 (pins/place-bin-slot-scope-red-dbcc2bb2.log): the stranger's
// claim and reservation are gone.

type slotWorld struct {
	db   *store.DB
	slot *nodes.Node
	bin  int64
}

func newSlotWorld(t *testing.T, prefix string) slotWorld {
	t.Helper()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	slot := &nodes.Node{Name: prefix + "-SLOT", NodeTypeID: sd.StorageNode.NodeTypeID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "the slot")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, prefix+"-BIN")
	return slotWorld{db: db, slot: slot, bin: bin.ID}
}

// holdSlot gives orderID the slot the way a dispatch does: a confirmed slot
// reservation and the node's claim.
func (w slotWorld) holdSlot(t *testing.T, orderID int64) {
	t.Helper()
	testutil.MustNoErr(t, reservations.AcquireSlot(w.db.DB, orderID, w.slot.ID, "test"), "reserve the slot")
	testutil.MustNoErr(t, w.db.ConfirmSlotReservation(w.slot.ID, orderID), "confirm it")
	_, err := w.db.DB.Exec(`UPDATE nodes SET claimed_by=$1 WHERE id=$2`, orderID, w.slot.ID)
	testutil.MustNoErr(t, err, "claim the slot")
}

func (w slotWorld) place(t *testing.T, placer int64) {
	t.Helper()
	tx, err := w.db.Begin()
	testutil.MustNoErr(t, err, "begin")
	defer tx.Rollback()
	_, err = w.db.PlaceBinTx(tx, store.BinPlacement{
		BinID: w.bin, ToNodeID: w.slot.ID, PlacedByOrder: placer, ReleaseDestinationSlot: true,
	})
	testutil.MustNoErr(t, err, "place the bin")
	testutil.MustNoErr(t, tx.Commit(), "commit")
}

func (w slotWorld) holder(t *testing.T) (claimedBy *int64, rows int) {
	t.Helper()
	testutil.MustNoErr(t, w.db.DB.QueryRow(`SELECT claimed_by FROM nodes WHERE id=$1`, w.slot.ID).Scan(&claimedBy), "claim")
	testutil.MustNoErr(t, w.db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE node_id=$1`, w.slot.ID).Scan(&rows), "rows")
	return claimedBy, rows
}

func TestPlaceBin_AnArrivalLeavesAStrangersSlotAlone(t *testing.T) {
	t.Parallel()
	w := newSlotWorld(t, "SCOPE-STRANGER")
	// A robot's order: dispatched, so its confirmed slot is a hold in flight.
	stranger := testdb.CreateOrder(t, w.db, func(o *orders.Order) {
		o.EdgeUUID = "scope-stranger"
		o.Status = protocol.StatusDispatched
	})
	placer := testdb.CreateOrder(t, w.db, func(o *orders.Order) { o.EdgeUUID = "scope-placer" })
	w.holdSlot(t, stranger.ID)

	w.place(t, placer.ID)

	if by, rows := w.holder(t); by == nil || *by != stranger.ID || rows != 1 {
		t.Errorf("after another order's arrival the slot is claimed by %v with %d reservation row(s); want order %d's claim and its row",
			by, rows, stranger.ID)
	}
}

func TestPlaceBin_AnArrivalFreesItsOwnAndItsFamilysSlot(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// holderOf picks the slot's holder from the placer's family.
		holderOf func(parent, placer, sibling int64) int64
	}{
		{"its own", func(_, placer, _ int64) int64 { return placer }},
		{"its sibling's", func(_, _, sibling int64) int64 { return sibling }},
		{"its parent's", func(parent, _, _ int64) int64 { return parent }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newSlotWorld(t, "SCOPE-FAMILY")
			// A dig's parent: reshuffling, so its lane hold is a hold in flight.
			parent := testdb.CreateOrder(t, w.db, func(o *orders.Order) {
				o.EdgeUUID = "scope-parent"
				o.Status = protocol.StatusReshuffling
			})
			leg := func(uuid string) int64 {
				return testdb.CreateOrder(t, w.db, func(o *orders.Order) { o.EdgeUUID = uuid; o.ParentOrderID = &parent.ID }).ID
			}
			placer, sibling := leg("scope-leg-1"), leg("scope-leg-2")
			w.holdSlot(t, c.holderOf(parent.ID, placer, sibling))
			// The family's rows elsewhere: a lane hold of the parent's and
			// another slot of the sibling's. The arrival frees this slot only.
			elsewhere := &nodes.Node{Name: "SCOPE-FAMILY-OTHER", Enabled: true}
			testutil.MustNoErr(t, w.db.CreateNode(elsewhere), "another node")
			_, err := w.db.DB.Exec(`INSERT INTO reservations (order_id, resource_kind, node_id, state, reserved_by)
				VALUES ($1, 'mouth', $3, 'confirmed', 'test'), ($2, 'slot', $3, 'pending', 'test')`,
				parent.ID, sibling, elsewhere.ID)
			testutil.MustNoErr(t, err, "the family's rows elsewhere")

			w.place(t, placer)

			if by, rows := w.holder(t); by != nil || rows != 0 {
				t.Errorf("after the arrival the slot is claimed by %v with %d reservation row(s); want it free", by, rows)
			}
			var kept int
			testutil.MustNoErr(t, w.db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE node_id=$1`, elsewhere.ID).Scan(&kept), "kept")
			if kept != 2 {
				t.Errorf("the family's rows on another node: %d left, want 2: an arrival frees its own slot only", kept)
			}
		})
	}
}
