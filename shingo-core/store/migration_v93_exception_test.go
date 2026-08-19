//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/bins"
)

// The v93 exceptions ledger: one migration test that proves the backfill
// derives the same rows the readers used to derive from raw bin_uop_audit,
// plus the op-set pin that keeps the backfill's copy of EpochBumpOps honest.

// seedV93Audit writes one bin_uop_audit row directly.
func seedV93Audit(t *testing.T, db *store.DB, binID int64, before, after int, op, payload string, at time.Time) {
	t.Helper()
	var b any
	if before >= 0 || before < 0 {
		b = before
	}
	if _, err := db.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,$4,'test',$5,'test','{"reason":"consume_tick"}',$6)`,
		binID, b, after, op, payload, at); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
}

// TestV93_BackfillDerivesCrossingsDropsAndBoundaries seeds the four shapes the
// backfill must derive, re-opens through the migrate path (the self-heal
// re-runs nothing here — v93 is already recorded on the template clone — so
// this exercises the FIRST-APPLY path the way the V72 test does: seed, drop
// the version row, re-open).
//
// The seeded history, one bin:
//
//	set_for_production  (boundary 1)   0 → 300
//	bin_uop_delta                      300 → 120
//	bin_uop_delta        CROSSING      120 → -7
//	bin_uop_delta        continuation   -7 → -30
//	clear_for_reuse      (boundary 2)  -30 → 0      ← recovery + boundary
//
// and a second bin carrying one of each drop kind.
func TestV93_BackfillDerivesCrossingsDropsAndBoundaries(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)

	const bin1, bin2 = 9001, 9002
	seedV93Audit(t, db, bin1, 0, 300, "set_for_production", "PART-A", base)
	seedV93Audit(t, db, bin1, 300, 120, "bin_uop_delta", "PART-A", base.Add(time.Minute))
	seedV93Audit(t, db, bin1, 120, -7, "bin_uop_delta", "PART-A", base.Add(2*time.Minute))
	seedV93Audit(t, db, bin1, -7, -30, "bin_uop_delta", "PART-A", base.Add(3*time.Minute))
	seedV93Audit(t, db, bin1, -30, 0, "clear_for_reuse", "PART-A", base.Add(4*time.Minute))

	seedV93Audit(t, db, bin2, 50, 50, "stale_epoch_dropped", "PART-B", base.Add(5*time.Minute))
	seedV93Audit(t, db, bin2, 50, 50, "payload_mismatch_dropped", "PART-B", base.Add(6*time.Minute))

	// Force the first-apply path: the template already recorded v93, so drop
	// the row AND the table's contents to re-run it against seeded history.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 93`); err != nil {
		t.Fatalf("clear v93 row: %v", err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v93: %v", err)
	}
	defer migrated.Close()

	// Crossings: exactly one (the continuation is folded, not listed).
	var nCross, deepest, recoveredNull int
	rows, err := migrated.Query(`SELECT deepest_uop, recovered_at IS NULL FROM bin_uop_exception
		WHERE kind = 'negative_crossing'`)
	if err != nil {
		t.Fatalf("read crossings: %v", err)
	}
	for rows.Next() {
		nCross++
		var recNull bool
		if err := rows.Scan(&deepest, &recNull); err != nil {
			t.Fatalf("scan crossing: %v", err)
		}
		if recNull {
			recoveredNull++
		}
	}
	rows.Close()
	if nCross != 1 || recoveredNull != 0 {
		t.Fatalf("crossings = %d (open=%d), want exactly 1 recovered crossing", nCross, recoveredNull)
	}
	if deepest != -30 {
		t.Errorf("deepest = %d, want -30 (continuation folded)", deepest)
	}

	// Drops: one of each kind, detail carrying the metadata blob.
	var nStale, nMismatch int
	if err := migrated.QueryRow(`SELECT count(*), count(*) FILTER (WHERE kind='payload_mismatch')
		FROM bin_uop_exception WHERE kind IN ('stale_epoch','payload_mismatch')`).
		Scan(&nStale, &nMismatch); err != nil {
		t.Fatalf("read drops: %v", err)
	}
	if nStale != 2 || nMismatch != 1 {
		t.Fatalf("drop rows = %d (mismatch=%d), want 2 total / 1 mismatch", nStale, nMismatch)
	}

	// Boundaries: two for bin1 (set_for_production, clear_for_reuse), zero
	// for bin2. epoch_seq must be the bump ordinal — 1 and 2 on bin1.
	var nBound int
	if err := migrated.QueryRow(`SELECT count(*) FROM bin_uop_exception WHERE kind='boundary'`).
		Scan(&nBound); err != nil {
		t.Fatalf("read boundaries: %v", err)
	}
	if nBound != 2 {
		t.Fatalf("boundary rows = %d, want 2", nBound)
	}
	var seq1, seqCross int64
	if err := migrated.QueryRow(`SELECT epoch_seq FROM bin_uop_exception
		WHERE kind='boundary' AND op='set_for_production'`).Scan(&seq1); err != nil {
		t.Fatalf("read boundary 1 seq: %v", err)
	}
	if err := migrated.QueryRow(`SELECT epoch_seq FROM bin_uop_exception
		WHERE kind='negative_crossing'`).Scan(&seqCross); err != nil {
		t.Fatalf("read crossing seq: %v", err)
	}
	if seq1 != 1 || seqCross != 1 {
		t.Errorf("epoch_seq: boundary1=%d crossing=%d, want 1 and 1 (both inside the first binding)", seq1, seqCross)
	}
}

