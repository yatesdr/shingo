package www

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — covers handlers_kanbans.go and handlers_backup.go.
//
// Kanbans: handleKanbans and handleKanbansPartial both render templates
// (renderTemplate is a no-op in the test harness for the former; the
// latter calls h.tmpl.ExecuteTemplate directly on a nil *template.Template
// and panics). Only admin-gate-style coverage is achievable. In router.go
// /kanbans and /kanbans/partial are public routes, so there is no
// middleware gate to hit; the handlers are not exercised from tests.
// Coverage of their DB call sites therefore lands through the process
// helpers already exercised elsewhere — we assert shape at the DB layer.
//
// Backup: every endpoint except apiUpdateBackupConfig guards on
// `h.backup == nil` and returns 501. In the test harness h.backup is nil,
// so we cover all five early-exit branches. apiUpdateBackupConfig does
// *not* have that guard and is testable end-to-end (cfg.Save lands on
// disk, h.requestBackup is a safe no-op when h.backup is nil).
// ═══════════════════════════════════════════════════════════════════════

func newKanbansBackupRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	r.Route("/api", func(r chi.Router) {
		r.Get("/backups", h.apiListBackups)
		r.Get("/backups/status", h.apiBackupStatus)
		r.Put("/backups/config", h.apiUpdateBackupConfig)
		r.Post("/backups/test", h.apiTestBackupConfig)
		r.Post("/backups/run", h.apiRunBackup)
		r.Post("/backups/restore", h.apiStageBackupRestore)
	})
	return h, r
}

// ═══════════════════════════════════════════════════════════════════════
// Nil-backup early exits — 5 endpoints, same 501 branch.
// ═══════════════════════════════════════════════════════════════════════

