//go:build docker

package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol/auth"
	"shingo/protocol/debuglog"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/store"
)

// Characterization tests for handlers_config.go — handleConfig (renders the
// config page) and apiConfigSave (PUT /api/config: the page's one save door,
// which replaced the per-section form door POST /config/save in U2, R4).
//
// The save handler writes config.yaml to disk via draft.Save(ConfigPath()),
// so we set ConfigPath to a temp file and assert the in-memory config.

// testHandlersWithConfigPath builds a handler whose engine has a real
// ConfigPath pointing at a writable temp file. Required by apiConfigSave.
func testHandlersWithConfigPath(t *testing.T) (*Handlers, *store.DB, string) {
	t.Helper()
	h, db, path, _ := testHandlersWithConfigAndDB(t)
	return h, db, path
}

// testHandlersWithConfigAndDB is testHandlersWithConfigPath plus the
// coordinates of the test container's database, for the database save, which
// pings a real pool against what it is given (C3, R27).
func testHandlersWithConfigAndDB(t *testing.T) (*Handlers, *store.DB, string, *config.DatabaseConfig) {
	t.Helper()

	db, dbCfg := testdb.OpenWithConfig(t)
	sim := simulator.New()

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"
	msgClient := messaging.NewClient(&cfg.Messaging)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	// Pre-write so that Save's WriteFile works.
	testutil.MustNoErr(t, os.WriteFile(cfgPath, []byte("{}"), 0644), "seed config file")

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
		engine:        eng,
		orchestration: eng,
		sessions:      newSessionStore("test-secret"),
		tmpls:         make(map[string]*template.Template),
		eventHub:      hub,
		debugLog:      dbgLog,
	}
	loadTestTemplates(t, h)
	return h, db, cfgPath, dbCfg
}

// --- handleConfig (page render) ---------------------------------------------

