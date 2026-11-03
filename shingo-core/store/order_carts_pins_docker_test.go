//go:build docker

package store_test

// order_carts_pins_docker_test.go — U0 of the order-carts stream, the store's
// doors. Each pin says what it asserts at main f8f22f19 (red or green); the
// predictions are in the design folder's PINS.md.

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestOrderCartsPin_APlainHoldWritesItsRow: the plain order's hold
// (AcquireAndPoint, the scanner's reserve) writes the order's cart row in the
// statement that points the order at its cart. RED at main: the hold's CTE
// writes orders.bin_id and no row (reservations.go:466-477).
func TestOrderCartsPin_APlainHoldWritesItsRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CARTS-PLAIN")
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.SourceNode, o.DeliveryNode = "carts-plain", sd.StorageNode.Name, sd.LineNode.Name
	})
	testutil.MustNoErr(t, reservations.AcquireAndPoint(db.DB, o.ID, o.ID, bin.ID, "pin"), "the plain hold")

	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload")
	if after.BinID == nil || *after.BinID != bin.ID {
		t.Fatalf("the hold did not point the order at bin %d", bin.ID)
	}
	if !testdb.HasCartRow(t, db, o.ID, bin.ID) {
		t.Fatalf("the plain hold wrote no cart row for bin %d (rows %v)", bin.ID, testdb.CartRowBins(t, db, o.ID))
	}
}

// TestOrderCartsPin_ATakerUnpointsAJunctionOnlyHolder: a two-cart holder whose
// bin_id names X and whose second cart Y is known only by its row, both promised
// (pending) — the paper a fleet refusal leaves. A person's move takes Y: the
// holder loses its row for Y and is told (bin-taken in its history). RED at
// main: pointsHere asks bin_id (store/orders.go:629-690), so Y's promise is
// deleted, the holder's row for Y stays, and nothing is recorded.
func TestOrderCartsPin_ATakerUnpointsAJunctionOnlyHolder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	x := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CARTS-X")
	y := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.LineNode.ID, "CARTS-Y")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "carts-holder" })
	for _, b := range []int64{x.ID, y.ID} {
		testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, b, "pin"), "the holder's promise")
	}
	testutil.MustNoErr(t, db.UpdateOrderBinID(holder.ID, x.ID), "the holder points at X")
	testdb.SeedCartRow(t, db, holder.ID, x.ID, 0, 1)
	testdb.SeedCartRow(t, db, holder.ID, y.ID, 2, 3)
	taker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OriginClass = "carts-taker", protocol.OriginClassNoDemand
	})

	_, unpointed := take(t, db, y.ID, taker.ID)

	if testdb.HasCartRow(t, db, holder.ID, y.ID) {
		t.Errorf("the holder still has a row for Y (%d) after Y was taken", y.ID)
	}
	if !testdb.HasCartRow(t, db, holder.ID, x.ID) {
		t.Errorf("the holder lost its row for X (%d), which nobody took", x.ID)
	}
	var told int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM order_history WHERE order_id=$1 AND code='bin-taken'`,
		holder.ID).Scan(&told), "the trail")
	if told != 1 || unpointed != 1 {
		t.Errorf("the holder was told %d times and un-pointed %d; want 1 and 1", told, unpointed)
	}
}

// TestOrderCartsPin_ThePairReleaseDeletesItsUnliftedRows: a leg that gives its
// acquisitions back without ending (ReleaseOrderHoldings) gives back its cart
// rows with them. GREEN at main (releaseOrderHoldingsTx deletes the junction),
// and it stays green once the terminal path stops deleting.
func TestOrderCartsPin_ThePairReleaseDeletesItsUnliftedRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CARTS-PAIR")
	o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "carts-pair" })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, o.ID, o.ID, bin.ID, "pin"), "the leg's promise")
	testutil.MustNoErr(t, db.UpdateOrderBinID(o.ID, bin.ID), "the leg points at its cart")
	testdb.SeedCartRow(t, db, o.ID, bin.ID, 0, 1)

	testutil.MustNoErr(t, db.ReleaseOrderHoldings(o.ID), "the pair release")

	if rows := testdb.CartRowBins(t, db, o.ID); len(rows) != 0 {
		t.Fatalf("the released leg still has cart rows %v", rows)
	}
}

// TestOrderCartsPin_TheScannersForgetKeepsANamedOrdersRow: a named order (one
// cart, named by its asker) whose hold was reaped is forgotten by the scanner
// (ForgetHeldCart); its row for the cart it was issued for survives, because
// re-finding it can only ever fetch that cart. GREEN at main (the forget touches
// no row).
func TestOrderCartsPin_TheScannersForgetKeepsANamedOrdersRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CARTS-NAMED")
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.NamedBinID, o.BinID = "carts-named", &bin.ID, &bin.ID
	})
	testdb.SeedCartRow(t, db, o.ID, bin.ID, 0, 1)

	testutil.MustNoErr(t, db.ForgetHeldCart(o.ID), "the scanner's forget")

	if !testdb.HasCartRow(t, db, o.ID, bin.ID) {
		t.Fatalf("the named order lost its row for the cart it names (%d)", bin.ID)
	}
}

// TestOrderCartsPin_TheScannersForgetDeletesAPlainOrdersRow: a plain order's
// pointer is forgotten (its soft hold was reaped) and its row goes with it, in
// the same statement, so the finder picks again from nothing. RED at main (the
// forget clears bin_id and leaves the row).
func TestOrderCartsPin_TheScannersForgetDeletesAPlainOrdersRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "CARTS-FORGET")
	o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID, o.BinID = "carts-forget", &bin.ID })
	testdb.SeedCartRow(t, db, o.ID, bin.ID, 0, 1)

	testutil.MustNoErr(t, db.ForgetHeldCart(o.ID), "the scanner's forget")

	if rows := testdb.CartRowBins(t, db, o.ID); len(rows) != 0 {
		t.Fatalf("the forgotten order still has cart rows %v", rows)
	}
}