func TestApiBackup_NilService_ReturnsNotImplemented(t *testing.T) {
	_, router := newKanbansBackupRouter(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"status", "GET", "/api/backups/status", nil},
		{"list", "GET", "/api/backups", nil},
		{"test", "POST", "/api/backups/test", map[string]string{"endpoint": "x"}},
		{"run", "POST", "/api/backups/run", nil},
		{"stage_restore", "POST", "/api/backups/restore", map[string]string{"key": "some/key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRequest(t, router, tc.method, tc.path, tc.body, nil)
			assertStatus(t, resp, http.StatusNotImplemented)
			assertJSONPath(t, resp, "error", "backup service unavailable")
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// apiUpdateBackupConfig — no nil-check, fully testable.
// ═══════════════════════════════════════════════════════════════════════

func TestApiUpdateBackupConfig_DisabledDefaults(t *testing.T) {
	h, router := newKanbansBackupRouter(t)

	// Disabled path skips the required-fields validation; interval can still
	// be supplied for schedule bookkeeping.
	body := map[string]interface{}{
		"enabled":           false,
		"schedule_interval": "2h",
		"keep_hourly":       4,
		"keep_daily":        7,
		"keep_weekly":       4,
		"keep_monthly":      12,
		"endpoint":          "  ",
		"bucket":            "",
	}
	resp := doRequest(t, router, "PUT", "/api/backups/config", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	cfg := h.engine.AppConfig()
	cfg.Lock()
	defer cfg.Unlock()
	if cfg.Backup.Enabled {
		t.Errorf("Backup.Enabled: got true, want false")
	}
	if cfg.Backup.ScheduleInterval != 2*time.Hour {
		t.Errorf("ScheduleInterval: got %v, want 2h", cfg.Backup.ScheduleInterval)
	}
	if cfg.Backup.KeepHourly != 4 || cfg.Backup.KeepDaily != 7 ||
		cfg.Backup.KeepWeekly != 4 || cfg.Backup.KeepMonthly != 12 {
		t.Errorf("retention fields: got H=%d D=%d W=%d M=%d",
			cfg.Backup.KeepHourly, cfg.Backup.KeepDaily,
			cfg.Backup.KeepWeekly, cfg.Backup.KeepMonthly)
	}
}

func TestApiUpdateBackupConfig_EnabledTrimsAndPersists(t *testing.T) {
	h, router := newKanbansBackupRouter(t)

	body := map[string]interface{}{
		"enabled":                  true,
		"schedule_interval":        "30m",
		"keep_hourly":              1,
		"endpoint":                 "  https://s3.example.com  ",
		"bucket":                   "  my-bucket  ",
		"region":                   "  us-east-1  ",
		"access_key":               "  AKIA...  ",
		"secret_key":               "  SECRET  ",
		"use_path_style":           true,
		"insecure_skip_tls_verify": false,
	}
	resp := doRequest(t, router, "PUT", "/api/backups/config", body, nil)
	assertStatus(t, resp, http.StatusOK)

	cfg := h.engine.AppConfig()
	cfg.Lock()
	defer cfg.Unlock()
	if !cfg.Backup.Enabled {
		t.Error("Backup.Enabled: got false, want true")
	}
	if cfg.Backup.ScheduleInterval != 30*time.Minute {
		t.Errorf("ScheduleInterval: got %v, want 30m", cfg.Backup.ScheduleInterval)
	}
	// All string fields should be trimmed.
	if cfg.Backup.S3.Endpoint != "https://s3.example.com" {
		t.Errorf("Endpoint: got %q", cfg.Backup.S3.Endpoint)
	}
	if cfg.Backup.S3.Bucket != "my-bucket" {
		t.Errorf("Bucket: got %q", cfg.Backup.S3.Bucket)
	}
	if cfg.Backup.S3.Region != "us-east-1" {
		t.Errorf("Region: got %q", cfg.Backup.S3.Region)
	}
	if cfg.Backup.S3.AccessKey != "AKIA..." {
		t.Errorf("AccessKey: got %q", cfg.Backup.S3.AccessKey)
	}
	if cfg.Backup.S3.SecretKey != "SECRET" {
		t.Errorf("SecretKey: got %q", cfg.Backup.S3.SecretKey)
	}
	if !cfg.Backup.S3.UsePathStyle {
		t.Error("UsePathStyle: got false, want true")
	}
}

func TestApiUpdateBackupConfig_EnabledRequiresEndpoint(t *testing.T) {
	_, router := newKanbansBackupRouter(t)

	body := map[string]interface{}{
		"enabled":           true,
		"schedule_interval": "1h",
		"endpoint":          "", // missing required field
		"bucket":            "b",
		"access_key":        "k",
		"secret_key":        "s",
	}
	resp := doRequest(t, router, "PUT", "/api/backups/config", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiUpdateBackupConfig_EnabledRequiresBucket(t *testing.T) {
	_, router := newKanbansBackupRouter(t)

	body := map[string]interface{}{
		"enabled":           true,
		"schedule_interval": "1h",
		"endpoint":          "https://example.com",
		"bucket":            "  ", // whitespace = empty after trim
		"access_key":        "k",
		"secret_key":        "s",
	}
	resp := doRequest(t, router, "PUT", "/api/backups/config", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiUpdateBackupConfig_EnabledRequiresAccessAndSecret(t *testing.T) {
	_, router := newKanbansBackupRouter(t)

	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing access_key", map[string]interface{}{
			"enabled":           true,
			"schedule_interval": "1h",
			"endpoint":          "e",
			"bucket":            "b",
			"access_key":        "",
			"secret_key":        "s",
		}},
		{"missing secret_key", map[string]interface{}{
			"enabled":           true,
			"schedule_interval": "1h",
			"endpoint":          "e",
			"bucket":            "b",
			"access_key":        "k",
			"secret_key":        "",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRequest(t, router, "PUT", "/api/backups/config", tc.body, nil)
			assertStatus(t, resp, http.StatusBadRequest)
		})
	}
}

func TestApiUpdateBackupConfig_InvalidScheduleInterval(t *testing.T) {
	_, router := newKanbansBackupRouter(t)

	body := map[string]interface{}{
		"enabled":           false,
		"schedule_interval": "not-a-duration",
	}
	resp := doRequest(t, router, "PUT", "/api/backups/config", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiUpdateBackupConfig_EnabledZeroInterval(t *testing.T) {
	_, router := newKanbansBackupRouter(t)

	// When ScheduleInterval string is empty, the handler defaults to 1h —
	// so to hit the "interval <= 0" branch we must pass "0s" explicitly.
	body := map[string]interface{}{
		"enabled":           true,
		"schedule_interval": "0s",
		"endpoint":          "e",
		"bucket":            "b",
		"access_key":        "k",
		"secret_key":        "s",
	}
	resp := doRequest(t, router, "PUT", "/api/backups/config", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiUpdateBackupConfig_InvalidJSON(t *testing.T) {
	_, router := newKanbansBackupRouter(t)

	// enabled must be bool; sending a string breaks the outer decode.
	body := map[string]interface{}{"enabled": "yes"}
	resp := doRequest(t, router, "PUT", "/api/backups/config", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// requestBackup — package-level helper that the handler calls after
// persisting. With h.backup == nil it must be a safe no-op; this is
// already exercised indirectly by TestApiUpdateBackupConfig_EnabledTrimsAndPersists,
// but we pin the contract directly here to guard against regressions.
// ═══════════════════════════════════════════════════════════════════════

func TestRequestBackup_NilServiceIsSafe(t *testing.T) {
	h, _ := newKanbansBackupRouter(t)
	// Must not panic.
	h.requestBackup("unit-test")
}
