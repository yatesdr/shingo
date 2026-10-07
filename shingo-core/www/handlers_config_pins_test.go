package www

// U0 pins for Core's config save door, re-pointed by U2 at the new door
// (BRIEF-lead-ui-config-and-cleanup-2026-10-07, Part 1). At 5c0beb74 they
// pinned the form door, POST /config/save; U2 deleted it (R4) and each case
// now sends the same input to PUT /api/config as JSON and asserts the "after"
// value the evidence folder's predictions/u0-core.md wrote beside it. Every
// assertion that moved carries its label (C1-C9, R4, L1, "one save path").
// Case names are kept from U0 so each can be found against its prediction,
// even where the name describes the old behaviour.
//
// No database: the engine is a stub that answers AppConfig/ConfigPath and
// records the database ping (R27) and the Reconfigure* calls, so each case can
// say what the save would have checked and reloaded without a real database.
// The password door needs the admin store and is pinned in
// handlers_config_pins_docker_test.go.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"shingo/protocol/testutil"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"shingocore/config"
	"shingocore/service"
)

// configPinEngine is an EngineOrchestration whose only working methods are the
// ones the config doors reach: AppConfig, ConfigPath, HealthService and the
// three Reconfigure* verbs, which it records instead of running; the save
// door's database ping (Handlers.pingDB) is recorded through ping. Anything else panics on the nil embedded interface.
type configPinEngine struct {
	EngineOrchestration
	cfg   *config.Config
	path  string
	calls []string

	pingErr   error            // the save door's database ping answers this when set
	reconfErr map[string]error // a Reconfigure* answers its section's error
}

func (e *configPinEngine) AppConfig() *config.Config { return e.cfg }
func (e *configPinEngine) ConfigPath() string        { return e.path }

// HealthService is real: TestDatabase does not read its db, and a nil db
// makes PoolStats answer not-ok, so the page shows no database words.
func (e *configPinEngine) HealthService() *service.HealthService {
	return service.NewHealthService(nil)
}

// fileState says whether the config file still holds the seed, so a call can
// be recorded with where it ran relative to the write (C3: the database is
// pinged before the write).
func (e *configPinEngine) fileState() string {
	if raw, err := os.ReadFile(e.path); err == nil && string(raw) == configPinSeed {
		return "unwritten"
	}
	return "written"
}

// ping is the save door's database check (R27: ping only, no prepare, no
// swap).
func (e *configPinEngine) ping(config.DatabaseConfig) error {
	e.calls = append(e.calls, "ping-database@"+e.fileState())
	return e.pingErr
}

func (e *configPinEngine) record(name string) error {
	e.calls = append(e.calls, name)
	return e.reconfErr[name]
}
func (e *configPinEngine) ReconfigureFleet() error         { return e.record("fleet") }
func (e *configPinEngine) ReconfigureMessaging() error     { return e.record("messaging") }
func (e *configPinEngine) ReconfigureNotifications() error { return e.record("notifications") }

// configPinSeed is the file content before a save. A case whose save never
// reaches Save leaves exactly these bytes.
const configPinSeed = "{}"

// pinBaselineConfig is Defaults() with every field the five sections touch set
// to a value a dropped input would visibly leave behind.
func pinBaselineConfig() *config.Config {
	cfg := config.Defaults()
	pg := &cfg.Database.Postgres
	pg.Host = "db-old.test"
	pg.Port = 5432
	pg.Database = "olddb"
	pg.User = "olduser"
	pg.Password = "old-pg-secret"
	pg.SSLMode = "disable"
	pg.MaxOpenConns = 10
	pg.MaxIdleConns = 5
	pg.ConnMaxLifetime = 30 * time.Minute

	cfg.RDS.BaseURL = "http://fleet-old.test:8088"
	cfg.RDS.PollInterval = 5 * time.Second
	cfg.RDS.Timeout = 10 * time.Second
	cfg.RDS.FaultGrace = 45 * time.Minute

	cfg.Messaging.Kafka.Brokers = []string{"old-a.test:9092", "old-b.test:9092"}
	cfg.Messaging.Kafka.GroupID = "old-group"
	cfg.Messaging.OrdersTopic = "old.orders"
	cfg.Messaging.DispatchTopic = "old.dispatch"

	cfg.FireAlarm.Enabled = true
	cfg.FireAlarm.AutoResumeDefault = true

	n := &cfg.Notifications
	n.Enabled = true
	n.SMTPHost = "smtp-old.test"
	n.SMTPPort = 587
	n.SMTPTLS = true
	n.SMTPUser = "old-smtp-user"
	n.SMTPPassword = "old-smtp-secret"
	n.FromAddress = "old-from@test.invalid"
	n.Recipients = []string{"old-1@test.invalid", "old-2@test.invalid"}
	n.ThrottleMinutes = 15
	return cfg
}

// newConfigPinHandlers builds the handlers with a boot snapshot of cfg as it
// is passed in, so the restart notice is computed as it would be in a process
// that booted with it.
func newConfigPinHandlers(t *testing.T, cfg *config.Config) (*Handlers, *configPinEngine) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configPinSeed), 0644); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	e := &configPinEngine{cfg: cfg, path: path}
	return &Handlers{engine: e, orchestration: e, boot: takeBootSnapshot(cfg), pingDB: e.ping}, e
}

// pinYAML reads the written file back and returns the value at a dotted path,
// rendered with fmt.Sprint ("10s", "5432", "[a b]", "<absent>").
func pinYAML(t *testing.T, path, key string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse written yaml: %v", err)
	}
	var cur any = doc
	for _, part := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "<absent>"
		}
		cur, ok = m[part]
		if !ok {
			return "<absent>"
		}
	}
	return fmtAny(cur)
}

func fmtAny(v any) string {
	if l, ok := v.([]any); ok {
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = fmtAny(x)
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable: " + err.Error() + ">"
	}
	return strings.Trim(string(b), `"`)
}

