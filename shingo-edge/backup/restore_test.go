package backup

import (
	"bytes"
	"database/sql"
	"errors"
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

// TestRestore_AfterACrashPutsTheBackupInPlace: an Edge that was killed leaves
// the live database's sidecars beside it. SQLite replays a write-ahead log, or
// rolls back a hot journal, onto whatever file has the database's name, so a
// restore that replaces only the main file does not put the backup in place.
func TestRestore_AfterACrashPutsTheBackupInPlace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, journal string
		sidecars      []string
		// left is what the killed writer had done when it died.
		left []string
	}{
		{"a write-ahead log", "WAL", []string{"-wal", "-shm"},
			[]string{`UPDATE shifts SET name = 'LIVE'`}},
		{"a hot journal", "DELETE", []string{"-journal"},
			[]string{`UPDATE shifts SET name = 'LIVE'`,
				`PRAGMA cache_size = 1`,
				`BEGIN`,
				`UPDATE shifts SET name = 'LIVE, UNCOMMITTED'`,
				`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300)
					INSERT INTO outbox (topic, payload, msg_type) SELECT 'shingo.orders', zeroblob(2000), 'crash.pin' FROM n`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dbPath := filepath.Join(t.TempDir(), "shingoedge.db")
			configPath, archive := shiftArchive(t, dbPath, nil)

			// The box was restored from this backup, ran, and was killed. Its file
			// and the backup then share a page layout, so what replays reads whole;
			// with another layout the replay is a mixed file.
			testutil.MustNoErr(t, RestoreArchiveNow(configPath, bytes.NewReader(archive), ""), "the earlier restore")
			base, err := os.ReadFile(dbPath)
			testutil.MustNoErr(t, err, "read the restored file")
			// A crash image is the files as they stand while the writer still holds them.
			writing := filepath.Join(t.TempDir(), "writing.db")
			testutil.MustNoErr(t, os.WriteFile(writing, base, 0o644), "the writer's file")
			writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(writing)+"?_pragma=journal_mode("+c.journal+")")
			testutil.MustNoErr(t, err, "the killed writer")
			writer.SetMaxOpenConns(1)
			defer writer.Close()
			for _, stmt := range c.left {
				_, err := writer.Exec(stmt)
				testutil.MustNoErr(t, err, stmt)
			}
			for _, suffix := range append([]string{""}, c.sidecars...) {
				image, err := os.ReadFile(writing + suffix)
				testutil.MustNoErr(t, err, "the crash image's "+suffix)
				if suffix != "-shm" && len(image) == 0 {
					t.Fatalf("the writer left an empty %q: the test has no crash to restore over", suffix)
				}
				testutil.MustNoErr(t, os.WriteFile(dbPath+suffix, image, 0o644), "place the crash image")
			}

			testutil.MustNoErr(t, RestoreArchiveNow(configPath, bytes.NewReader(archive), ""), "restore")
			if got := shiftNames(t, dbPath); got != "BACKUP" {
				t.Errorf("shifts after the restore = %q, want \"BACKUP\": the restore did not put the backup in place", got)
			}
		})
	}
}

// TestRestore_WhatItPutsInPlace is which backups replace the live database:
// one of this build's schema does; one from below v59 is refused by name when
// it is staged, while it waits and when it is applied, with the live database
// and the config untouched.
func TestRestore_WhatItPutsInPlace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		plant string
		want  string
	}{
		{"this build's schema", "", "BACKUP"},
		{"a schema below v59", `DELETE FROM schema_migrations WHERE version > 40`, "LIVE"},
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
			configBefore, err := os.ReadFile(configPath)
			testutil.MustNoErr(t, err, "read the config")
			dbBefore, err := os.ReadFile(dbPath)
			testutil.MustNoErr(t, err, "read the live database")

			refused := c.plant != ""
			judge := func(door string, err error) {
				t.Helper()
				var refusal *store.Refusal
				switch {
				case !refused && err != nil:
					t.Errorf("%s: %v, want it passed", door, err)
				case refused && !errors.As(err, &refusal):
					t.Errorf("%s: %v, want a refusal", door, err)
				case refused && !strings.Contains(err.Error(), store.RetiredChainRelease):
					t.Errorf("%s: %v does not name %s", door, err, store.RetiredChainRelease)
				}
			}
			judge("staging", checkArchiveData(archive, ""))
			testutil.MustNoErr(t, StageRestoreArchive(configPath, "key", bytes.NewReader(archive), ""), "leave one staged")
			staged, err := CheckPendingRestore(configPath)
			if !staged {
				t.Error("the staged restore was not seen")
			}
			judge("migrate check", err)
			judge("applying", RestoreArchiveNow(configPath, bytes.NewReader(archive), ""))

			if refused {
				configAfter, err := os.ReadFile(configPath)
				testutil.MustNoErr(t, err, "read the config after")
				dbAfter, err := os.ReadFile(dbPath)
				testutil.MustNoErr(t, err, "read the live database after")
				if !bytes.Equal(configBefore, configAfter) || !bytes.Equal(dbBefore, dbAfter) {
					t.Error("a refused restore changed the config or the live database")
				}
			}
			if got := shiftNames(t, dbPath); got != c.want {
				t.Errorf("shifts after the restore = %q, want %q", got, c.want)
			}
		})
	}
}
