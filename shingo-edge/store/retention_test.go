package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"shingoedge/store/counters"
)

// insertSnapshotAt writes one counter_snapshots row with an explicit
// recorded_at. InsertCounterSnapshot takes the column default
// (datetime('now')), which is no use for testing a retention cutoff.
// anomaly is written as SQL NULL when empty, matching InsertSnapshot.
func insertSnapshotAt(t *testing.T, db *DB, rpID int64, recordedAt time.Time, anomaly string, confirmed bool) int64 {
	t.Helper()
	var anomalyPtr *string
	if anomaly != "" {
		anomalyPtr = &anomaly
	}
	res, err := db.Exec(
		`INSERT INTO counter_snapshots (reporting_point_id, count_value, delta, anomaly, operator_confirmed, recorded_at)
		 VALUES (?, 1, 1, ?, ?, ?)`,
		rpID, anomalyPtr, confirmed, recordedAt.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatalf("insert snapshot at %s: %v", recordedAt, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func snapshotIDs(t *testing.T, db *DB) map[int64]bool {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM counter_snapshots ORDER BY id`)
	if err != nil {
		t.Fatalf("list ids: %v", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		out[id] = true
	}
	return out
}

// TestPurgeOldCounterSnapshots_KeepsWindowAndUnconfirmedJumps is the main
// retention contract: everything older than the window goes, everything
// inside it stays, and an unconfirmed jump survives at any age because it
// is the operator's popover.
//
// Verified red twice, once against each clause of the predicate:
//   - `WHERE recorded_at < ?` widened to `(recorded_at < ? OR 1=1)` →
//     "purged 4 rows, want 3" / "recent normal row was purged".
//   - the NOT(...) term replaced by `1=1` → "purged 4 rows, want 3" /
//     "ancient unconfirmed jump was purged — that row is the operator's
//     popover".
func TestPurgeOldCounterSnapshots_KeepsWindowAndUnconfirmedJumps(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	rpID, err := db.CreateReportingPoint("PLC", "TAG", sid)
	if err != nil {
		t.Fatalf("create rp: %v", err)
	}

	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	oldNormal := insertSnapshotAt(t, db, rpID, old, "", true)
	oldReset := insertSnapshotAt(t, db, rpID, old, "reset", true)
	oldConfirmedJump := insertSnapshotAt(t, db, rpID, old, "jump", true)
	oldOpenJump := insertSnapshotAt(t, db, rpID, old, "jump", false)
	recentNormal := insertSnapshotAt(t, db, rpID, recent, "", true)
	recentOpenJump := insertSnapshotAt(t, db, rpID, recent, "jump", false)

	n, err := counters.PurgeOldSnapshots(db.DB, counters.SnapshotRetention)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 3 {
		t.Errorf("purged %d rows, want 3", n)
	}

	left := snapshotIDs(t, db)
	if len(left) != 3 {
		t.Errorf("kept %d rows, want 3", len(left))
	}
	if left[oldNormal] {
		t.Error("old normal row survived the purge")
	}
	if left[oldReset] {
		t.Error("old reset row survived the purge")
	}
	if left[oldConfirmedJump] {
		t.Error("old CONFIRMED jump survived the purge — confirmation is what releases it, nothing reads it after")
	}
	if !left[oldOpenJump] {
		t.Error("ancient unconfirmed jump was purged — that row is the operator's popover")
	}
	if !left[recentNormal] {
		t.Error("recent normal row was purged")
	}
	if !left[recentOpenJump] {
		t.Error("recent unconfirmed jump was purged")
	}
}

// TestPurgeOldCounterSnapshots_DeletesNullAnomalyUnconfirmed pins the
// three-valued-logic hole in the predicate as originally proposed.
//
// `NOT (anomaly = 'jump' AND operator_confirmed = 0)` evaluates to NULL —
// not TRUE — for a row with anomaly NULL and operator_confirmed = 0, so
// that row fails the WHERE and is retained forever with no way to tell
// from the outside. The column's schema DEFAULT is 0 and only
// plc/manager.go's `confirmed := anomaly != "jump"` keeps such rows off
// today's plants.
//
// Verified red: unwrapping the COALESCE so the predicate reads a bare
// anomaly column — the predicate exactly as originally proposed — makes
// this fail with "purged 0 rows, want 1" and "an old anomaly-NULL,
// unconfirmed row survived".
func TestPurgeOldCounterSnapshots_DeletesNullAnomalyUnconfirmed(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	rpID, err := db.CreateReportingPoint("PLC", "TAG", sid)
	if err != nil {
		t.Fatalf("create rp: %v", err)
	}

	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	id := insertSnapshotAt(t, db, rpID, old, "", false)

	n, err := counters.PurgeOldSnapshots(db.DB, counters.SnapshotRetention)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d rows, want 1", n)
	}
	if snapshotIDs(t, db)[id] {
		t.Error("an old anomaly-NULL, unconfirmed row survived — SQLite's NULL AND TRUE is NULL, not FALSE")
	}
}

// TestVacuumIfFragmented_RebuildsOnlyWhenFreeSpaceEarnsIt exercises both
// arms of the threshold on a real file, and pins the one property that
// makes the whole purge worth running on a Pi: with auto_vacuum NONE, a
// DELETE alone does not give the disk back.
//
// Verified red: making VacuumIfFragmented return (false, nil)
// unconditionally fails with "vacuum did not run"; dropping the threshold
// check so it always vacuums fails with "vacuum ran on an unfragmented
// file".
func TestVacuumIfFragmented_RebuildsOnlyWhenFreeSpaceEarnsIt(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "vac.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// auto_vacuum must be NONE for any of this to be the real behaviour.
	var autoVacuum int
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	if autoVacuum != 0 {
		t.Fatalf("auto_vacuum = %d, want 0 (NONE) — the purge would not need a VACUUM at all", autoVacuum)
	}

	// A freshly migrated database is not fragmented.
	if ran, err := db.VacuumIfFragmented(VacuumFreeFraction); err != nil || ran {
		t.Fatalf("vacuum ran on an unfragmented file: ran=%v err=%v", ran, err)
	}

	_, sid := seedProcessStyle(t, db, "P", "S")
	rpID, err := db.CreateReportingPoint("PLC", "TAG", sid)
	if err != nil {
		t.Fatalf("create rp: %v", err)
	}
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < 20000; i++ {
		if _, err := tx.Exec(
			`INSERT INTO counter_snapshots (reporting_point_id, count_value, delta, anomaly, operator_confirmed, recorded_at)
			 VALUES (?, ?, 1, NULL, 1, ?)`, rpID, i, old); err != nil {
			tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := db.CheckpointWAL(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	grown := fileSize(t, dbPath)

	if _, err := counters.PurgeOldSnapshots(db.DB, counters.SnapshotRetention); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := db.CheckpointWAL(); err != nil {
		t.Fatalf("checkpoint after purge: %v", err)
	}

	// The delete alone leaves the file the size it grew to.
	if afterDelete := fileSize(t, dbPath); afterDelete < grown {
		t.Errorf("file shrank from %d to %d on DELETE alone — auto_vacuum is not NONE after all", grown, afterDelete)
	}

	ran, err := db.VacuumIfFragmented(VacuumFreeFraction)
	if err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if !ran {
		t.Fatalf("vacuum did not run after purging %d rows", 20000)
	}
	if afterVacuum := fileSize(t, dbPath); afterVacuum >= grown {
		t.Errorf("file is %d bytes after VACUUM, was %d before the purge", afterVacuum, grown)
	}

	// And it stops firing: the rebuilt file has no freelist to reclaim.
	if ran, err := db.VacuumIfFragmented(VacuumFreeFraction); err != nil || ran {
		t.Errorf("vacuum ran a second time: ran=%v err=%v — the threshold is meant to be self-limiting", ran, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}
