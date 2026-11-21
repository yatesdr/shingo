package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"shingo/protocol/testutil"
	"strings"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/internal/testdb"
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
