//go:build docker

package heartbeat_test

import (
	"strings"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/heartbeat"
)

// insertDedupAt writes one production_tick_dedup row with an explicit
// applied_at. TryDedup takes the column default (NOW()), which is no use
// for testing a retention cutoff.
func insertDedupAt(t *testing.T, db *store.DB, station string, id int64, appliedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO production_tick_dedup (station, edge_snapshot_id, applied_at) VALUES ($1, $2, $3)`,
		station, id, appliedAt); err != nil {
		t.Fatalf("insert dedup %s/%d: %v", station, id, err)
	}
}

func dedupRowExists(t *testing.T, db *store.DB, station string, id int64) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM production_tick_dedup WHERE station = $1 AND edge_snapshot_id = $2`,
		station, id).Scan(&n); err != nil {
		t.Fatalf("count dedup %s/%d: %v", station, id, err)
	}
	return n > 0
}

// TestCoverage_ProductionTickDedupRetention pins the retention this table
// never had: rows outside the window go, rows inside it stay, and — the
// part that matters — the dedup guarantee still holds afterwards for the
// rows that remain.
//
// Verified red: with PurgeOldDedup's WHERE clause removed (`DELETE FROM
// production_tick_dedup`), this fails with "purged 2 rows, want 1",
// "recent dedup row was purged" and "TryDedup treated a replay as new
// after the purge" — the double-projection this table exists to prevent.
func TestCoverage_ProductionTickDedupRetention(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC()

	insertDedupAt(t, db, "STN-RET", 1, now.AddDate(0, 0, -120))
	insertDedupAt(t, db, "STN-RET", 2, now.AddDate(0, 0, -3))

	purged, err := heartbeat.PurgeOldDedup(db.DB, 90, now)
	if err != nil {
		t.Fatalf("PurgeOldDedup: %v", err)
	}
	if purged != 1 {
		t.Errorf("purged %d rows, want 1", purged)
	}
	if dedupRowExists(t, db, "STN-RET", 1) {
		t.Error("dedup row 120 days old survived the purge")
	}
	if !dedupRowExists(t, db, "STN-RET", 2) {
		t.Error("recent dedup row was purged")
	}

	// The guarantee still holds for what remains: replaying a row inside the
	// window is still recognised as a duplicate.
	if isNew, err := heartbeat.TryDedup(db.DB, "STN-RET", 2); err != nil || isNew {
		t.Errorf("TryDedup treated a replay as new after the purge: isNew=%v err=%v", isNew, err)
	}
	// A tick whose dedup record has aged out is accepted again. That is the
	// deliberate cost of any retention on a dedup table, and it is harmless
	// here because the redelivery being guarded against is bounded by the
	// outbox's own 24-hour window, three orders of magnitude inside 90 days.
	if isNew, err := heartbeat.TryDedup(db.DB, "STN-RET", 1); err != nil || !isNew {
		t.Errorf("TryDedup after expiry: isNew=%v err=%v, want true/nil", isNew, err)
	}
}

// TestProductionTickDedup_CannotBePartitionedByAppliedAt is the evidence
// for why PurgeOldDedup is a DELETE rather than a partition drop like its
// sibling cell_part_events.
//
// "Reuse the existing partition manager" is the obvious move — same event,
// same function, identical row count — and it is not available: Postgres
// requires every partition-key column to appear in any unique constraint
// on a partitioned table, and this table's entire purpose is a composite
// PK that does not include time. This asserts the database says so rather
// than trusting the manual.
//
// Verified red by construction: it fails if either CREATE/INSERT succeeds,
// which is precisely the world in which partitioning would have been the
// right answer.
func TestProductionTickDedup_CannotBePartitionedByAppliedAt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	_, err := db.Exec(`CREATE TABLE production_tick_dedup_partitioned (
		station          TEXT NOT NULL,
		edge_snapshot_id BIGINT NOT NULL,
		applied_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (station, edge_snapshot_id)
	) PARTITION BY RANGE (applied_at)`)
	if err == nil {
		t.Fatal("Postgres accepted a table partitioned by applied_at with a PK excluding it — " +
			"the partition-drop approach would then be available and PurgeOldDedup should be revisited")
	}
	if !strings.Contains(err.Error(), "must include all partitioning columns") {
		t.Errorf("rejected for an unexpected reason: %v", err)
	}

	// The only way to satisfy that rule is to widen the PK with applied_at —
	// and that is what destroys the guard: TryDedup's ON CONFLICT target no
	// longer names a unique index, so the statement will not even plan.
	if _, err := db.Exec(`CREATE TABLE production_tick_dedup_widened (
		station          TEXT NOT NULL,
		edge_snapshot_id BIGINT NOT NULL,
		applied_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (station, edge_snapshot_id, applied_at)
	)`); err != nil {
		t.Fatalf("create widened table: %v", err)
	}
	_, err = db.Exec(`INSERT INTO production_tick_dedup_widened (station, edge_snapshot_id)
		VALUES ($1, $2) ON CONFLICT (station, edge_snapshot_id) DO NOTHING`, "STN-W", int64(7))
	if err == nil {
		t.Fatal("ON CONFLICT (station, edge_snapshot_id) planned against a PK widened with applied_at — " +
			"expected no matching unique index")
	}
	if !strings.Contains(err.Error(), "no unique or exclusion constraint matching the ON CONFLICT specification") {
		t.Errorf("widened-PK insert rejected for an unexpected reason: %v", err)
	}
}
