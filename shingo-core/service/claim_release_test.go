//go:build docker

// claim_release_test.go — the coupled release primitive (commit 1).
//
// ClaimForDispatch (Acquire -> claim -> Confirm) leaves a CONFIRMED reservation
// on success. Its inverse must release that reservation too, or a
// dispatch-failure rollback that only clears claimed_by orphans the confirmed
// row and bricks the bin via uq_reservations_bin_active. These tests assert
// RE-ACQUIRABILITY after rollback — not just claimed_by IS NULL — because a
// claimed_by-only rollback passes the latter while the bin stays bricked.

package service

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingo/shared/clock"
	"shingocore/internal/testdb"
	"shingocore/store/reservations"
)

func TestReleaseClaim_ClearsClaimAndReservation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	reAcquirable := func(t *testing.T, binID int64) {
		t.Helper()
		probe := testdb.CreateOrder(t, db)
		if err := reservations.Acquire(db, probe.ID, binID, "test", "reacquire", clock.Now().Add(time.Minute)); err != nil {
			t.Errorf("bin %d not re-acquirable after release: %v (confirmed reservation row leaked?)", binID, err)
		}
	}

	t.Run("ForBin", func(t *testing.T) {
		bin := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RC-FORBIN")
		order := testdb.CreateOrder(t, db)
		testutil.MustNoErr(t, svc.ClaimForDispatch(bin.ID, order.ID, nil), "ClaimForDispatch")

		testutil.MustNoErr(t, db.ReleaseClaimForBin(bin.ID, order.ID), "ReleaseClaimForBin")

		got, _ := db.GetBin(bin.ID)
		if got.ClaimedBy != nil {
			t.Errorf("claimed_by = %v, want nil after ReleaseClaimForBin", got.ClaimedBy)
		}
		reAcquirable(t, bin.ID)
	})

	t.Run("ByOrder_multiBin", func(t *testing.T) {
		bin1 := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RC-BYORD-1")
		bin2 := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "BIN-RC-BYORD-2")
		order := testdb.CreateOrder(t, db)
		testutil.MustNoErr(t, svc.ClaimForDispatch(bin1.ID, order.ID, nil), "ClaimForDispatch bin1")
		testutil.MustNoErr(t, svc.ClaimForDispatch(bin2.ID, order.ID, nil), "ClaimForDispatch bin2")

		testutil.MustNoErr(t, db.ReleaseClaimByOrder(order.ID), "ReleaseClaimByOrder")

		for _, b := range []int64{bin1.ID, bin2.ID} {
			got, _ := db.GetBin(b)
			if got.ClaimedBy != nil {
				t.Errorf("bin %d claimed_by = %v, want nil after ReleaseClaimByOrder", b, got.ClaimedBy)
			}
			reAcquirable(t, b)
		}
	})
}
