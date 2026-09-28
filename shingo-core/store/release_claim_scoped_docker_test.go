//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// release_claim_scoped_docker_test.go — ReleaseClaimForBin releases THIS
// order's hold and nobody else's.
//
// ReleaseClaimForBin is the coupled rollback for one ClaimForDispatch: clear the
// bin's claim if this order holds it, and drop the reservation that tracks it.
// It has three callers, and they split in two:
//
//   - dispatch/allocator.go (reconcile a stray confirmed bin) — the order still
//     HOLDS the claim and its confirmed reservation. The call must release both.
//   - engine/bin_move.go and engine/carried_bin_recovery.go — called AFTER
//     failOrderAndEmit, whose terminalize has already released the order's claim
//     and reservation. The call is a belt; it must find nothing of its own and
//     touch nothing.
//
// The second case is where the bin-keyed delete went wrong: in the window
// between the terminalize and the belt, the bin is free, and another order may
// reserve it. A delete keyed on the bin alone takes that order's reservation
// with it.

// reservationsFor counts the bin reservations order holds on binID.
func reservationsFor(t *testing.T, db *store.DB, binID, orderID int64) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(
		`SELECT COUNT(*) FROM reservations WHERE bin_id=$1 AND order_id=$2`, binID, orderID).Scan(&n); err != nil {
		t.Fatalf("count reservations bin=%d order=%d: %v", binID, orderID, err)
	}
	return n
}

// TestReleaseClaimForBin_SparesAnotherOrdersReservation is the flat bug: order
// A's holds are already gone, order B has since reserved and claimed the bin,
// and A's belt call arrives. B's claim and reservation must both survive.
func TestReleaseClaimForBin_SparesAnotherOrdersReservation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RCB-SCOPED")
	inTransit := func(o *orders.Order) { o.Status = "in_transit" }

	a := testdb.CreateOrder(t, db, inTransit)
	testdb.ClaimBinForTest(t, db, bin.ID, a.ID)
	// A's holds are released (the terminalize's work, stood in for here).
	testutil.MustNoErr(t, db.ReleaseClaimForBin(bin.ID, a.ID), "release A's holds")

	b := testdb.CreateOrder(t, db, inTransit)
	testdb.ClaimBinForTest(t, db, bin.ID, b.ID)

	// A's belt call.
	testutil.MustNoErr(t, db.ReleaseClaimForBin(bin.ID, a.ID), "A's belt call")

	got := testdb.RequireBin(t, db, bin.ID)
	if got.ClaimedBy == nil || *got.ClaimedBy != b.ID {
		t.Errorf("claimed_by = %v, want order B (%d): A's call cleared a claim it does not hold", got.ClaimedBy, b.ID)
	}
	if n := reservationsFor(t, db, bin.ID, b.ID); n != 1 {
		t.Errorf("order B's reservations on bin %d = %d, want 1: order A's release deleted "+
			"order B's reservation — the delete must be scoped to the claim it actually released", bin.ID, n)
	}
}

// TestReleaseClaimForBin_OwnerReleasesItsOwn is the allocator's case
// (dispatch/allocator.go, the stray confirmed bin-claim arm): the order holds the
// claim and its confirmed reservation, and the call releases both, leaving the
// bin re-acquirable.
func TestReleaseClaimForBin_OwnerReleasesItsOwn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RCB-OWNER")

	owner := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, bin.ID, owner.ID)

	testutil.MustNoErr(t, db.ReleaseClaimForBin(bin.ID, owner.ID), "owner releases")

	got := testdb.RequireBin(t, db, bin.ID)
	if got.ClaimedBy != nil {
		t.Errorf("claimed_by = %v, want nil after the owner's release", *got.ClaimedBy)
	}
	if n := reservationsFor(t, db, bin.ID, owner.ID); n != 0 {
		t.Errorf("owner's reservations on bin %d = %d, want 0: the owner's release must still "+
			"release the reservation it holds", bin.ID, n)
	}
	// Re-acquirable: the unique active-reservation index would refuse a
	// leaked row.
	next := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	testdb.ClaimBinForTest(t, db, bin.ID, next.ID)
}