// configAnswer is the new door's JSON answer.
type configAnswer struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error"`
	Errors  map[string]string `json:"errors"`
	Applied []string          `json:"applied"`
	Restart []string          `json:"restart"`
	Failed  []string          `json:"failed"`
}

// configSaveCase is one posted body and what the new door does with it.
type configSaveCase struct {
	name string
	body map[string]any // {section: {...}}
	// mutate adjusts the baseline before the post (nil = baseline as is).
	mutate func(*config.Config)
	// engine adjusts the stub (ping or reconfigure errors).
	engine func(*configPinEngine)

	wantStatus  int
	wantError   string   // substring of the top-level error ("" = not checked)
	wantErrors  []string // the field-error keys, sorted (nil = none)
	wantCalls   []string // database ping / Reconfigure* calls, in order (nil = none)
	wantWritten bool     // false: the file still holds configPinSeed
	wantApplied []string // 200 only
	wantRestart []string // 200 only
	wantFailed  []string // 200 only
	// wantCfg checks the in-memory config after the post.
	wantCfg func(t *testing.T, c *config.Config)
	// wantYAML pins keys of the written file (dotted path -> value).
	wantYAML map[string]string
}

// sect builds {name: fields} with fields given as key, value pairs.
func sect(name string, kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return map[string]any{name: m}
}

// with returns a copy of a one-section body with fields overridden; a nil
// value deletes the field.
func with(body map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for name, fields := range body {
		m := map[string]any{}
		for k, v := range fields.(map[string]any) {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		out[name] = m
	}
	return out
}

func runConfigSaveCases(t *testing.T, cases []configSaveCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pinBaselineConfig()
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			h, eng := newConfigPinHandlers(t, cfg)
			if tc.engine != nil {
				tc.engine(eng)
			}
			before := cfg.Clone()

			rec := pinPut(t, h.apiConfigSave, "/api/config", tc.body)
			ans := checkConfigAnswer(t, rec, tc)

			if !reflect.DeepEqual(eng.calls, tc.wantCalls) {
				t.Errorf("engine calls: got %v, want %v", eng.calls, tc.wantCalls)
			}
			raw, err := os.ReadFile(eng.path)
			if err == nil {
				written := string(raw) != configPinSeed
				if written != tc.wantWritten {
					t.Errorf("file written: got %v, want %v", written, tc.wantWritten)
				}
			} else if tc.wantWritten {
				t.Errorf("read config file: %v", err)
			}
			if tc.wantStatus != http.StatusOK && !reflect.DeepEqual(cfg, before) {
				// One save path: any refusal leaves the live config untouched.
				t.Errorf("live config changed by a refused save")
			}
			if tc.wantStatus == http.StatusOK {
				pinEqList(t, "applied", ans.Applied, tc.wantApplied)
				pinEqList(t, "restart", ans.Restart, tc.wantRestart)
				pinEqList(t, "failed", ans.Failed, tc.wantFailed)
			}
			if tc.wantCfg != nil {
				tc.wantCfg(t, cfg)
			}
			for key, want := range tc.wantYAML {
				if got := pinYAML(t, eng.path, key); got != want {
					t.Errorf("yaml %s: got %q, want %q", key, got, want)
				}
			}
		})
	}
}

func checkConfigAnswer(t *testing.T, rec *httptest.ResponseRecorder, tc configSaveCase) configAnswer {
	t.Helper()
	if rec.Code != tc.wantStatus {
		t.Fatalf("status: got %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "" {
		t.Errorf("Location: got %q, want none (JSON door, no redirect)", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q", ct)
	}
	var ans configAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if ans.OK != (tc.wantStatus == http.StatusOK) {
		t.Errorf("ok: got %v at status %d", ans.OK, rec.Code)
	}
	if tc.wantStatus != http.StatusOK && ans.Error == "" {
		t.Errorf("a refusal must carry a top-level error (L2/L5); body=%q", rec.Body.String())
	}
	if tc.wantError != "" && !strings.Contains(ans.Error, tc.wantError) {
		t.Errorf("error: got %q, want it to contain %q", ans.Error, tc.wantError)
	}
	keys := make([]string, 0, len(ans.Errors))
	for k := range ans.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pinEqList(t, "field errors", keys, tc.wantErrors)
	return ans
}

func pinEq[T comparable](t *testing.T, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", field, got, want)
	}
}

func pinEqList(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %q, want %q", field, got, want)
	}
}

// The restart notice's labels (coreRestartFields).
const (
	rsTimezone = "Plant timezone"
	rsPoll     = "Ask for robot status every"
	rsGrace    = "Fail a faulted order after (the value Edges are offered)"
	rsOrders   = "Orders topic"
	rsDispatch = "Dispatch topic"
)

// rsDatabase: every database field applies after a restart (R27).
var rsDatabase = []string{
	"Database server", "Database name", "Database sign-in",
	"Database encryption (SSL mode)", "Database connection pool",
}

// --- database ---------------------------------------------------------------

