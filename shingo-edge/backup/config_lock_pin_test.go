//go:build lockcheck || sim

package backup

import (
	"context"
	"errors"
	"os"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/config"
)

// TestConfigLockPin_BackupNeverReadLocksTheConfigTwice (LOCKS.md, E1): the
// backup read the station under its own read lock on the config, and
// StationID read-locks it again. A config write arriving between the two (the
// heartbeat's timezone read took the write lock every 60 s) leaves both
// waiting for ever, and every later reader of the config with them. The guard
// fails a second read lock on one goroutine, so this is red in a guard build at
// 545b8b57, on each of the three paths.
func TestConfigLockPin_BackupNeverReadLocksTheConfigTwice(t *testing.T) {
	svc, _ := newTestService(t, config.BackupConfig{Enabled: true, S3: completeS3()})
	fail := errors.New("no storage in this test")
	svc.storageFactory = func(config.BackupS3Config) (Storage, error) { return nil, fail }

	if _, _, _, err := svc.storageFromConfig(); !errors.Is(err, fail) {
		t.Fatalf("storageFromConfig: %v", err)
	}
	if err := svc.TestConfig(context.Background(), completeS3()); !errors.Is(err, fail) {
		t.Fatalf("TestConfig: %v", err)
	}
	testutil.MustNoErr(t, os.WriteFile(svc.configPath, []byte("{}"), 0o600), "a config file to copy")
	_, _, _, cleanup, err := createSnapshotArchive(svc.db, svc.cfg, svc.configPath, "test")
	if err != nil {
		t.Fatalf("createSnapshotArchive: %v", err)
	}
	cleanup()
}
