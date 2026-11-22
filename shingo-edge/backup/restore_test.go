package backup

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"shingo/protocol/testutil"
	"strings"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/internal/testdb"
	"shingoedge/store"
)

func TestStageAndLoadRestoreMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "shingoedge.yaml")
	if err := os.WriteFile(configPath, []byte("database_path: test.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive := []byte("archive-bytes")
	testutil.MustNoErr(t, StageRestoreArchive(configPath, "station/backup.tar.gz", bytesReader(archive), "station-1"), "stage restore archive")

	marker, err := PendingRestore(configPath)
	if err != nil {
		t.Fatalf("pending restore: %v", err)
	}
	if marker == nil {
		t.Fatalf("expected restore marker")
	}
	if marker.Key != "station/backup.tar.gz" {
		t.Fatalf("unexpected key %q", marker.Key)
	}
	if marker.StationID != "station-1" {
		t.Fatalf("unexpected station id %q", marker.StationID)
	}
	if marker.StagedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("unexpected staged time %v", marker.StagedAt)
	}
	if _, err := os.Stat(marker.Archive); err != nil {
		t.Fatalf("expected staged archive to exist: %v", err)
	}
}

// TestRestoreArchiveNow_TheRestoredUIDIsTheOneTheEdgeBootsWith pins hardware
// replacement through shingoedge --restore, which hands RestoreArchiveNow the
// station uid the engineer typed (runInteractiveRestore). A fresh box that has
// already booted once, under a uid it minted, restores the old box's archive:
// its config file becomes the archived one, so the next boot loads the old
// box's uid (main's ensureIdentity mints only when the file has none:
// TestEnsureIdentity_AFreshEdgeMintsOnceAndKeepsIt), and the archived database
// lands where that config says. An expected uid other than the archive's (the
// fresh box's own) is refused and changes nothing.
func TestRestoreArchiveNow_TheRestoredUIDIsTheOneTheEdgeBootsWith(t *testing.T) {
	t.Parallel()
	const oldUID, freshUID = "stn-0d0d0d0d0d0d0d0d", "stn-f1f1f1f1f1f1f1f1"
	// The install path both boxes keep their database at.
	dbPath := filepath.ToSlash(filepath.Join(t.TempDir(), "shingoedge.db"))
	writeConfig := func(uid string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "shingoedge.yaml")
		testutil.MustNoErr(t, os.WriteFile(path, []byte("station_uid: "+uid+"\ndatabase_path: "+dbPath+"\n"), 0o644), "write config")
		return path
	}

	oldPath := writeConfig(oldUID)
	oldCfg, err := config.Load(oldPath)
	testutil.MustNoErr(t, err, "load the old box's config")
	archivePath, manifest, _, cleanup, err := createSnapshotArchive(testdb.Open(t), oldCfg, oldPath, "v-pin")
	testutil.MustNoErr(t, err, "back up the old box")
	defer cleanup()
	if manifest.StationID != oldUID {
		t.Fatalf("the old box's archive names station %q, want %q", manifest.StationID, oldUID)
	}
	archive, err := os.ReadFile(archivePath)
	testutil.MustNoErr(t, err, "read the archive")

	freshPath := writeConfig(freshUID)
	before, err := os.ReadFile(freshPath)
	testutil.MustNoErr(t, err, "read the fresh config")
	err = RestoreArchiveNow(freshPath, bytes.NewReader(archive), freshUID)
	if err == nil || !strings.Contains(err.Error(), "does not match expected station") {
		t.Fatalf("restore expecting the fresh box's own uid = %v, want refused: the archive is %s's", err, oldUID)
	}
	after, err := os.ReadFile(freshPath)
	testutil.MustNoErr(t, err, "read the fresh config after the refused restore")
	if !bytes.Equal(after, before) {
		t.Fatal("the refused restore changed the fresh box's config")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("a database is at %s before the restore (err %v): the check below would see it", dbPath, err)
	}

	testutil.MustNoErr(t, RestoreArchiveNow(freshPath, bytes.NewReader(archive), oldUID), "restore the old box's archive")
	oldFile, err := os.ReadFile(oldPath)
	testutil.MustNoErr(t, err, "read the old box's config")
	restored, err := os.ReadFile(freshPath)
	testutil.MustNoErr(t, err, "read the fresh config after the restore")
	if !bytes.Equal(restored, oldFile) {
		t.Errorf("the fresh box's config after the restore is\n%s\nwant the old box's\n%s", restored, oldFile)
	}
	booted, err := config.Load(freshPath)
	testutil.MustNoErr(t, err, "load the restored config")
	if got := booted.StationID(); got != oldUID {
		t.Errorf("after the restore the box boots as %q, want the old box's %q", got, oldUID)
	}
	if db, err := os.ReadFile(dbPath); err != nil || len(db) < 16 || string(db[:15]) != "SQLite format 3" {
		t.Errorf("the restored database at %s: %d bytes, err %v; want the archived SQLite file", dbPath, len(db), err)
	}
}

