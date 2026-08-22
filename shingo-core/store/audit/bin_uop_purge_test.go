//go:build docker

package audit_test

import (
	"testing"
	"time"

	"database/sql"

	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// The D6 retention purge: delta rows past the window go; everything else —
// fresh deltas, old NON-delta rows (bump ops feed the epoch derivation, the
// rare observations are forensic) — stays.

func TestPurgeOldBinUOPDelta_DeletesOnlyExpiredDeltaRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC()
	old := now.Add(-91 * 24 * time.Hour)
	fresh := now.Add(-10 * 24 * time.Hour)
	pi := func(n int) *int { return &n }

	// Expired delta: deleted.
	mustAppend(t, db.DB, 1, pi(10), 7, "bin_uop_delta", old)
	// Fresh delta: stays.
	mustAppend(t, db.DB, 1, pi(7), 4, "bin_uop_delta", fresh)
	// Expired NON-delta (bump op): stays forever — the epoch derivation counts it.
	mustAppend(t, db.DB, 1, pi(0), 300, audit.OpSetForProduction, old)
	// Expired NON-delta (rare observation): stays.
	mustAppendOverride(t, db.DB, 1, 50, 50, audit.OpStaleEpochDropped, old)

	n, err := audit.PurgeOldBinUOPDelta(db.DB, 90, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1 (the expired delta only)", n)
	}

	var deltas, bumps, obs, freshDeltas int
	if err := db.QueryRow(`SELECT
		count(*) FILTER (WHERE op='bin_uop_delta' AND applied_at < $1),
		count(*) FILTER (WHERE op = $2),
		count(*) FILTER (WHERE op = $3),
		count(*) FILTER (WHERE op='bin_uop_delta' AND applied_at >= $1)
		FROM bin_uop_ledger WHERE bin_id=1`,
		now.Add(-90*24*time.Hour), audit.OpSetForProduction, audit.OpStaleEpochDropped).
		Scan(&deltas, &bumps, &obs, &freshDeltas); err != nil {
		t.Fatalf("read: %v", err)
	}
	if deltas != 0 {
		t.Errorf("expired deltas remaining = %d, want 0", deltas)
	}
	if freshDeltas != 1 {
		t.Errorf("fresh deltas remaining = %d, want 1", freshDeltas)
	}
	if bumps != 1 || obs != 1 {
		t.Errorf("non-delta rows: bump=%d obs=%d, want 1 and 1 (never purged)", bumps, obs)
	}

	// Idempotent: a second run finds nothing.
	n2, err := audit.PurgeOldBinUOPDelta(db.DB, 90, now)
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second purge deleted %d rows, want 0", n2)
	}
}

// mustAppend writes one bin_uop_ledger row at a controlled time. bin_id is
// planted directly (no FK on bin_uop_ledger.bin_id? it has none — the audit
// stream deliberately keeps rows whose bin is gone).
func mustAppend(t *testing.T, db *sql.DB, binID int64, before *int, after int, op string, at time.Time) {
	t.Helper()
	var b any
	if before != nil {
		b = *before
	}
	if _, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, applied_at)
		VALUES ($1,$2,$3,$4,'test','PART-A','test',$5)`,
		binID, b, after, op, at.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed audit row (%s): %v", op, err)
	}
}

func mustAppendOverride(t *testing.T, db *sql.DB, binID int64, suggested, operator int, op string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,$4,'test','PART-A','test','{"delta":-3}',$5)`,
		binID, suggested, operator, op, at.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed override row (%s): %v", op, err)
	}
}
