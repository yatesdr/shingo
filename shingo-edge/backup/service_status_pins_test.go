package backup

import (
	"encoding/json"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

// U0 pin (ui-cleanup, 2026-10-07), re-pointed by U3: what /api/backups/status
// carries with automatic backups off.
//
// E7: at the U0 tree refreshStaticStatus set NextScheduledAt whatever Enabled
// said, so the JSON named a next run that would never happen. It is omitted
// now when enabled=false. Everything else the U0 pin asserted is unchanged.
//
// E6: the status gains `configured` — the storage settings are complete,
// whatever Enabled says — so the page can list and restore with automatic
// backups off.
func TestPinEdgeConfig_BackupStatusDisabled(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/edge.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	cfg.Backup.Enabled = false
	cfg.Backup.ScheduleInterval = 2 * time.Hour
	svc := NewService(db, cfg, t.TempDir()+"/config.yaml", "pin", func(string, ...any) {})

	st := svc.Status()

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if m["enabled"] != false {
		t.Errorf("enabled = %v, want false", m["enabled"])
	}
	if m["schedule_interval"] != "2h0m0s" {
		t.Errorf("schedule_interval = %v, want 2h0m0s", m["schedule_interval"])
	}
	// E7: no next run with automatic backups off.
	if _, ok := m["next_scheduled_at"]; ok || st.NextScheduledAt != nil {
		t.Errorf("next_scheduled_at present with enabled=false (E7 omits it): %s", raw)
	}
	if m["stale"] != false {
		t.Errorf("stale = %v, want false when disabled", m["stale"])
	}
	if _, ok := m["stale_reason"]; ok {
		t.Errorf("stale_reason present when disabled: %s", raw)
	}
	// E6: no storage set → configured false.
	if m["configured"] != false {
		t.Errorf("configured = %v, want false with no storage set: %s", m["configured"], raw)
	}

	// E6: complete storage with automatic backups OFF is still configured
	// (backupsConfigured would say false here, which is why it is not reused).
	cfg.Lock()
	cfg.Backup.S3 = config.BackupS3Config{Endpoint: "http://s3.pin.invalid", Bucket: "b", AccessKey: "k", SecretKey: "s"}
	cfg.Unlock()
	if st := svc.Status(); !st.Configured || st.Enabled || st.NextScheduledAt != nil {
		t.Errorf("complete storage, disabled: configured=%v enabled=%v next=%v, want true/false/nil",
			st.Configured, st.Enabled, st.NextScheduledAt)
	}

	// Enabled: the next run is back (now + interval, no last success).
	cfg.Lock()
	cfg.Backup.Enabled = true
	cfg.Unlock()
	before := time.Now().UTC()
	st = svc.Status()
	if st.NextScheduledAt == nil || st.NextScheduledAt.Before(before.Add(2*time.Hour-time.Second)) ||
		st.NextScheduledAt.After(time.Now().UTC().Add(2*time.Hour+time.Second)) {
		t.Errorf("enabled: next_scheduled_at = %v, want about now+2h", st.NextScheduledAt)
	}
}