// shiftArchive backs up a database whose one shift is named BACKUP, after
// plant (nil = none) changed it, as the archive of a box whose config puts the
// database at dbPath. It returns that config's path and the archive.
func shiftArchive(t *testing.T, dbPath string, plant func(*store.DB) error) (configPath string, archive []byte) {
	t.Helper()
	const uid = "stn-0d0d0d0d0d0d0d0d"
	configPath = filepath.Join(t.TempDir(), "shingoedge.yaml")
	testutil.MustNoErr(t, os.WriteFile(configPath,
		[]byte("station_uid: "+uid+"\ndatabase_path: "+filepath.ToSlash(dbPath)+"\n"), 0o644), "write config")
	cfg, err := config.Load(configPath)
	testutil.MustNoErr(t, err, "load config")
	src := testdb.Open(t)
	_, err = src.Exec(`INSERT INTO shifts (name, shift_number, start_time, end_time) VALUES ('BACKUP', 1, '06:00', '14:00')`)
	testutil.MustNoErr(t, err, "the backup's row")
	if plant != nil {
		testutil.MustNoErr(t, plant(src), "plant the backup's shape")
	}
	archivePath, _, _, cleanup, err := createSnapshotArchive(src, cfg, configPath, "v-pin")
	testutil.MustNoErr(t, err, "back up")
	defer cleanup()
	archive, err = os.ReadFile(archivePath)
	testutil.MustNoErr(t, err, "read the archive")
	return configPath, archive
}

// shiftNames reads the shifts of the database at path as a plain SQLite client.
func shiftNames(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	testutil.MustNoErr(t, err, "open")
	defer db.Close()
	var names sql.NullString
	testutil.MustNoErr(t, db.QueryRow(`SELECT group_concat(name) FROM shifts`).Scan(&names), "read the shifts")
	return names.String
}

// TestRestore_WhatItPutsInPlace is which backups replace the live database: one
// of this build's schema, and one from before it.
func TestRestore_WhatItPutsInPlace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		plant string
		want  string
	}{
		{"this build's schema", "", "BACKUP"},
		{"a schema below v59", `DELETE FROM schema_migrations WHERE version > 40`, "BACKUP"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dbPath := filepath.Join(t.TempDir(), "shingoedge.db")
			var plant func(*store.DB) error
			if c.plant != "" {
				plant = func(db *store.DB) error { _, err := db.Exec(c.plant); return err }
			}
			configPath, archive := shiftArchive(t, dbPath, plant)
			live, err := store.Open(dbPath)
			testutil.MustNoErr(t, err, "the live database")
			_, err = live.Exec(`INSERT INTO shifts (name, shift_number, start_time, end_time) VALUES ('LIVE', 1, '07:00', '15:00')`)
			testutil.MustNoErr(t, err, "the live row")
			testutil.MustNoErr(t, live.Close(), "a clean stop")

			testutil.MustNoErr(t, RestoreArchiveNow(configPath, bytes.NewReader(archive), ""), "restore")
			if got := shiftNames(t, dbPath); got != c.want {
				t.Errorf("shifts after the restore = %q, want %q", got, c.want)
			}
		})
	}
}