func TestPinConfig_Database(t *testing.T) {
	full := sect("database",
		"host", "db-new.test",
		"port", "5433",
		"database", "newdb",
		"user", "newuser",
		"password", "new-pg-secret",
		"sslmode", "require",
		"max_open_conns", "42",
		"max_idle_conns", "7",
		"conn_max_lifetime", "1h0m0s",
	)
	// C3 + R27: the draft is pinged before the file is written; nothing is
	// prepared, migrated or swapped in, and every database field waits for a
	// restart.
	prepared := []string{"ping-database@unwritten"}
	saved := []string{"database"}
	restart := rsDatabase
	cases := []configSaveCase{
		{
			name: "HappyPath", body: full,
			wantStatus: http.StatusOK, wantCalls: prepared, wantWritten: true, wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pg := c.Database.Postgres
				pinEq(t, "host", pg.Host, "db-new.test")
				pinEq(t, "port", pg.Port, 5433)
				pinEq(t, "database", pg.Database, "newdb")
				pinEq(t, "user", pg.User, "newuser")
				pinEq(t, "password", pg.Password, "new-pg-secret")
				pinEq(t, "sslmode", pg.SSLMode, "require")
				pinEq(t, "max_open", pg.MaxOpenConns, 42)
				pinEq(t, "max_idle", pg.MaxIdleConns, 7)
				pinEq(t, "lifetime", pg.ConnMaxLifetime, time.Hour)
			},
			wantYAML: map[string]string{
				"database.postgres.host":              "db-new.test",
				"database.postgres.port":              "5433",
				"database.postgres.password":          "new-pg-secret",
				"database.postgres.sslmode":           "require",
				"database.postgres.max_open_conns":    "42",
				"database.postgres.max_idle_conns":    "7",
				"database.postgres.conn_max_lifetime": "1h0m0s",
			},
		},
		{
			// C2: a non-numeric port is a field error; nothing written.
			name: "NonNumericPort", body: with(full, "port", "54x2"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"pg_port"},
		},
		{
			// C4: blank keeps the saved password, today and after.
			name: "BlankPasswordKeeps", body: with(full, "password", ""),
			wantStatus: http.StatusOK, wantCalls: prepared, wantWritten: true, wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "password", c.Database.Postgres.Password, "old-pg-secret")
			},
			wantYAML: map[string]string{"database.postgres.password": "old-pg-secret"},
		},
		{
			// C4 (rule 8): the Remove control clears a saved secret.
			name: "RemoveClearsPassword", body: with(full, "password", "", "clear_password", true),
			wantStatus: http.StatusOK, wantCalls: prepared, wantWritten: true, wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "password", c.Database.Postgres.Password, "")
			},
		},
		{
			// C2: SSL mode is one of the four.
			name: "SSLModeGarbage", body: with(full, "sslmode", "banana"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"pg_sslmode"},
		},
		{
			// C2: an unparseable duration is a field error.
			name: "UnparseableLifetime", body: with(full, "conn_max_lifetime", "forever"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"pg_conn_max_lifetime"},
		},
		{
			// C2: zero fails the same > 0 guard, now visibly.
			name: "ZeroLifetime", body: with(full, "conn_max_lifetime", "0s"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"pg_conn_max_lifetime"},
		},
		{
			// C2: non-numeric and zero connection counts.
			name: "BadConnCounts", body: with(full, "max_open_conns", "lots", "max_idle_conns", "0"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"pg_max_idle_conns", "pg_max_open_conns"},
		},
		{
			// An empty section: every ruled field is a field error (C2);
			// nothing is prepared or written.
			name: "OnlySectionPosted", body: map[string]any{"database": map[string]any{}},
			wantStatus: http.StatusBadRequest,
			wantErrors: []string{"pg_conn_max_lifetime", "pg_max_idle_conns", "pg_max_open_conns", "pg_port", "pg_sslmode"},
		},
		{
			// L1: host, database and user left out are written blank, as the
			// form door did (subject to C3's ping, which the stub passes).
			name: "HostDatabaseUserAbsentStoredBlank", body: with(full, "host", nil, "database", nil, "user", nil),
			wantStatus: http.StatusOK, wantCalls: prepared, wantWritten: true, wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pg := c.Database.Postgres
				pinEq(t, "host", pg.Host, "")
				pinEq(t, "database", pg.Database, "")
				pinEq(t, "user", pg.User, "")
			},
			wantYAML: map[string]string{"database.postgres.host": ""},
		},
		{
			// C3/R2 (R27 keeps it): the ping fails (dead host): 400 with the
			// driver error; the yaml and the live config are untouched.
			name: "PingFails", body: full,
			engine: func(e *configPinEngine) {
				e.pingErr = errors.New("ping new db: dial tcp: connection refused")
			},
			wantStatus: http.StatusBadRequest, wantError: "connection refused",
			wantCalls: []string{"ping-database@unwritten"},
		},
		{
			// C3: an unchanged database section pings nothing and waits on
			// no restart.
			name: "UnchangedPingsNothing",
			body: sect("database", "host", "db-old.test", "port", 5432, "database", "olddb", "user", "olduser",
				"password", "", "sslmode", "disable", "max_open_conns", 10, "max_idle_conns", 5, "conn_max_lifetime", "30m0s"),
			wantStatus: http.StatusOK, wantWritten: true, wantApplied: saved,
		},
	}
	runConfigSaveCases(t, cases)
}

