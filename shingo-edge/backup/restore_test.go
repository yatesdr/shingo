package backup

import (
	"os"
	"path/filepath"
	"shingo/protocol/testutil"
	"testing"
	"time"
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