func TestHandleConfig_RendersHTML(t *testing.T) {
	t.Parallel()
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

// --- apiConfigSave (re-pointed from the form door, U2) ----------------------

// L3 + R27: the database save pings the draft (C3), so it posts the test
// container's own coordinates; the posted fields are applied to the config
// and the file, and nothing swaps the live pool: the *sql.DB is the same object
// afterwards and still answers, and every changed database field waits on a
// restart.
func TestPinConfig_R27_DatabaseSaveKeepsLivePool(t *testing.T) {
	t.Parallel()
	h, db, cfgPath, dbCfg := testHandlersWithConfigAndDB(t)
	h.boot = takeBootSnapshot(h.engine.AppConfig())
	pg := dbCfg.Postgres
	livePool := db.DB

	rec := pinPut(t, h.apiConfigSave, "/api/config", map[string]any{"database": map[string]any{
		"host": pg.Host, "port": pg.Port, "database": pg.Database, "user": pg.User,
		"password": pg.Password, "sslmode": pg.SSLMode, "max_open_conns": 42,
		"max_idle_conns": 5, "conn_max_lifetime": "5m0s",
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	cfg := h.engine.AppConfig()
	if cfg.Database.Postgres.Host != pg.Host {
		t.Errorf("host: got %q", cfg.Database.Postgres.Host)
	}
	if cfg.Database.Postgres.Port != pg.Port {
		t.Errorf("port: got %d", cfg.Database.Postgres.Port)
	}
	if cfg.Database.Postgres.Database != pg.Database {
		t.Errorf("database: got %q", cfg.Database.Postgres.Database)
	}
	if cfg.Database.Postgres.User != pg.User {
		t.Errorf("user: got %q", cfg.Database.Postgres.User)
	}
	if cfg.Database.Postgres.Password != pg.Password {
		t.Errorf("password not updated")
	}
	if cfg.Database.Postgres.MaxOpenConns != 42 {
		t.Errorf("max_open_conns: got %d", cfg.Database.Postgres.MaxOpenConns)
	}
	if got := pinYAML(t, cfgPath, "database.postgres.max_open_conns"); got != "42" {
		t.Errorf("yaml max_open_conns: got %q", got)
	}
	// R27: the live pool is the one Core booted with, still open.
	if db.DB != livePool {
		t.Errorf("the live *sql.DB was replaced by a database save")
	}
	if got := db.Stats().MaxOpenConnections; got == 42 {
		t.Errorf("the live pool took max_open_conns=42 before a restart")
	}
	if err := h.engine.HealthService().PingDB(); err != nil {
		t.Errorf("ping after the save: %v", err)
	}
	var ans configAnswer
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &ans), "decode answer")
	found := false
	for _, f := range ans.Restart {
		found = found || f == "Database connection pool"
	}
	if !found {
		t.Errorf("restart: got %q, want it to list the database connection pool", ans.Restart)
	}
}

// C3 (L3's added case): an unreachable host is refused before anything is
// written: 400, the file and the live config untouched, Core still on its
// database.
func TestHandleConfigSave_DatabaseDeadHostRefused(t *testing.T) {
	t.Parallel()
	h, _, cfgPath := testHandlersWithConfigPath(t)
	before := h.engine.AppConfig().Database
	fileBefore, err := os.ReadFile(cfgPath)
	testutil.MustNoErr(t, err, "read config before")

	rec := pinPut(t, h.apiConfigSave, "/api/config", map[string]any{"database": map[string]any{
		"host": "127.0.0.1", "port": 1, "database": "nodb", "user": "nobody",
		"password": "x", "sslmode": "disable", "max_open_conns": 2,
		"max_idle_conns": 1, "conn_max_lifetime": "1m0s",
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "did not answer") {
		t.Errorf("body should say the database did not answer, got %q", rec.Body.String())
	}
	if after := h.engine.AppConfig().Database; after != before {
		t.Errorf("live database config changed: %+v", after)
	}
	fileAfter, err := os.ReadFile(cfgPath)
	testutil.MustNoErr(t, err, "read config after")
	if string(fileAfter) != string(fileBefore) {
		t.Errorf("config file written by a refused database save")
	}
	if err := h.engine.HealthService().PingDB(); err != nil {
		t.Errorf("Core lost its database: %v", err)
	}
}

// C3's Test connection opens and pings the draft, nothing more.
func TestHandleConfigTestDatabase_RealContainer(t *testing.T) {
	t.Parallel()
	h, _, cfgPath, dbCfg := testHandlersWithConfigAndDB(t)
	pg := dbCfg.Postgres
	rec := pinRequest(t, http.MethodPost, h.apiConfigTestDatabase, "/api/config/test-database", map[string]any{
		"host": pg.Host, "port": pg.Port, "database": pg.Database, "user": pg.User,
		"password": pg.Password, "sslmode": pg.SSLMode, "max_open_conns": 2,
		"max_idle_conns": 1, "conn_max_lifetime": "1m0s",
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(cfgPath)
	testutil.MustNoErr(t, err, "read config file")
	if string(raw) != "{}" {
		t.Errorf("config file written by the test door")
	}
}

func TestHandleConfigSave_FleetSection(t *testing.T) {
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)

	rec := pinPut(t, h.apiConfigSave, "/api/config", map[string]any{"fleet": map[string]any{
		"base_url": "http://fleet:8080", "poll_interval": "10s", "timeout": "5s", "fault_grace": "45m0s",
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	cfg := h.engine.AppConfig()
	if cfg.RDS.BaseURL != "http://fleet:8080" {
		t.Errorf("base url: got %q", cfg.RDS.BaseURL)
	}
}

func TestHandleConfigSave_MessagingSection(t *testing.T) {
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)

	// Hosts are 127.0.0.1, ports closed, not made-up broker names: the save
	// triggers ReconfigureMessaging → Connect(), whose 5s dial deadline on an
	// unresolvable hostname made this test a flat ~11.7s (two brokers × 5s) — a
	// quarter of the package's wall time. Connection refused is instant and
	// exercises the same unreachable-broker path.
	rec := pinPut(t, h.apiConfigSave, "/api/config", map[string]any{"messaging": map[string]any{
		"brokers":        []string{"127.0.0.1:9092", "127.0.0.1:9093"},
		"group_id":       "shingo-test",
		"orders_topic":   "orders.test",
		"dispatch_topic": "dispatch.test",
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	cfg := h.engine.AppConfig()
	if len(cfg.Messaging.Kafka.Brokers) != 2 {
		t.Fatalf("brokers: got %d, want 2: %v", len(cfg.Messaging.Kafka.Brokers), cfg.Messaging.Kafka.Brokers)
	}
	if cfg.Messaging.Kafka.Brokers[0] != "127.0.0.1:9092" {
		t.Errorf("broker[0]: got %q", cfg.Messaging.Kafka.Brokers[0])
	}
	if cfg.Messaging.Kafka.Brokers[1] != "127.0.0.1:9093" {
		t.Errorf("broker[1]: got %q", cfg.Messaging.Kafka.Brokers[1])
	}
	if cfg.Messaging.OrdersTopic != "orders.test" {
		t.Errorf("orders_topic: got %q", cfg.Messaging.OrdersTopic)
	}
}

func TestHandleConfigSave_FireAlarmSection(t *testing.T) {
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)

	rec := pinPut(t, h.apiConfigSave, "/api/config", map[string]any{"fire_alarm": map[string]any{
		"enabled": true, "auto_resume_default": true,
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
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
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)

	rec := pinPut(t, h.apiConfigSave, "/api/config", map[string]any{"no-such-section": map[string]any{}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown section") {
		t.Errorf("body should mention 'unknown section', got %q", rec.Body.String())
	}
}

func TestHandleConfigSave_InvalidConfigPathReturns500(t *testing.T) {
	t.Parallel()
	h, _, cfgPath := testHandlersWithConfigPath(t)
	// Make the config path unwritable by removing it and putting a directory
	// in its place — Save's WriteFile will then fail.
	testutil.MustNoErr(t, os.Remove(cfgPath), "remove cfg file")
	testutil.MustNoErr(t, os.Mkdir(cfgPath, 0755), "mkdir over cfg path")

	rec := pinPut(t, h.apiConfigSave, "/api/config", map[string]any{"fire_alarm": map[string]any{"enabled": true}})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to save") {
		t.Errorf("body should mention 'Failed to save', got %q", rec.Body.String())
	}
}

// --- handleConfigPassword (admin password rotation) --------------------------

// loggedInSession returns a session cookie for the seeded admin/admin user, so
// the password handler can read a username off the request the way a real
// browser request carries one.
func loggedInSession(t *testing.T, h *Handlers) *http.Cookie {
	t.Helper()
	h.ensureDefaultAdmin()

	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "admin")
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleLogin(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionName {
			return c
		}
	}
	t.Fatalf("no %s cookie after login; status=%d", sessionName, rec.Code)
	return nil
}

func postPassword(t *testing.T, h *Handlers, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/config/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.handleConfigPassword(rec, req)
	return rec
}

func TestHandleConfigPassword_HappyPathRotatesTheHash(t *testing.T) {
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)
	cookie := loggedInSession(t, h)

	before, err := h.engine.AdminService().GetUser("admin")
	if err != nil {
		t.Fatalf("GetUser before: %v", err)
	}

	rec := postPassword(t, h, cookie,
		`{"old_password":"admin","new_password":"a-new-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	after, err := h.engine.AdminService().GetUser("admin")
	if err != nil {
		t.Fatalf("GetUser after: %v", err)
	}
	if after.PasswordHash == before.PasswordHash {
		t.Error("password hash unchanged after a successful rotation")
	}
	// The new password must actually authenticate, and the old one must not.
	if !auth.CheckPassword(after.PasswordHash, "a-new-password") {
		t.Error("new password does not verify against the stored hash")
	}
	if auth.CheckPassword(after.PasswordHash, "admin") {
		t.Error("old password still verifies after rotation")
	}
}

func TestHandleConfigPassword_WrongCurrentPasswordIsRejected(t *testing.T) {
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)
	cookie := loggedInSession(t, h)

	before, err := h.engine.AdminService().GetUser("admin")
	if err != nil {
		t.Fatalf("GetUser before: %v", err)
	}

	rec := postPassword(t, h, cookie,
		`{"old_password":"not-the-password","new_password":"whatever"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	after, err := h.engine.AdminService().GetUser("admin")
	if err != nil {
		t.Fatalf("GetUser after: %v", err)
	}
	if after.PasswordHash != before.PasswordHash {
		t.Error("password hash changed despite a rejected current password")
	}
}

func TestHandleConfigPassword_UnauthenticatedIsRejected(t *testing.T) {
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)
	h.ensureDefaultAdmin()

	rec := postPassword(t, h, nil,
		`{"old_password":"admin","new_password":"a-new-password"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleConfigPassword_EmptyNewPasswordIsRejected(t *testing.T) {
	t.Parallel()
	h, _, _ := testHandlersWithConfigPath(t)
	cookie := loggedInSession(t, h)

	rec := postPassword(t, h, cookie, `{"old_password":"admin","new_password":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