// R27: the save fails after the ping: 500, nothing else ran (there is no
// prepared pool to discard, nothing was migrated), and the live config is
// unchanged.
func TestPinConfig_DatabaseSaveFailsAfterPing(t *testing.T) {
	cfg := pinBaselineConfig()
	h, eng := newConfigPinHandlers(t, cfg)
	if err := os.Remove(eng.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eng.path, 0755); err != nil {
		t.Fatal(err)
	}
	rec := pinPut(t, h.apiConfigSave, "/api/config", sect("database", "host", "db-new.test", "port", "5433",
		"database", "newdb", "user", "u", "sslmode", "disable", "max_open_conns", 5, "max_idle_conns", 2,
		"conn_max_lifetime", "5m0s"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to save") || strings.Contains(rec.Body.String(), "migrated") {
		t.Errorf("body: got %q", rec.Body.String())
	}
	// The stub's fileState reads a directory as "written"; what matters is
	// that the ping is the only database call.
	pinEqList(t, "calls", eng.calls, []string{"ping-database@written"})
	pinEq(t, "live host", cfg.Database.Postgres.Host, "db-old.test")
}

// --- general (fleet) --------------------------------------------------------

func TestPinConfig_General(t *testing.T) {
	full := sect("fleet",
		"base_url", "http://fleet-new.test:8088",
		"poll_interval", "10s",
		"timeout", "3s",
		"fault_grace", "30m0s",
	)
	saved := []string{"fleet"}
	cases := []configSaveCase{
		{
			// C7: poll interval and fault grace are named in the restart list.
			name: "HappyPath", body: full,
			wantStatus: http.StatusOK, wantCalls: []string{"fleet"}, wantWritten: true,
			wantApplied: saved, wantRestart: []string{rsPoll, rsGrace},
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "base_url", c.RDS.BaseURL, "http://fleet-new.test:8088")
				pinEq(t, "poll", c.RDS.PollInterval, 10*time.Second)
				pinEq(t, "timeout", c.RDS.Timeout, 3*time.Second)
				pinEq(t, "grace", c.RDS.FaultGrace, 30*time.Minute)
			},
			wantYAML: map[string]string{
				"rds.base_url":      "http://fleet-new.test:8088",
				"rds.poll_interval": "10s",
				"rds.timeout":       "3s",
				"rds.fault_grace":   "30m0s",
			},
		},
		{
			// C2: unparseable durations are field errors; nothing applied.
			name: "UnparseableDurations", body: with(full, "poll_interval", "ten seconds", "timeout", "45m0", "fault_grace", "soon"),
			wantStatus: http.StatusBadRequest,
			wantErrors: []string{"fleet_fault_grace", "fleet_poll_interval", "fleet_timeout"},
		},
		{
			// C2: blank is not a duration (the page always sends the box).
			name: "BlankGraceKeeps", body: with(full, "fault_grace", ""),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"fleet_fault_grace"},
		},
		{
			// C2: a zero grace stays refused, now visibly.
			name: "ZeroGraceKeeps", body: with(full, "fault_grace", "0s"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"fleet_fault_grace"},
		},
		{
			// L1: a zero poll interval or timeout is stored as 0s, as today.
			name: "ZeroPollAndTimeoutStored", body: with(full, "poll_interval", "0s", "timeout", "0"),
			wantStatus: http.StatusOK, wantCalls: []string{"fleet"}, wantWritten: true,
			wantApplied: saved, wantRestart: []string{rsPoll, rsGrace},
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "poll", c.RDS.PollInterval, time.Duration(0))
				pinEq(t, "timeout", c.RDS.Timeout, time.Duration(0))
			},
			wantYAML: map[string]string{"rds.poll_interval": "0s", "rds.timeout": "0s"},
		},
		{
			// Unchanged: a blank base URL is stored blank.
			name: "BlankBaseURLStored", body: with(full, "base_url", ""),
			wantStatus: http.StatusOK, wantCalls: []string{"fleet"}, wantWritten: true,
			wantApplied: saved, wantRestart: []string{rsPoll, rsGrace},
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "base_url", c.RDS.BaseURL, "")
				pinEq(t, "poll", c.RDS.PollInterval, 10*time.Second)
			},
			wantYAML: map[string]string{"rds.base_url": ""},
		},
		{
			// R4: the form door's "no base URL key applies nothing" is gone
			// with it. The JSON section is the whole section: a post without
			// base_url and timeout is refused on the timeout (C2) and nothing
			// is applied or written.
			name:       "NoBaseURLKeyAppliesNothing",
			body:       sect("fleet", "poll_interval", "10s", "fault_grace", "30m0s"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"fleet_timeout"},
		},
		{
			// R4: `fleet` is the section's real name now; there is no alias.
			name: "FleetAlias", body: full,
			wantStatus: http.StatusOK, wantCalls: []string{"fleet"}, wantWritten: true,
			wantApplied: saved, wantRestart: []string{rsPoll, rsGrace},
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "poll", c.RDS.PollInterval, 10*time.Second)
			},
		},
		{
			// R4: the `general` wrapper is gone.
			name: "GeneralSectionGone", body: sect("general", "base_url", "http://x"),
			wantStatus: http.StatusBadRequest, wantError: "unknown section",
		},
	}
	runConfigSaveCases(t, cases)
}

// --- services (messaging) ---------------------------------------------------

