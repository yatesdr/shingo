//go:build docker

package uop_test

import (
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/uop"
)

// redelivery_test.go — a redelivered count message is judged a duplicate
// BEFORE the stale-epoch guard sees it (lead ruling, lane C's redelivery table).
//
// A capture_reduction that takes a bin to <= 0 clears it, and the clear bumps
// the bin's generation. The same message delivered again carries the old
// generation, so a stale-epoch check that runs first records it as a stale
// drop: a permanent exception, a ledger row, the anomaly flag and an epoch
// refresh, for a message whose count already landed. Once Core commits after
// handling (lane C, S2), a redelivery after a crash is expected, so this would
// be routine.

func staleRows(t *testing.T, db *store.DB, binID int64) (ledger, exceptions int) {
	t.Helper()
	testutil.MustNoErr(t, db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op='stale_epoch_dropped'),
		(SELECT COUNT(*) FROM bin_uop_exception WHERE bin_id=$1 AND kind='stale_epoch')`, binID).
		Scan(&ledger, &exceptions), "count stale rows")
	return ledger, exceptions
}

// A redelivered capture_reduction that emptied the bin is a silent duplicate:
// no stale exception, no stale ledger row, no second delta row, no anomaly.
func TestRedelivery_CaptureToZeroRedeliveredIsADuplicate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-REDELIVER", "PART-A", 5)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=1 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "set epoch 1")

	d := &protocol.BinUOPDelta{
		BinID: bin.ID, PayloadCode: "PART-A", Delta: -5, Reason: protocol.ReasonCaptureReduction,
		SequenceID: 1, Epoch: 1, WindowStart: time.Now().UTC().Add(-5 * time.Second), WindowEnd: time.Now().UTC(),
	}
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "first delivery")
	if got := binEpoch(t, db, bin.ID); got != 2 {
		t.Fatalf("delta_epoch after the clear = %d, want 2 (the clear bumps)", got)
	}

	err = svc.ApplyBinUOPDelta(testStation, d)
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("redelivery: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if l, e := staleRows(t, db, bin.ID); l != 0 || e != 0 {
		t.Errorf("stale ledger/exception rows = %d/%d, want 0/0 (a redelivery is not a stale drop)", l, e)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != 1 {
		t.Errorf("delta ledger rows = %d, want 1", got)
	}
	var anomaly bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT anomaly_at IS NOT NULL FROM bins WHERE id=$1`, bin.ID).Scan(&anomaly), "read anomaly")
	if anomaly {
		t.Error("anomaly_at set by a redelivery")
	}
}

// A delta on a retired generation that was never applied is still a stale drop,
// exactly as before: the duplicate check looks at the delta's OWN epoch, and
// there is nothing there.
func TestRedelivery_NeverAppliedOldEpochIsStillStaleDropped(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-STALE-NEW", "PART-A", 50)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "set epoch 2")

	err = svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -3, 1, 1, time.Now().UTC()))
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("old-epoch delta: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if l, e := staleRows(t, db, bin.ID); l != 1 || e != 1 {
		t.Errorf("stale ledger/exception rows = %d/%d, want 1/1", l, e)
	}
	if got := binUOP(t, db, bin.ID); got != 50 {
		t.Errorf("uop_remaining = %d, want 50", got)
	}
}
