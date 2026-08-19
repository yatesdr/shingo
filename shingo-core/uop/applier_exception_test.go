//go:build docker

package uop_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/audit"
	"shingocore/uop"
)

// The v93 permanent exceptions ledger, event-time half: every exception the
// applier can produce at apply time must land on bin_uop_exception in the
// same transaction as its raw-stream twin — that table is what survives the
// 90-day retention on bin_uop_audit (D6), so a missing writer here is a
// permanently missing row, not a stale one.

// TestExceptionLedger_CrossingAndRecoveryLifecycle drives one bin through a
// crossing, a continuation, and a recovery, and pins what each stage writes:
// the crossing opens a negative_crossing row; the continuation writes NOTHING
// (same excursion); the recovery closes the row and folds the deepest.
func TestExceptionLedger_CrossingAndRecoveryLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-EXC-CROSS", "PART-X", 10)

	// Crossing: 10 → -4.
	if err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-X", -14, 1, protocol.ReasonConsumeTick)); err != nil {
		t.Fatalf("crossing apply: %v", err)
	}
	var n, before, after int
	var recoveredNull bool
	var deepestNull *int // deepest_uop stays NULL until the recovery folds it
	if err := db.QueryRow(`SELECT count(*), min(before_uop), min(after_uop),
	       bool_and(recovered_at IS NULL)
		FROM bin_uop_exception WHERE bin_id=$1 AND kind='negative_crossing'`,
		bin.ID).Scan(&n, &before, &after, &recoveredNull); err != nil {
		t.Fatalf("read crossing: %v", err)
	}
	if n != 1 || before != 10 || after != -4 {
		t.Fatalf("crossing row = n=%d before=%d after=%d, want 1 row 10→-4", n, before, after)
	}
	if !recoveredNull {
		t.Error("fresh crossing must be open (recovered_at NULL)")
	}
	if err := db.QueryRow(`SELECT deepest_uop FROM bin_uop_exception
		WHERE bin_id=$1 AND kind='negative_crossing'`, bin.ID).Scan(&deepestNull); err != nil {
		t.Fatalf("read deepest: %v", err)
	}
	if deepestNull != nil {
		t.Errorf("open crossing deepest_uop = %d, want NULL (folded only at recovery)", *deepestNull)
	}

	// Continuation: -4 → -20. Same excursion — no new row, deepest still open.
	if err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-X", -16, 2, protocol.ReasonConsumeTick)); err != nil {
		t.Fatalf("continuation apply: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM bin_uop_exception
		WHERE bin_id=$1 AND kind='negative_crossing'`, bin.ID).Scan(&n); err != nil {
		t.Fatalf("count after continuation: %v", err)
	}
	if n != 1 {
		t.Fatalf("continuation rows = %d, want 1 (continuations do not write)", n)
	}

	// Recovery: a produce tick -20 → +5 closes the excursion, deepest folds -20.
	if err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-X", 25, 3, protocol.ReasonProduceTick)); err != nil {
		t.Fatalf("recovery apply: %v", err)
	}
	var deepest int // at recovery this is folded and non-NULL
	if err := db.QueryRow(`SELECT count(*), min(deepest_uop), bool_and(recovered_at IS NOT NULL)
		FROM bin_uop_exception WHERE bin_id=$1 AND kind='negative_crossing'`,
		bin.ID).Scan(&n, &deepest, &recoveredNull); err != nil {
		t.Fatalf("read recovery: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovery rows = %d, want 1 (still one excursion)", n)
	}
	if deepest != -20 {
		t.Errorf("deepest = %d, want -20 (the continuation folded in at recovery)", deepest)
	}
	if !recoveredNull {
		t.Error("recovered excursion must carry recovered_at")
	}
}

// TestExceptionLedger_StaleEpochDropWritesException: the stale-epoch branch
// writes its permanent row on the same transaction as the raw observation.
func TestExceptionLedger_StaleEpochDropWritesException(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-EXC-STALE", "PART-S", 100)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "advance epoch")

	d := makeBinDelta(bin.ID, "PART-S", -7, 1, protocol.ReasonConsumeTick)
	d.Epoch = 1
	if err := svc.ApplyBinUOPDelta(testStation, d); err == nil {
		t.Fatal("stale-epoch delta must not apply cleanly")
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM bin_uop_exception
		WHERE bin_id=$1 AND kind='stale_epoch'`, bin.ID).Scan(&n); err != nil {
		t.Fatalf("read exception: %v", err)
	}
	if n != 1 {
		t.Fatalf("stale_epoch exception rows = %d, want 1", n)
	}
}

// TestExceptionLedger_PayloadMismatchDropWritesException: the reject path
// writes its permanent row best-effort on s.db, beside the raw observation.
func TestExceptionLedger_PayloadMismatchDropWritesException(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-EXC-MM", "PART-REAL", 50)
	if err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-OTHER", -3, 1, protocol.ReasonConsumeTick)); err == nil {
		t.Fatal("mismatched delta must be rejected")
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM bin_uop_exception
		WHERE bin_id=$1 AND kind='payload_mismatch'`, bin.ID).Scan(&n); err != nil {
		t.Fatalf("read exception: %v", err)
	}
	if n != 1 {
		t.Fatalf("payload_mismatch exception rows = %d, want 1", n)
	}
}

// TestExceptionLedger_BoundaryWrittenForEveryBumpOp drives one full load
// cycle (set_for_production → consume → clear_for_reuse) through the manifest
// service and pins that each bump op wrote its boundary row — the AppendBinUOP
// hook, which is what keeps a sixth bump site from silently missing its
// boundary.
func TestExceptionLedger_BoundaryWrittenForEveryBumpOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	mSvc := service.NewBinManifestService(db, service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-EXC-BOUND", "", 0)

	if _, err := mSvc.SetFromTemplate(bin.ID, sd.Payload.Code, nil); err != nil {
		t.Fatalf("set for production: %v", err)
	}
	if _, err := mSvc.ClearForReuse(bin.ID, nil); err != nil {
		t.Fatalf("clear for reuse: %v", err)
	}

	rows, err := db.Query(`SELECT op, epoch_seq FROM bin_uop_exception
		WHERE bin_id=$1 AND kind='boundary' ORDER BY id`, bin.ID)
	if err != nil {
		t.Fatalf("read boundaries: %v", err)
	}
	defer rows.Close()
	var got []struct {
		op  string
		seq int64
	}
	for rows.Next() {
		var r struct {
			op  string
			seq int64
		}
		if err := rows.Scan(&r.op, &r.seq); err != nil {
			t.Fatalf("scan boundary: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("boundary rows = %d, want 2 (set_for_production + clear_for_reuse): %+v", len(got), got)
	}
	if got[0].op != audit.OpSetForProduction || got[0].seq != 1 {
		t.Errorf("boundary 1 = %s seq %d, want set_for_production seq 1", got[0].op, got[0].seq)
	}
	if got[1].op != audit.OpClearForReuse || got[1].seq != 2 {
		t.Errorf("boundary 2 = %s seq %d, want clear_for_reuse seq 2", got[1].op, got[1].seq)
	}
}