func TestPinConfig_Services(t *testing.T) {
	topics := []any{"group_id", "new-group", "orders_topic", "new.orders", "dispatch_topic", "new.dispatch"}
	post := func(brokers ...string) map[string]any {
		return sect("messaging", append([]any{"brokers", brokers}, topics...)...)
	}
	saved := []string{"messaging"}
	restart := []string{rsOrders, rsDispatch}
	cases := []configSaveCase{
		{
			// C7: the two topics are named in the restart list.
			name:       "HappyPath",
			body:       post("kafka-a.test:9092", "kafka-b.test:9093"),
			wantStatus: http.StatusOK, wantCalls: []string{"messaging"}, wantWritten: true,
			wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-a.test:9092", "kafka-b.test:9093"})
				pinEq(t, "group", c.Messaging.Kafka.GroupID, "new-group")
				pinEq(t, "orders", c.Messaging.OrdersTopic, "new.orders")
				pinEq(t, "dispatch", c.Messaging.DispatchTopic, "new.dispatch")
			},
			wantYAML: map[string]string{
				"messaging.kafka.brokers":  "[kafka-a.test:9092 kafka-b.test:9093]",
				"messaging.kafka.group_id": "new-group",
				"messaging.orders_topic":   "new.orders",
				"messaging.dispatch_topic": "new.dispatch",
			},
		},
		{
			// C2: a broker with a blank port is a field error on its row.
			name: "BlankBrokerPort", body: post("kafka-a.test:"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"kafka_broker_0"},
		},
		{
			// C2: a non-numeric port too.
			name: "NonNumericBrokerPort", body: post("kafka-a.test:abc"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"kafka_broker_0"},
		},
		{
			// C1: the first row removed, the rest are kept.
			name:       "IndexGapRemoveFirst",
			body:       post("kafka-b.test:9092", "kafka-c.test:9092"),
			wantStatus: http.StatusOK, wantCalls: []string{"messaging"}, wantWritten: true,
			wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-b.test:9092", "kafka-c.test:9092"})
			},
			wantYAML: map[string]string{"messaging.kafka.brokers": "[kafka-b.test:9092 kafka-c.test:9092]"},
		},
		{
			// C1: a middle row removed, both others kept.
			name:       "IndexGapMiddle",
			body:       post("kafka-a.test:9092", "kafka-c.test:9092"),
			wantStatus: http.StatusOK, wantCalls: []string{"messaging"}, wantWritten: true,
			wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-a.test:9092", "kafka-c.test:9092"})
			},
		},
		{
			// L1: group and topics left out are stored blank, as today.
			name:       "TopicsAbsentStoredBlank",
			body:       sect("messaging", "brokers", []string{"kafka-a.test:9092"}),
			wantStatus: http.StatusOK, wantCalls: []string{"messaging"}, wantWritten: true,
			wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "group", c.Messaging.Kafka.GroupID, "")
				pinEq(t, "orders", c.Messaging.OrdersTopic, "")
				pinEq(t, "dispatch", c.Messaging.DispatchTopic, "")
			},
			wantYAML: map[string]string{"messaging.orders_topic": ""},
		},
		{
			// R4: `messaging` is the section's real name now.
			name:       "MessagingAlias",
			body:       post("kafka-a.test:9092"),
			wantStatus: http.StatusOK, wantCalls: []string{"messaging"}, wantWritten: true,
			wantApplied: saved, wantRestart: restart,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-a.test:9092"})
			},
		},
		{
			// R4: the `services` wrapper is gone.
			name: "ServicesSectionGone", body: sect("services", "brokers", []string{"kafka-a.test:9092"}),
			wantStatus: http.StatusBadRequest, wantError: "unknown section",
		},
		{
			// The file is written and the reconfigure fails: the answer
			// names it in `failed`, the save stands.
			name: "ReconfigureFailsIsReported",
			body: post("kafka-a.test:9092"),
			engine: func(e *configPinEngine) {
				e.reconfErr = map[string]error{"messaging": errors.New("no broker answered")}
			},
			wantStatus: http.StatusOK, wantCalls: []string{"messaging"}, wantWritten: true,
			wantApplied: saved, wantRestart: restart,
			wantFailed: []string{"messaging (no broker answered)"},
		},
	}
	runConfigSaveCases(t, cases)
}

// --- fire_alarm -------------------------------------------------------------

func TestPinConfig_FireAlarm(t *testing.T) {
	saved := []string{"fire_alarm"}
	off := func(c *config.Config) {
		c.FireAlarm.Enabled = false
		c.FireAlarm.AutoResumeDefault = false
	}
	cases := []configSaveCase{
		{
			// No Reconfigure* call for fire_alarm: the flags are read at use.
			name: "BothOn", body: sect("fire_alarm", "enabled", true, "auto_resume_default", true),
			mutate:     off,
			wantStatus: http.StatusOK, wantWritten: true, wantApplied: saved,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "enabled", c.FireAlarm.Enabled, true)
				pinEq(t, "auto_resume", c.FireAlarm.AutoResumeDefault, true)
			},
			wantYAML: map[string]string{"fire_alarm.enabled": "true", "fire_alarm.auto_resume_default": "true"},
		},
		{
			// R4 (shape only): left out is false.
			name: "AbsentIsOff", body: map[string]any{"fire_alarm": map[string]any{}},
			wantStatus: http.StatusOK, wantWritten: true, wantApplied: saved,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "enabled", c.FireAlarm.Enabled, false)
				pinEq(t, "auto_resume", c.FireAlarm.AutoResumeDefault, false)
			},
			wantYAML: map[string]string{"fire_alarm.enabled": "false", "fire_alarm.auto_resume_default": "false"},
		},
		{
			// R4: the form's "only `on` counts" is gone; the wire is JSON
			// booleans, and a string is not one: the section is refused.
			name: "TrueIsNotOn", body: sect("fire_alarm", "enabled", "true", "auto_resume_default", "1"),
			mutate:     off,
			wantStatus: http.StatusBadRequest, wantErrors: []string{"fire_alarm"},
		},
	}
	runConfigSaveCases(t, cases)
}

// --- notifications ----------------------------------------------------------

