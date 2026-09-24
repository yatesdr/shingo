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

// TestPurgeOldCounterSnapshots_KeepsTheWindowOnly is the retention contract:
// everything older than the window goes, everything inside it stays. A jump
// is no exception any more — it is counted at the poll (close-out 2b), so an
// unconfirmed one a previous build left behind is a record like any row. It
// used to be kept at any age as the operator's popover.
func TestPurgeOldCounterSnapshots_KeepsTheWindowOnly(t *testing.T) {
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

	oldRows := []int64{
		insertSnapshotAt(t, db, rpID, old, "", true),
		insertSnapshotAt(t, db, rpID, old, "reset", true),
		insertSnapshotAt(t, db, rpID, old, "jump", true),
		insertSnapshotAt(t, db, rpID, old, "jump", false), // left unconfirmed by a previous build
		insertSnapshotAt(t, db, rpID, old, "", false),
	}
	recentRows := []int64{
		insertSnapshotAt(t, db, rpID, recent, "", true),
		insertSnapshotAt(t, db, rpID, recent, "jump", false),
	}

	n, err := counters.PurgeOldSnapshots(db.DB, counters.SnapshotRetention)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != int64(len(oldRows)) {
		t.Errorf("purged %d rows, want %d", n, len(oldRows))
	}
	left := snapshotIDs(t, db)
	for _, id := range oldRows {
		if left[id] {
			t.Errorf("old row %d survived the purge", id)
		}
	}
	for _, id := range recentRows {
		if !left[id] {
			t.Errorf("recent row %d was purged", id)
		}
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
