//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// TestBinManifestService_Unconfirm_WritesItsLedgerRow pins I5: Unconfirm leaves
// a manifest_unconfirmed row the way Confirm leaves manifest_confirmed, with the
// bin's unchanged count on both sides, in the same transaction as the flag.
func TestBinManifestService_Unconfirm_WritesItsLedgerRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db, EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-UNCONF-1", "", 0)
	testutil.MustNoErr(t, svc.RecordProducedBin(bin.ID, `{"items":[]}`, sd.Payload.Code, 40, ""), "load")
	testutil.MustNoErr(t, svc.Unconfirm(bin.ID), "Unconfirm")

	got, _ := db.GetBin(bin.ID)
	if got.ManifestConfirmed {
		t.Fatal("ManifestConfirmed = true after Unconfirm")
	}
	var n, before, after int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*), MAX(before_uop), MAX(after_uop) FROM bin_uop_ledger
		WHERE bin_id=$1 AND op=$2`, bin.ID, audit.OpManifestUnconfirmed).Scan(&n, &before, &after), "ledger")
	if n != 1 || before != 40 || after != 40 {
		t.Errorf("manifest_unconfirmed rows=%d before=%d after=%d, want 1 row at 40->40", n, before, after)
	}
}