func TestPinConfig_Notifications(t *testing.T) {
	full := sect("notifications",
		"enabled", true,
		"smtp_host", "smtp-new.test",
		"smtp_port", "2525",
		"smtp_tls", true,
		"smtp_user", "new-smtp-user",
		"smtp_password", "new-smtp-secret",
		"from_address", "new-from@test.invalid",
		"throttle_minutes", "30",
		"recipients", []string{"r0@test.invalid", "r1@test.invalid"},
	)
	saved := []string{"notifications"}
	ok := func(c configSaveCase) configSaveCase {
		c.wantStatus, c.wantCalls, c.wantWritten, c.wantApplied = http.StatusOK, []string{"notifications"}, true, saved
		return c
	}
	cases := []configSaveCase{
		ok(configSaveCase{
			name: "HappyPath", body: full,
			wantCfg: func(t *testing.T, c *config.Config) {
				n := c.Notifications
				pinEq(t, "enabled", n.Enabled, true)
				pinEq(t, "host", n.SMTPHost, "smtp-new.test")
				pinEq(t, "port", n.SMTPPort, 2525)
				pinEq(t, "tls", n.SMTPTLS, true)
				pinEq(t, "user", n.SMTPUser, "new-smtp-user")
				pinEq(t, "password", n.SMTPPassword, "new-smtp-secret")
				pinEq(t, "from", n.FromAddress, "new-from@test.invalid")
				pinEq(t, "throttle", n.ThrottleMinutes, 30)
				pinEqList(t, "recipients", n.Recipients, []string{"r0@test.invalid", "r1@test.invalid"})
			},
			wantYAML: map[string]string{
				"notifications.enabled":          "true",
				"notifications.smtp_host":        "smtp-new.test",
				"notifications.smtp_port":        "2525",
				"notifications.smtp_password":    "new-smtp-secret",
				"notifications.throttle_minutes": "30",
				"notifications.recipients":       "[r0@test.invalid r1@test.invalid]",
			},
		}),
		ok(configSaveCase{
			// C4: blank now KEEPS the saved SMTP password (it erased it).
			name: "BlankSMTPPasswordErases", body: with(full, "smtp_password", ""),
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "password", c.Notifications.SMTPPassword, "old-smtp-secret")
			},
			wantYAML: map[string]string{"notifications.smtp_password": "old-smtp-secret"},
		}),
		ok(configSaveCase{
			// C4 (rule 8): the Remove control clears it.
			name: "RemoveClearsSMTPPassword", body: with(full, "smtp_password", "", "clear_smtp_password", true),
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "password", c.Notifications.SMTPPassword, "")
			},
		}),
		{
			// C2: a non-numeric SMTP port is a field error.
			name: "NonNumericSMTPPort", body: with(full, "smtp_port", "smtp"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"notif_smtp_port"},
		},
		{
			// C2: a zero throttle is a field error.
			name: "BadThrottle", body: with(full, "throttle_minutes", "0"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"notif_throttle_minutes"},
		},
		ok(configSaveCase{
			// C1: the first row removed, the rest are kept.
			name: "IndexGapRemoveFirst", body: with(full, "recipients", []string{"r1@test.invalid", "r2@test.invalid"}),
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "recipients", c.Notifications.Recipients, []string{"r1@test.invalid", "r2@test.invalid"})
			},
			wantYAML: map[string]string{"notifications.recipients": "[r1@test.invalid r2@test.invalid]"},
		}),
		ok(configSaveCase{
			// R4 (shape only): JSON booleans; left out is false.
			name: "CheckboxesAbsentAreOff", body: with(full, "enabled", nil, "smtp_tls", false),
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "enabled", c.Notifications.Enabled, false)
				pinEq(t, "tls", c.Notifications.SMTPTLS, false)
			},
			wantYAML: map[string]string{"notifications.enabled": "false", "notifications.smtp_tls": "false"},
		}),
	}
	runConfigSaveCases(t, cases)
}

// --- plant (C9; new, nothing to pin at 5c0beb74) ----------------------------

func TestConfigSave_Plant(t *testing.T) {
	runConfigSaveCases(t, []configSaveCase{
		{
			// C9: a known zone is saved; restart-only, so it is in the
			// notice; no subsystem is reconfigured.
			name: "KnownZone", body: sect("plant", "timezone", "America/Chicago"),
			wantStatus: http.StatusOK, wantWritten: true,
			wantApplied: []string{"plant"}, wantRestart: []string{rsTimezone},
			wantCfg:  func(t *testing.T, c *config.Config) { pinEq(t, "tz", c.Timezone, "America/Chicago") },
			wantYAML: map[string]string{"timezone": "America/Chicago"},
		},
		{
			// C9: time.LoadLocation refuses it before the save.
			name: "UnknownZone", body: sect("plant", "timezone", "Mars/Olympus"),
			wantStatus: http.StatusBadRequest, wantErrors: []string{"timezone"},
		},
		{
			// Blank is unset (Core shows UTC, tells its Edges nothing).
			name:       "BlankIsUnset",
			body:       sect("plant", "timezone", ""),
			mutate:     func(c *config.Config) { c.Timezone = "America/Chicago" },
			wantStatus: http.StatusOK, wantWritten: true,
			wantApplied: []string{"plant"}, wantRestart: []string{rsTimezone},
			wantCfg: func(t *testing.T, c *config.Config) { pinEq(t, "tz", c.Timezone, "") },
		},
	})
}

// Two sections in one save: both apply, in page order, with one write.
func TestConfigSave_TwoSectionsOneWrite(t *testing.T) {
	runConfigSaveCases(t, []configSaveCase{{
		name: "FleetAndFireAlarm",
		body: map[string]any{
			"fire_alarm": map[string]any{"enabled": false, "auto_resume_default": false},
			"fleet":      map[string]any{"base_url": "http://fleet-old.test:8088", "poll_interval": "5s", "timeout": "4s", "fault_grace": "45m0s"},
		},
		wantStatus: http.StatusOK, wantCalls: []string{"fleet"}, wantWritten: true,
		wantApplied: []string{"fleet", "fire_alarm"},
		wantCfg: func(t *testing.T, c *config.Config) {
			pinEq(t, "timeout", c.RDS.Timeout, 4*time.Second)
			pinEq(t, "fire alarm", c.FireAlarm.Enabled, false)
		},
	}, {
		// A field error in one section refuses the whole save.
		name: "OneBadSectionRefusesBoth",
		body: map[string]any{
			"fire_alarm": map[string]any{"enabled": false},
			"fleet":      map[string]any{"base_url": "x", "poll_interval": "5s", "timeout": "nope", "fault_grace": "45m0s"},
		},
		wantStatus: http.StatusBadRequest, wantErrors: []string{"fleet_timeout"},
	}})
}

// --- door-level -------------------------------------------------------------

func TestPinConfig_UnknownSection(t *testing.T) {
	runConfigSaveCases(t, []configSaveCase{
		{
			// R4: the words `unknown section` stay (L2).
			name: "NoSuchSection", body: sect("no-such", "host", "x"),
			wantStatus: http.StatusBadRequest, wantError: "unknown section",
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "host", c.Database.Postgres.Host, "db-old.test")
			},
		},
		{
			name: "NoSection", body: map[string]any{},
			wantStatus: http.StatusBadRequest, wantError: "unknown section",
		},
	})
}

