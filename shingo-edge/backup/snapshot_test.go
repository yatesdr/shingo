package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/internal/testdb"
)

// The archive is what a disaster recovery restores from, and nothing in this
// package ever built one in a test: the two existing test functions cover the
// restore MARKER and the retention policy, and neither produces or opens a
// .tar.gz. So the entry list, the entry order, the manifest and -- most
// importantly -- whether the recorded SHA256s describe the bytes actually
// written were all unverified.
//
// This opens the real artifact and checks it end to end.

func snapshotFixture(t *testing.T) (cfg *config.Config, configPath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "shingoedge.yaml")
	testutil.MustNoErr(t, os.WriteFile(configPath,
		[]byte("station_id: ST-BACKUP\ndatabase_path: test.db\n"), 0o644), "write config")
	cfg = config.Defaults()
	return cfg, configPath
}

func TestCreateSnapshotArchive_RoundTrip(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	cfg, configPath := snapshotFixture(t)

	archivePath, manifest, size, cleanup, err := createSnapshotArchive(db, cfg, configPath, "v-test")
	if err != nil {
		t.Fatalf("createSnapshotArchive: %v", err)
	}
	defer cleanup()

	if size <= 0 {
		t.Errorf("reported size = %d, want > 0", size)
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if info.Size() != size {
		t.Errorf("reported size %d != actual %d — the caller uploads this number", size, info.Size())
	}

	raw, err := os.ReadFile(archivePath)
	testutil.MustNoErr(t, err, "read archive")
	entries, order := readArchive(t, raw)

	// Manifest FIRST: a restore reads it to learn what else is in the tar, so
	// it cannot be behind the members it describes.
	if len(entries) != 3 {
		t.Fatalf("archive has %d entries, want 3: %v", len(entries), order)
	}
	if order[0] != ManifestName {
		t.Errorf("first entry = %q, want %q — a restore reads the manifest before the members", order[0], ManifestName)
	}
	for _, want := range []string{ManifestName, ConfigEntryName, DBEntryName} {
		if _, ok := entries[want]; !ok {
			t.Errorf("archive is missing %q; has %v", want, order)
		}
	}

	var onDisk Manifest
	testutil.MustNoErr(t, json.Unmarshal(entries[ManifestName], &onDisk), "parse manifest from the archive")
	if onDisk.FormatVersion != FormatVersion {
		t.Errorf("format_version = %d, want %d", onDisk.FormatVersion, FormatVersion)
	}
	if onDisk.AppVersion != "v-test" {
		t.Errorf("app_version = %q, want v-test", onDisk.AppVersion)
	}
	if onDisk.StationID != manifest.StationID {
		t.Errorf("manifest in the tar says station %q, returned manifest says %q",
			onDisk.StationID, manifest.StationID)
	}
	if len(onDisk.Files) != 2 {
		t.Fatalf("manifest lists %d files, want 2 (config + db)", len(onDisk.Files))
	}

	// THE ASSERTION THIS TEST EXISTS FOR. A manifest whose hashes describe
	// something other than the bytes in the tar is worse than no manifest: a
	// restore would verify happily against the wrong content.
	for _, mf := range onDisk.Files {
		body, ok := entries[mf.Name]
		if !ok {
			t.Errorf("manifest names %q but the tar has no such entry", mf.Name)
			continue
		}
		if int64(len(body)) != mf.Size {
			t.Errorf("%s: manifest size %d, archived bytes %d", mf.Name, mf.Size, len(body))
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != mf.SHA256 {
			t.Errorf("%s: manifest sha256 %s does not match the archived bytes (%s)", mf.Name, mf.SHA256, got)
		}
	}

	// The config in the tar is the file that was passed in, not a default.
	original, err := os.ReadFile(configPath)
	testutil.MustNoErr(t, err, "read source config")
	if !bytes.Equal(entries[ConfigEntryName], original) {
		t.Error("the archived config is not the file that was backed up")
	}
	// VACUUM INTO produces a real SQLite file; its header is the contract a
	// restore relies on.
	if db := entries[DBEntryName]; len(db) < 16 || string(db[:15]) != "SQLite format 3" {
		t.Error("the archived database does not begin with a SQLite header")
	}
}

// The SUCCESS path's cleanup contract: the archive survives until the caller
// runs cleanup, and cleanup then removes the directory.
//
// This does NOT cover the open-handle defect -- by the time a test can call
// cleanup, createSnapshotArchive has returned and its defers have already
// closed everything. The failure path is where that went wrong, and it is
// pinned by TestPackArchive_LeavesNothingOpenOnFailure.
func TestCreateSnapshotArchive_CleanupRemovesTheTempDir(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	cfg, configPath := snapshotFixture(t)

	archivePath, _, _, cleanup, err := createSnapshotArchive(db, cfg, configPath, "v-test")
	if err != nil {
		t.Fatalf("createSnapshotArchive: %v", err)
	}
	tmpDir := filepath.Dir(archivePath)
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("archive missing before cleanup: %v", err)
	}

	cleanup()

	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Errorf("temp dir %s survived cleanup (stat err = %v); on Windows this is a "+
			"leaked copy of the config and the whole database, and the error is discarded",
			tmpDir, err)
	}
}

// readArchive returns the entries by name AND the order they appear in, which
// is part of the contract: a restore reads the manifest to learn what else is
// in the tar.
func readArchive(t *testing.T, raw []byte) (map[string][]byte, []string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	testutil.MustNoErr(t, err, "gzip reader")
	defer gz.Close()

	out := map[string][]byte{}
	var order []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		testutil.MustNoErr(t, err, "tar next")
		body, err := io.ReadAll(tr)
		testutil.MustNoErr(t, err, "read tar entry")
		out[h.Name] = body
		order = append(order, h.Name)
	}
	return out, order
}

// THE WINDOWS PIN. packArchive must leave nothing open behind it, because the
// archive it creates lives inside the temp dir its caller then deletes.
//
// Driven through the ERROR path on purpose: that is where the old code called
// os.RemoveAll while the archive handle was still open, since a deferred Close
// inside createSnapshotArchive did not run until that whole function returned.
// Linux unlinks an open file without complaint, so this always passed there and
// the defect was invisible on the Pi and in CI. On Windows RemoveAll returns
// "The process cannot access the file because it is being used by another
// process", gives up rather than retrying, and the caller discards the error.
func TestPackArchive_LeavesNothingOpenOnFailure(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "out.tar.gz")

	// No staged config or database in tmpDir, so packArchive gets past
	// os.Create -- the handle is open -- and then fails writing the members.
	_, err := packArchive(archivePath, tmpDir, []byte(`{"format_version":1}`), time.Now().UTC())
	if err == nil {
		t.Fatal("packArchive succeeded with nothing staged to archive; the fixture is not exercising the error path")
	}
	if _, sErr := os.Stat(archivePath); sErr != nil {
		t.Fatalf("expected a partial archive to exist after the failure: %v", sErr)
	}

	if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
		t.Fatalf("the temp dir could not be removed after packArchive failed: %v\n"+
			"packArchive left the archive handle open, which on Windows leaks the "+
			"whole directory -- and the production caller discards this error", rmErr)
	}
}
