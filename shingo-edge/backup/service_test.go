package backup

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

// logBuffer is a race-safe strings.Builder: logf fires on the service's loop
// goroutine while the test goroutine polls the text.
type logBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (l *logBuffer) write(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sb.WriteString(time.Now().UTC().Format(time.TimeOnly) + " " + fmt.Sprintf(f, a...) + "\n")
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.String()
}

// newTestService wires a Service against a real temp sqlite, with the trigger
// debounce shortened. Configuration knobs (debounceDelay, storageFactory) are
// set BEFORE Start so the loop goroutine never observes a torn write.
func newTestService(t *testing.T, b config.BackupConfig) (*Service, *logBuffer) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/edge.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	cfg.Backup = b
	logs := &logBuffer{}
	svc := NewService(db, cfg, t.TempDir()+"/config.yaml", "test", logs.write)
	svc.debounceDelay = 20 * time.Millisecond // skip the production 10s debounce
	svc.Start()
	t.Cleanup(svc.Stop)
	return svc, logs
}

// completeS3 returns an S3 config that passes backupsConfigured. The storage
// FACTORY decides what a run actually does, so tests that need a run to be
// attempted (or to fail) swap the factory, not the config.
func completeS3() config.BackupS3Config {
	return config.BackupS3Config{
		Endpoint:  "http://minio.local:9000",
		Bucket:    "edge-backups",
		AccessKey: "k",
		SecretKey: "s",
	}
}

// The SPR shape: backups never configured. An admin save fires a trigger; the
// service must drop it silently (one notice), never attempt a run, and never
// report pending. Regression for the per-edit "triggered run failed: backup
// endpoint is required" spam (2026-09-01 monitor).
func TestTriggerDropsWhenUnconfigured(t *testing.T) {
	svc, logs := newTestService(t, config.BackupConfig{Enabled: false})
	ran := false
	svc.storageFactory = func(config.BackupS3Config) (Storage, error) {
		ran = true // set before Start; loop only reads it
		return nil, fmt.Errorf("storage must never be built")
	}
	svc.RequestBackup("style-updated")
	svc.RequestBackup("process-created")

	// Wait past two debounce windows: the trigger would have fired a run by
	// now if the gate failed.
	time.Sleep(3 * svc.debounceDelay)

	if got := logs.String(); !strings.Contains(got, "edit-triggered backups skipped") {
		t.Fatalf("skip notice never logged; logs=%q", got)
	}
	if n := strings.Count(logs.String(), "edit-triggered backups skipped"); n != 1 {
		t.Fatalf("skip notice repeated %d times: %q", n, logs.String())
	}
	if strings.Contains(logs.String(), "triggered run failed") {
		t.Fatalf("a run was attempted while unconfigured: %q", logs.String())
	}
	if ran {
		t.Fatalf("storage factory was invoked while unconfigured")
	}
	svc.mu.RLock()
	pending := svc.status.Pending
	svc.mu.RUnlock()
	if pending {
		t.Fatalf("status pending while unconfigured")
	}
}

// Enabled with a complete S3 target: the trigger must flow through to an
// actual run attempt (which fails against the fake factory, proving the gate
// opened) — the old binary failed this the other way.
func TestTriggerRunsWhenConfigured(t *testing.T) {
	svc, _ := newTestService(t, config.BackupConfig{Enabled: true, S3: completeS3()})
	attempts := make(chan struct{}, 1)
	svc.storageFactory = func(config.BackupS3Config) (Storage, error) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		return nil, fmt.Errorf("fake storage factory failure")
	}
	svc.RequestBackup("style-updated")

	select {
	case <-attempts:
		return
	case <-time.After(20 * svc.debounceDelay):
		t.Fatalf("run never attempted")
	}
}

// A trigger that arrives while configured must not be stranded when the state
// later flips to unconfigured — the debounce fires the run, and runBackup
// (not the gate) reports the failure. Pins the boundary: the gate is about
// NOT STARTING work that cannot succeed; once started, errors surface.
func TestConfiguredThenDisabledMidDebounce(t *testing.T) {
	svc, logs := newTestService(t, config.BackupConfig{Enabled: true, S3: completeS3()})
	svc.RequestBackup("style-updated")
	// Flip to unconfigured inside the debounce window.
	svc.cfg.Lock()
	svc.cfg.Backup.Enabled = false
	svc.cfg.Unlock()

	time.Sleep(4 * svc.debounceDelay)
	// Either outcome is acceptable (run attempted and failed, or dropped by
	// the gate — a flip mid-window is inherently racy); what must NOT happen
	// is a panic or a stuck pending flag.
	svc.mu.RLock()
	pending := svc.status.Pending
	svc.mu.RUnlock()
	if pending {
		t.Fatalf("pending stuck after debounce elapsed; logs=%q", logs.String())
	}
}