// "One save path": a failed Save answers 500 and the live config is NOT
// mutated (copy, validate, write, then swap). At 5c0beb74 it already held the
// posted values.
func TestPinConfig_SaveFailsLiveConfigAlreadyMutated(t *testing.T) {
	cfg := pinBaselineConfig()
	h, eng := newConfigPinHandlers(t, cfg)
	if err := os.Remove(eng.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eng.path, 0755); err != nil {
		t.Fatal(err)
	}

	rec := pinPut(t, h.apiConfigSave, "/api/config",
		sect("fleet", "base_url", "http://fleet-new.test:8088", "poll_interval", "10s", "timeout", "10s", "fault_grace", "45m0s"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	var ans configAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if !strings.Contains(ans.Error, "Failed to save") {
		t.Errorf("error: got %q", ans.Error)
	}
	if len(eng.calls) != 0 {
		t.Errorf("reconfigure calls: got %v, want none", eng.calls)
	}
	pinEq(t, "live base_url", cfg.RDS.BaseURL, "http://fleet-old.test:8088")
	pinEq(t, "live poll", cfg.RDS.PollInterval, 5*time.Second)
}

// "One save path": two people saving different sections at once both land,
// and the live config ends as the file does.
func TestConfigSave_ConcurrentSectionsBothLand(t *testing.T) {
	cfg := pinBaselineConfig()
	h, eng := newConfigPinHandlers(t, cfg)
	done := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 20; j++ {
				body := sect("fire_alarm", "enabled", false, "auto_resume_default", false)
				if i == 1 {
					body = sect("plant", "timezone", "America/Chicago")
				}
				if rec := pinPut(t, h.apiConfigSave, "/api/config", body); rec.Code != http.StatusOK {
					t.Errorf("save %d/%d: %d %s", i, j, rec.Code, rec.Body.String())
				}
			}
		}(i)
	}
	<-done
	<-done
	_ = eng
	pinEq(t, "fire alarm", cfg.FireAlarm.Enabled, false)
	pinEq(t, "timezone", cfg.Timezone, "America/Chicago")
	pinEq(t, "yaml fire alarm", pinYAML(t, eng.path, "fire_alarm.enabled"), "false")
	pinEq(t, "yaml timezone", pinYAML(t, eng.path, "timezone"), "America/Chicago")
}

// The restart notice: no boot snapshot, no notice.
func TestConfigSave_NoSnapshotNoNotice(t *testing.T) {
	cfg := pinBaselineConfig()
	h, _ := newConfigPinHandlers(t, cfg)
	h.boot = nil
	rec := pinPut(t, h.apiConfigSave, "/api/config", sect("plant", "timezone", "America/Chicago"))
	var ans configAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if ans.Restart == nil || len(ans.Restart) != 0 {
		t.Errorf("restart: got %#v, want an empty list", ans.Restart)
	}
}

// --- test-database (C3's Test connection) -----------------------------------

func TestConfigTestDatabase(t *testing.T) {
	// A loopback port nothing listens on: the dial is refused at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	body := map[string]any{"host": "127.0.0.1", "port": dead, "database": "x", "user": "x", "password": "",
		"sslmode": "disable", "max_open_conns": 2, "max_idle_conns": 1, "conn_max_lifetime": "1m0s"}

	t.Run("DeadHostAnswersNotOK", func(t *testing.T) {
		cfg := pinBaselineConfig()
		h, eng := newConfigPinHandlers(t, cfg)
		rec := pinRequest(t, http.MethodPost, h.apiConfigTestDatabase, "/api/config/test-database", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
		var ans struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &ans)
		if ans.OK || ans.Error == "" {
			t.Errorf("answer: %q", rec.Body.String())
		}
		// Never saves, never prepares or migrates.
		raw, err := os.ReadFile(eng.path)
		testutil.MustNoErr(t, err, "read config file")
		if string(raw) != configPinSeed {
			t.Errorf("config file written by the test door")
		}
		if len(eng.calls) != 0 {
			t.Errorf("engine calls: %v", eng.calls)
		}
		pinEq(t, "live host", cfg.Database.Postgres.Host, "db-old.test")
	})
	t.Run("BadDraftIsFieldError", func(t *testing.T) {
		h, _ := newConfigPinHandlers(t, pinBaselineConfig())
		bad := map[string]any{}
		for k, v := range body {
			bad[k] = v
		}
		bad["port"] = "54x2"
		rec := pinRequest(t, http.MethodPost, h.apiConfigTestDatabase, "/api/config/test-database", bad)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "pg_port") {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
	})
}

// --- test-email / test-alert preconditions ----------------------------------

