//go:build docker

package www

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/store"
)

// Characterization tests for handlers_config.go — handleConfig (renders the
// config page) and handleConfigSave (per-section form persist + hot-reload).
//
// The save handler writes config.yaml to disk via cfg.Save(h.engine.ConfigPath()),
// so we set ConfigPath to a temp file and assert both the in-memory config
// mutation and the redirect.

// testHandlersWithConfigPath builds a handler whose engine has a real
// ConfigPath pointing at a writable temp file. Required by handleConfigSave.
func testHandlersWithConfigPath(t *testing.T) (*Handlers, *store.DB, string) {
	t.Helper()

	db := testdb.Open(t)
	sim := simulator.New()

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"
	msgClient := messaging.NewClient(&cfg.Messaging)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	// Pre-write so that Save's WriteFile works.
	if err := os.WriteFile(cfgPath, []byte("{}"), 0644); err != nil {
		t.Fatalf("seed config file: %v", err)
	}

	eng := engine.New(engine.Config{
		AppConfig:  cfg,
		DB:         db,
		Fleet:      sim,
		MsgClient:  msgClient,
		LogFunc:    t.Logf,
		ConfigPath: cfgPath,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })

	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })

	dbgLog, _ := debuglog.New(64, nil)

	h := &Handlers{
		engine:   eng,
		orchestration: eng,
		sessions: newSessionStore("test-secret"),
		tmpls:    make(map[string]*template.Template),
		eventHub: hub,
		debugLog: dbgLog,
	}
	loadTestTemplates(t, h)
	return h, db, cfgPath
}

// --- handleConfig (page render) ---------------------------------------------

func TestHandleConfig_RendersHTML(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	h.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The "Configuration" heading is in the config.html template; if it is
	// present we know render() actually ran the template.
	if !strings.Contains(body, "Configuration") {
		t.Errorf("rendered HTML missing 'Configuration' heading; len=%d", len(body))
	}
}

func TestHandleConfig_SavedBanner(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	req := httptest.NewRequest(http.MethodGet, "/config?saved=database", nil)
	rec := httptest.NewRecorder()
	h.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Settings saved") || !strings.Contains(body, "database") {
		t.Errorf("expected saved banner with section name; body excerpt=%q", excerpt(body))
	}
}

// --- handleConfigSave -------------------------------------------------------

func TestHandleConfigSave_DatabaseSection(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	form := url.Values{}
	form.Set("section", "database")
	form.Set("pg_host", "db-1.example")
	form.Set("pg_port", "5433")
	form.Set("pg_database", "newdb")
	form.Set("pg_user", "newuser")
	form.Set("pg_password", "newsecret")
	form.Set("pg_sslmode", "require")
	form.Set("pg_max_open_conns", "42")

	rec := postForm(t, h.handleConfigSave, "/config/save", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "saved=database") {
		t.Errorf("redirect: got %q, want '...saved=database'", loc)
	}

	cfg := h.engine.AppConfig()
	if cfg.Database.Postgres.Host != "db-1.example" {
		t.Errorf("host: got %q", cfg.Database.Postgres.Host)
	}
	if cfg.Database.Postgres.Port != 5433 {
		t.Errorf("port: got %d", cfg.Database.Postgres.Port)
	}
	if cfg.Database.Postgres.Database != "newdb" {
		t.Errorf("database: got %q", cfg.Database.Postgres.Database)
	}
	if cfg.Database.Postgres.User != "newuser" {
		t.Errorf("user: got %q", cfg.Database.Postgres.User)
	}
	if cfg.Database.Postgres.Password != "newsecret" {
		t.Errorf("password not updated")
	}
	if cfg.Database.Postgres.MaxOpenConns != 42 {
		t.Errorf("max_open_conns: got %d", cfg.Database.Postgres.MaxOpenConns)
	}
}

func TestHandleConfigSave_FleetSection(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	form := url.Values{}
	form.Set("section", "fleet")
	form.Set("fleet_base_url", "http://fleet:8080")
	form.Set("fleet_poll_interval", "10s")
	form.Set("fleet_timeout", "5s")

	rec := postForm(t, h.handleConfigSave, "/config/save", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	cfg := h.engine.AppConfig()
	if cfg.RDS.BaseURL != "http://fleet:8080" {
		t.Errorf("base url: got %q", cfg.RDS.BaseURL)
	}
}

func TestHandleConfigSave_MessagingSection(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	form := url.Values{}
	form.Set("section", "messaging")
	form.Set("kafka_host_0", "broker-a")
	form.Set("kafka_port_0", "9092")
	form.Set("kafka_host_1", "broker-b")
	form.Set("kafka_port_1", "9093")
	form.Set("group_id", "shingo-test")
	form.Set("orders_topic", "orders.test")
	form.Set("dispatch_topic", "dispatch.test")

	rec := postForm(t, h.handleConfigSave, "/config/save", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	cfg := h.engine.AppConfig()
	if len(cfg.Messaging.Kafka.Brokers) != 2 {
		t.Fatalf("brokers: got %d, want 2: %v", len(cfg.Messaging.Kafka.Brokers), cfg.Messaging.Kafka.Brokers)
	}
	if cfg.Messaging.Kafka.Brokers[0] != "broker-a:9092" {
		t.Errorf("broker[0]: got %q", cfg.Messaging.Kafka.Brokers[0])
	}
	if cfg.Messaging.Kafka.Brokers[1] != "broker-b:9093" {
		t.Errorf("broker[1]: got %q", cfg.Messaging.Kafka.Brokers[1])
	}
	if cfg.Messaging.OrdersTopic != "orders.test" {
		t.Errorf("orders_topic: got %q", cfg.Messaging.OrdersTopic)
	}
}

func TestHandleConfigSave_FireAlarmSection(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	form := url.Values{}
	form.Set("section", "fire_alarm")
	form.Set("fa_enabled", "on")
	form.Set("fa_auto_resume", "on")

	rec := postForm(t, h.handleConfigSave, "/config/save", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	cfg := h.engine.AppConfig()
	if !cfg.FireAlarm.Enabled {
		t.Error("fire alarm should be enabled")
	}
	if !cfg.FireAlarm.AutoResumeDefault {
		t.Error("fire alarm auto-resume should be enabled")
	}
}

func TestHandleConfigSave_UnknownSection(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	form := url.Values{}
	form.Set("section", "no-such-section")

	rec := postForm(t, h.handleConfigSave, "/config/save", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown section") {
		t.Errorf("body should mention 'unknown section', got %q", rec.Body.String())
	}
}

func TestHandleConfigSave_InvalidConfigPathReturns500(t *testing.T) {
	h, _, cfgPath := testHandlersWithConfigPath(t)
	// Make the config path unwritable by removing it and putting a directory
	// in its place — Save's WriteFile will then fail.
	if err := os.Remove(cfgPath); err != nil {
		t.Fatalf("remove cfg file: %v", err)
	}
	if err := os.Mkdir(cfgPath, 0755); err != nil {
		t.Fatalf("mkdir over cfg path: %v", err)
	}

	form := url.Values{}
	form.Set("section", "fire_alarm")
	form.Set("fa_enabled", "on")

	rec := postForm(t, h.handleConfigSave, "/config/save", form)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to save") {
		t.Errorf("body should mention 'Failed to save', got %q", rec.Body.String())
	}
}

// excerpt returns the first 200 chars of s for error messages.
func excerpt(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
