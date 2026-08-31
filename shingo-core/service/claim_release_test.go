//go:build docker

// claim_release_test.go — the coupled release primitive (commit 1).
//
// ClaimForDispatch (Acquire -> claim -> Confirm) leaves a CONFIRMED reservation
// on success. Its inverse must release that reservation too, or a
// dispatch-failure rollback that only clears claimed_by orphans the confirmed
// row and bricks the bin via uq_reservations_bin_active. This test asserts
// RE-ACQUIRABILITY after rollback — not just claimed_by IS NULL — because a
// claimed_by-only rollback passes the latter while the bin stays bricked.
//
// The by-order twin went with ReleaseClaimByOrder, which lost its last caller to
// the fleet-refusal demote door. The order-grain release set is TerminalizeOrder's
// now, and it is pinned where it lives.

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/reservations"
)

func TestReleaseClaim_ClearsClaimAndReservation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db, EpochAnnounce{})

	reAcquirable := func(t *testing.T, binID int64) {
		t.Helper()
		probe := testdb.CreateOrder(t, db)
		if err := reservations.Acquire(db, probe.ID, probe.ID, binID, "test"); err != nil {
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

}