// closingSMTP listens on loopback and closes every connection at once, so a
// send that passes the preconditions fails fast at the SMTP greeting.
func closingSMTP(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

// notifDraft is the page's notifications section for n, password blank (the
// saved one is used).
func notifDraft(n config.NotificationsConfig) map[string]any {
	return map[string]any{
		"enabled": n.Enabled, "smtp_host": n.SMTPHost, "smtp_port": n.SMTPPort, "smtp_tls": n.SMTPTLS,
		"smtp_user": n.SMTPUser, "smtp_password": "", "from_address": n.FromAddress,
		"recipients": n.Recipients, "throttle_minutes": n.ThrottleMinutes,
	}
}

type testSendCase struct {
	name  string
	query string // test-alert only
	// mutate adjusts the SAVED config; draft adjusts the POSTED draft, which
	// starts as the saved config's (after mutate). C5: the doors use the draft.
	mutate  func(c *config.Config, host string, port int)
	draft   func(d map[string]any, host string, port int)
	status  int
	ok      bool
	message string // substring
	json    bool   // false: plain http.Error body
}

func runTestSendCases(t *testing.T, handler func(*Handlers) http.HandlerFunc, path string, cases []testSendCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := closingSMTP(t)
			cfg := pinBaselineConfig()
			// Baseline points at the closing listener; the case may unset it.
			cfg.Notifications.SMTPHost = host
			cfg.Notifications.SMTPPort = port
			cfg.Notifications.SMTPTLS = false
			cfg.Notifications.SMTPUser = ""
			if tc.mutate != nil {
				tc.mutate(cfg, host, port)
			}
			draft := notifDraft(cfg.Notifications)
			if tc.draft != nil {
				tc.draft(draft, host, port)
			}
			h, eng := newConfigPinHandlers(t, cfg)
			before := cfg.Clone()

			rec := pinRequest(t, http.MethodPost, handler(h), path+tc.query, draft)

			if rec.Code != tc.status {
				t.Fatalf("status: got %d, want %d; body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.json {
				var resp struct {
					OK      bool   `json:"ok"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode %q: %v", rec.Body.String(), err)
				}
				if resp.OK != tc.ok {
					t.Errorf("ok: got %v, want %v", resp.OK, tc.ok)
				}
				if !strings.Contains(resp.Message, tc.message) {
					t.Errorf("message: got %q, want it to contain %q", resp.Message, tc.message)
				}
			} else if !strings.Contains(rec.Body.String(), tc.message) {
				t.Errorf("body: got %q, want it to contain %q", rec.Body.String(), tc.message)
			}
			// Neither door writes config, reloads anything, or touches the
			// live config (the draft is not saved).
			raw, err := os.ReadFile(eng.path)
			testutil.MustNoErr(t, err, "read config file")
			if string(raw) != configPinSeed {
				t.Errorf("config file written by a test door")
			}
			if len(eng.calls) != 0 {
				t.Errorf("reconfigure calls: got %v", eng.calls)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Errorf("live config changed by a test door")
			}
		})
	}
}

func TestPinConfig_TestEmail(t *testing.T) {
	missing := "SMTP host, from address, and at least one recipient are required"
	runTestSendCases(t, func(h *Handlers) http.HandlerFunc { return h.handleConfigTestEmail }, "/config/test-email", []testSendCase{
		{
			// C5: Enabled is not required (unchanged); the send is attempted
			// with the draft and its failure is a 200 with ok:false.
			name:   "DisabledStillSends",
			draft:  func(d map[string]any, _ string, _ int) { d["enabled"] = false },
			status: http.StatusOK, ok: false, message: "smtp client", json: true,
		},
		{
			// C5: the SMTP-fields check runs against the posted draft.
			name:   "BlankHost",
			draft:  func(d map[string]any, _ string, _ int) { d["smtp_host"] = "" },
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			name:   "BlankFrom",
			draft:  func(d map[string]any, _ string, _ int) { d["from_address"] = "" },
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			name:   "NoRecipients",
			draft:  func(d map[string]any, _ string, _ int) { d["recipients"] = []string{} },
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			// C5: the draft is used. The SAVED host is blank; the posted
			// draft has one, so this case sends.
			name:   "PostedDraftIgnored",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.SMTPHost = "" },
			draft:  func(d map[string]any, host string, _ int) { d["smtp_host"] = host },
			status: http.StatusOK, ok: false, message: "smtp client", json: true,
		},
		{
			// C5: the draft is validated like a save.
			name:   "BadDraftPort",
			draft:  func(d map[string]any, _ string, _ int) { d["smtp_port"] = "smtp" },
			status: http.StatusBadRequest, ok: false, message: "port", json: true,
		},
	})
}

func TestPinConfig_TestAlert(t *testing.T) {
	missing := "SMTP host, from address, and at least one recipient are required"
	runTestSendCases(t, func(h *Handlers) http.HandlerFunc { return h.handleConfigTestAlert }, "/config/test-alert", []testSendCase{
		{
			name: "BadType", query: "?type=bogus",
			status: http.StatusBadRequest, message: "type must be fault, fail, cleared, or chain",
		},
		{
			name: "NoType", query: "",
			status: http.StatusBadRequest, message: "type must be fault, fail, cleared, or chain",
		},
		{
			// C5: Enabled is no longer required; it sends with the draft.
			name: "DisabledRefused", query: "?type=fault",
			draft:  func(d map[string]any, _ string, _ int) { d["enabled"] = false },
			status: http.StatusOK, ok: false, message: "smtp client", json: true,
		},
		{
			// C5: with Enabled not checked, the draft's SMTP fields answer.
			name: "DisabledAndBlankHost", query: "?type=fault",
			draft: func(d map[string]any, _ string, _ int) {
				d["enabled"] = false
				d["smtp_host"] = ""
			},
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			name: "BlankHost", query: "?type=fail",
			draft:  func(d map[string]any, _ string, _ int) { d["smtp_host"] = "" },
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			name: "SendFails", query: "?type=cleared",
			status: http.StatusOK, ok: false, message: "smtp client", json: true,
		},
		{
			// Chain: the first send fails, so no 2 s sleep and no second send.
			name: "ChainFirstSendFails", query: "?type=chain",
			status: http.StatusOK, ok: false, message: "Fault email failed: smtp client", json: true,
		},
		{
			// C5: the draft is used: saved Enabled off, posted on, it sends.
			name: "PostedDraftIgnored", query: "?type=fault",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.Enabled = false },
			draft:  func(d map[string]any, _ string, _ int) { d["enabled"] = true },
			status: http.StatusOK, ok: false, message: "smtp client", json: true,
		},
	})
}

func pinPut(t *testing.T, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return pinRequest(t, http.MethodPut, handler, path, body)
}

func pinRequest(t *testing.T, method string, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}
