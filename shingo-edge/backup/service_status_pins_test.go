package backup

import (
	"encoding/json"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

// U0 pin (ui-cleanup, 2026-10-07): what /api/backups/status carries with
// automatic backups off. Today refreshStaticStatus sets NextScheduledAt
// whatever Enabled says (service.go:410), so the JSON names a next run that
// will never happen. E7 omits it when enabled=false; the prediction is in
// predictions/u0-edge.md.
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

	before := time.Now().UTC()
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
	if _, ok := m["next_scheduled_at"]; !ok {
		t.Fatalf("next_scheduled_at absent with enabled=false; the pin expects it present today: %s", raw)
	}
	// now + interval, no last success to anchor on.
	if st.NextScheduledAt == nil || st.NextScheduledAt.Before(before.Add(2*time.Hour-time.Second)) ||
		st.NextScheduledAt.After(time.Now().UTC().Add(2*time.Hour+time.Second)) {
		t.Errorf("next_scheduled_at = %v, want about now+2h", st.NextScheduledAt)
	}
	if m["stale"] != false {
		t.Errorf("stale = %v, want false when disabled", m["stale"])
	}
	if _, ok := m["stale_reason"]; ok {
		t.Errorf("stale_reason present when disabled: %s", raw)
	}
}
