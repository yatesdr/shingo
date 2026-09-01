package backup

import (
	"strings"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

// newTestService wires a Service against a real temp sqlite with a storage
// factory that fails the test if a backup run is ever attempted.
func newTestService(t *testing.T, b config.BackupConfig) (*Service, *strings.Builder) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/edge.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	cfg.Backup = b
	var logs strings.Builder
	svc := NewService(db, cfg, t.TempDir()+"/config.yaml", "test", func(f string, a ...any) {
		logs.WriteString(time.Now().UTC().Format(time.TimeOnly) + " " + sprintf(f, a...) + "\n")
	})
	svc.Start()
	t.Cleanup(svc.Stop)
	return svc, &logs
}

func sprintf(f string, a ...any) string {
	if len(a) == 0 {
		return f
	}
	return f + " " + fmtJoin(a)
}

func fmtJoin(a []any) string {
	parts := make([]string, 0, len(a))
	for _, v := range a {
		parts = append(parts, strings.TrimSpace(toStr(v)))
	}
	return strings.Join(parts, " ")
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	default:
		return ""
	}
}

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
	svc.RequestBackup("style-updated")
	svc.RequestBackup("process-created")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.RLock()
		pending := svc.status.Pending
		svc.mu.RUnlock()
		got := logs.String()
		if strings.Contains(got, "edit-triggered backups skipped") {
			if pending {
				t.Fatalf("status pending while unconfigured")
			}
			if strings.Count(got, "edit-triggered backups skipped") != 1 {
				t.Fatalf("skip notice repeated: %q", got)
			}
			if strings.Contains(got, "triggered run failed") {
				t.Fatalf("a run was attempted while unconfigured: %q", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("skip notice never logged; logs=%q", logs.String())
}

// Enabled with a complete S3 target: the trigger must flow through to an
// actual run attempt (which fails against the fake factory, proving the gate
// opened) — the old binary failed this the other way.
func TestTriggerRunsWhenConfigured(t *testing.T) {
	svc, logs := newTestService(t, config.BackupConfig{Enabled: true, S3: completeS3()})
	attempts := make(chan struct{}, 1)
	svc.debounceDelay = 20 * time.Millisecond // skip the production 10s debounce
	svc.storageFactory = func(config.BackupS3Config) (Storage, error) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		return nil, errFakeStorage
	}
	svc.RequestBackup("style-updated")

	select {
	case <-attempts:
		return
	case <-time.After(3 * time.Second):
		t.Fatalf("run never attempted; logs=%q", logs.String())
	}
}

var errFakeStorage = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake storage factory failure" }