// TestV93_IdempotentOnReapply: the self-heal can re-run v93 (its verify fails
// only if the table vanished), so the backfill must not duplicate on re-run.
// The three INSERTs have no IF NOT EXISTS to lean on — the guard is that
// re-running recreates the table contents from the same audit rows: v93
// truncates before the backfills.
func TestV93_IdempotentOnReapply(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seedV93Audit(t, db, 9100, 5, -2, "bin_uop_delta", "PART-I", base)

	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 93`); err != nil {
		t.Fatalf("clear v93 row: %v", err)
	}
	for i := 0; i < 2; i++ {
		migrated, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
		migrated.Close()
		// Drop the row again so the second open genuinely re-runs v93 rather
		// than no-op'ing on the row the first apply just recorded.
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 93`); err != nil {
			t.Fatalf("re-clear v93 row: %v", err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM bin_uop_exception WHERE bin_id = 9100`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("after two applies, bin 9100 has %d exception rows, want 1 (no duplicates)", n)
	}
}

// TestV93BackfillOpsMatchEpochBumpOps pins the migration's spelled-out copy of
// the bump-op set to audit.EpochBumpOps. The audit-side set is pinned to the
// bump call sites by TestEpochBumpOpsCoversEveryBumpSite; this closes the
// other direction, so a sixth bump op arrives as failing tests rather than as
// a silently short backfill (and, post-retention, a permanently short ledger).
func TestV93BackfillOpsMatchEpochBumpOps(t *testing.T) {
	want := map[string]bool{}
	for _, op := range audit.EpochBumpOps {
		want[op] = true
	}
	got := map[string]bool{}
	for _, op := range store.BumpOpsForBackfill {
		got[op] = true
	}
	for op := range want {
		if !got[op] {
			t.Errorf("bump op %q is in audit.EpochBumpOps but missing from the v93 backfill set — its historical boundaries will not be backfilled", op)
		}
	}
	for op := range got {
		if !want[op] {
			t.Errorf("bump op %q is in the v93 backfill set but not in audit.EpochBumpOps — one of the two is wrong", op)
		}
	}
}

// TestV93_OpenNegativeBinsReadsTheExceptionTable proves the repointed reader:
// a crossing in bin_uop_exception dates the bin even when the raw audit row is
// GONE — the entire point of the table (the raw row is what the 90-day purge
// deletes).
func TestV93_OpenNegativeBinsReadsTheExceptionTable(t *testing.T) {
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	b := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "EXC-1"}
	if err := bins.Create(db.DB, b); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	_, err := db.Exec(`UPDATE bins SET uop_remaining=-13, payload_code='PART-E' WHERE id=$1`, b.ID)
	if err != nil {
		t.Fatalf("drive negative: %v", err)
	}

	crossed := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	// An old raw audit row that the purge WILL delete...
	if _, err := db.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, applied_at)
		VALUES ($1, 4, -13, 'bin_uop_delta', 'test', 'PART-E', 'test', $2)`, b.ID, crossed); err != nil {
		t.Fatalf("seed raw crossing: %v", err)
	}
	// ...and the exception row, which outlives it.
	if err := audit.AppendBinUOPException(db.DB, audit.ExcNegativeCrossing, b.ID,
		"PART-E", "test", nil, crossed, intPtr(4), intPtr(-13), intPtr(-13), nil,
		"bin_uop_delta", nil); err != nil {
		t.Fatalf("seed exception: %v", err)
	}

	// The purge happens: the raw row goes, the exception row stays.
	if _, err := db.Exec(`DELETE FROM bin_uop_audit WHERE bin_id = $1`, b.ID); err != nil {
		t.Fatalf("purge raw: %v", err)
	}

	open, err := bins.OpenNegativeBins(db.DB)
	if err != nil {
		t.Fatalf("OpenNegativeBins: %v", err)
	}
	var found bool
	for _, o := range open {
		if o.BinID == b.ID {
			found = true
			if o.NegativeSince == nil || !o.NegativeSince.Equal(crossed) {
				t.Errorf("NegativeSince = %v, want the exception row's occurred_at %v", o.NegativeSince, crossed)
			}
		}
	}
	if !found {
		t.Fatal("the negative bin must still be listed after its raw audit rows are purged")
	}
}

func intPtr(i int) *int { return &i }
