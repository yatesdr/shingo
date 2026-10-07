package www

// U0 pins for Core's config save door at 5c0beb74 (BRIEF-lead-ui-config-and-
// cleanup-2026-10-07, Part 1 U0). They pin TODAY's behaviour, bugs included:
// every case here that U2 moves is listed with its label (C1-C9, R4) in the
// evidence folder's predictions/u0-core.md. A pin that moves without a label
// there is a STOP, not a fix.
//
// No database: the engine is a stub that answers AppConfig/ConfigPath and
// records the Reconfigure* calls, so each case can say which subsystem the
// save would have reloaded without the real engine reconnecting anything. The
// password door needs the admin store and is pinned in
// handlers_config_pins_docker_test.go.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"shingocore/config"
)

// configPinEngine is an EngineOrchestration whose only working methods are the
// ones the config doors reach: AppConfig, ConfigPath and the four
// Reconfigure* verbs, which it records instead of running. Anything else
// panics on the nil embedded interface.
type configPinEngine struct {
	EngineOrchestration
	cfg   *config.Config
	path  string
	calls []string
}

func (e *configPinEngine) AppConfig() *config.Config { return e.cfg }
func (e *configPinEngine) ConfigPath() string        { return e.path }
func (e *configPinEngine) ReconfigureDatabase()      { e.calls = append(e.calls, "database") }
func (e *configPinEngine) ReconfigureFleet()         { e.calls = append(e.calls, "fleet") }
func (e *configPinEngine) ReconfigureMessaging()     { e.calls = append(e.calls, "messaging") }
func (e *configPinEngine) ReconfigureNotifications() { e.calls = append(e.calls, "notifications") }

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

func newConfigPinHandlers(t *testing.T, cfg *config.Config) (*Handlers, *configPinEngine) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(configPinSeed), 0644); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	e := &configPinEngine{cfg: cfg, path: path}
	return &Handlers{engine: e, orchestration: e}, e
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
	return fmt.Sprint(cur)
}

// configSaveCase is one posted form and what today's door does with it.
type configSaveCase struct {
	name string
	form url.Values
	// mutate adjusts the baseline before the post (nil = baseline as is).
	mutate func(*config.Config)

	wantStatus   int
	wantLocation string   // "" = no Location header
	wantBody     string   // substring of the body ("" = not checked)
	wantCalls    []string // Reconfigure* calls, in order (nil = none)
	wantWritten  bool     // false: the file still holds configPinSeed
	// wantCfg checks the in-memory config after the post.
	wantCfg func(t *testing.T, c *config.Config)
	// wantYAML pins keys of the written file (dotted path -> fmt.Sprint value).
	wantYAML map[string]string
}

func pinForm(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
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

			rec := pinPost(t, h.handleConfigSave, "/config/save", tc.form)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Location"); got != tc.wantLocation {
				t.Errorf("Location: got %q, want %q", got, tc.wantLocation)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body: got %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if !reflect.DeepEqual(eng.calls, tc.wantCalls) {
				t.Errorf("reconfigure calls: got %v, want %v", eng.calls, tc.wantCalls)
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

// --- database ---------------------------------------------------------------

func TestPinConfig_Database(t *testing.T) {
	full := func(over ...string) url.Values {
		v := pinForm(
			"section", "database",
			"pg_host", "db-new.test",
			"pg_port", "5433",
			"pg_database", "newdb",
			"pg_user", "newuser",
			"pg_password", "new-pg-secret",
			"pg_sslmode", "require",
			"pg_max_open_conns", "42",
			"pg_max_idle_conns", "7",
			"pg_conn_max_lifetime", "1h",
		)
		for i := 0; i+1 < len(over); i += 2 {
			v.Set(over[i], over[i+1])
		}
		return v
	}
	saved := "/config?saved=database"
	cases := []configSaveCase{
		{
			name: "HappyPath", form: full(),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
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
			// C2: a non-numeric port is dropped and the redirect says saved.
			name: "NonNumericPort", form: full("pg_port", "54x2"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "port", c.Database.Postgres.Port, 5432)
				pinEq(t, "host", c.Database.Postgres.Host, "db-new.test")
			},
			wantYAML: map[string]string{"database.postgres.port": "5432"},
		},
		{
			// C4: blank keeps the saved password, today and after.
			name: "BlankPasswordKeeps", form: full("pg_password", ""),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "password", c.Database.Postgres.Password, "old-pg-secret")
			},
			wantYAML: map[string]string{"database.postgres.password": "old-pg-secret"},
		},
		{
			// C2: SSL mode is stored unchecked.
			name: "SSLModeGarbage", form: full("pg_sslmode", "banana"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "sslmode", c.Database.Postgres.SSLMode, "banana")
			},
			wantYAML: map[string]string{"database.postgres.sslmode": "banana"},
		},
		{
			// C2: an unparseable duration is dropped silently.
			name: "UnparseableLifetime", form: full("pg_conn_max_lifetime", "forever"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "lifetime", c.Database.Postgres.ConnMaxLifetime, 30*time.Minute)
			},
			wantYAML: map[string]string{"database.postgres.conn_max_lifetime": "30m0s"},
		},
		{
			// C2: zero/negative lifetime is dropped too (guard d > 0).
			name: "ZeroLifetime", form: full("pg_conn_max_lifetime", "0s"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "lifetime", c.Database.Postgres.ConnMaxLifetime, 30*time.Minute)
			},
		},
		{
			// C2: connection counts, non-numeric and zero, are dropped.
			name: "BadConnCounts", form: full("pg_max_open_conns", "lots", "pg_max_idle_conns", "0"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "max_open", c.Database.Postgres.MaxOpenConns, 10)
				pinEq(t, "max_idle", c.Database.Postgres.MaxIdleConns, 5)
			},
			wantYAML: map[string]string{
				"database.postgres.max_open_conns": "10",
				"database.postgres.max_idle_conns": "5",
			},
		},
		{
			// Fields absent from the post are blanked (FormValue == ""): host,
			// database, user and sslmode. Port, password, counts and lifetime
			// keep their old values through their guards.
			name: "OnlySectionPosted", form: pinForm("section", "database"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"database"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pg := c.Database.Postgres
				pinEq(t, "host", pg.Host, "")
				pinEq(t, "database", pg.Database, "")
				pinEq(t, "user", pg.User, "")
				pinEq(t, "sslmode", pg.SSLMode, "")
				pinEq(t, "port", pg.Port, 5432)
				pinEq(t, "password", pg.Password, "old-pg-secret")
			},
			wantYAML: map[string]string{"database.postgres.host": "", "database.postgres.sslmode": ""},
		},
	}
	runConfigSaveCases(t, cases)
}

// --- general (fleet) --------------------------------------------------------

func TestPinConfig_General(t *testing.T) {
	full := func(over ...string) url.Values {
		v := pinForm(
			"section", "general",
			"fleet_base_url", "http://fleet-new.test:8088",
			"fleet_poll_interval", "10s",
			"fleet_timeout", "3s",
			"fleet_fault_grace", "30m",
		)
		for i := 0; i+1 < len(over); i += 2 {
			v.Set(over[i], over[i+1])
		}
		return v
	}
	saved := "/config?saved=general"
	cases := []configSaveCase{
		{
			name: "HappyPath", form: full(),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"fleet"}, wantWritten: true,
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
			// C2: unparseable durations are dropped, the redirect says saved.
			name: "UnparseableDurations", form: full("fleet_poll_interval", "ten seconds", "fleet_timeout", "45m0", "fleet_fault_grace", "soon"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"fleet"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "poll", c.RDS.PollInterval, 5*time.Second)
				pinEq(t, "timeout", c.RDS.Timeout, 10*time.Second)
				pinEq(t, "grace", c.RDS.FaultGrace, 45*time.Minute)
				pinEq(t, "base_url", c.RDS.BaseURL, "http://fleet-new.test:8088")
			},
			wantYAML: map[string]string{
				"rds.poll_interval": "5s",
				"rds.timeout":       "10s",
				"rds.fault_grace":   "45m0s",
			},
		},
		{
			// Blank grace keeps the old one (the d > 0 guard).
			name: "BlankGraceKeeps", form: full("fleet_fault_grace", ""),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"fleet"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "grace", c.RDS.FaultGrace, 45*time.Minute)
			},
			wantYAML: map[string]string{"rds.fault_grace": "45m0s"},
		},
		{
			name: "ZeroGraceKeeps", form: full("fleet_fault_grace", "0s"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"fleet"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "grace", c.RDS.FaultGrace, 45*time.Minute)
			},
		},
		{
			// Unlike grace, a zero poll interval or timeout IS stored.
			name: "ZeroPollAndTimeoutStored", form: full("fleet_poll_interval", "0s", "fleet_timeout", "0"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"fleet"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "poll", c.RDS.PollInterval, time.Duration(0))
				pinEq(t, "timeout", c.RDS.Timeout, time.Duration(0))
			},
			wantYAML: map[string]string{"rds.poll_interval": "0s", "rds.timeout": "0s"},
		},
		{
			// A present-but-blank base URL is stored blank.
			name: "BlankBaseURLStored", form: full("fleet_base_url", ""),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"fleet"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "base_url", c.RDS.BaseURL, "")
				pinEq(t, "poll", c.RDS.PollInterval, 10*time.Second)
			},
			wantYAML: map[string]string{"rds.base_url": ""},
		},
		{
			// No fleet_base_url key at all: nothing in the section applies,
			// yet the file is written, ReconfigureFleet fires, and the
			// redirect says saved.
			name:       "NoBaseURLKeyAppliesNothing",
			form:       pinForm("section", "general", "fleet_poll_interval", "10s", "fleet_fault_grace", "30m"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"fleet"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "base_url", c.RDS.BaseURL, "http://fleet-old.test:8088")
				pinEq(t, "poll", c.RDS.PollInterval, 5*time.Second)
				pinEq(t, "grace", c.RDS.FaultGrace, 45*time.Minute)
			},
		},
		{
			// R4: the "fleet" alias, same apply, its own redirect.
			name: "FleetAlias", form: full("section", "fleet"),
			wantStatus: http.StatusSeeOther, wantLocation: "/config?saved=fleet",
			wantCalls: []string{"fleet"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "poll", c.RDS.PollInterval, 10*time.Second)
			},
		},
	}
	runConfigSaveCases(t, cases)
}

// --- services (messaging) ---------------------------------------------------

func TestPinConfig_Services(t *testing.T) {
	topics := []string{"group_id", "new-group", "orders_topic", "new.orders", "dispatch_topic", "new.dispatch"}
	post := func(kv ...string) url.Values {
		return pinForm(append(append([]string{"section", "services"}, topics...), kv...)...)
	}
	saved := "/config?saved=services"
	cases := []configSaveCase{
		{
			name: "HappyPath",
			form: post("kafka_host_0", "kafka-a.test", "kafka_port_0", "9092",
				"kafka_host_1", "kafka-b.test", "kafka_port_1", "9093"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"messaging"}, wantWritten: true,
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
			// C2: a blank broker port becomes 9093.
			name: "BlankBrokerPort", form: post("kafka_host_0", "kafka-a.test", "kafka_port_0", ""),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"messaging"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-a.test:9093"})
			},
			wantYAML: map[string]string{"messaging.kafka.brokers": "[kafka-a.test:9093]"},
		},
		{
			// C2: a non-numeric broker port is stored as host:abc.
			name: "NonNumericBrokerPort", form: post("kafka_host_0", "kafka-a.test", "kafka_port_0", "abc"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"messaging"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-a.test:abc"})
			},
			wantYAML: map[string]string{"messaging.kafka.brokers": "[kafka-a.test:abc]"},
		},
		{
			// C1: row 0 removed, rows 1 and 2 posted -> the loop stops at the
			// missing index 0 and the saved list is empty.
			name: "IndexGapRemoveFirst",
			form: post("kafka_host_1", "kafka-b.test", "kafka_port_1", "9092",
				"kafka_host_2", "kafka-c.test", "kafka_port_2", "9092"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"messaging"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, nil)
			},
			wantYAML: map[string]string{"messaging.kafka.brokers": "[]"},
		},
		{
			// C1: a gap in the middle truncates there.
			name: "IndexGapMiddle",
			form: post("kafka_host_0", "kafka-a.test", "kafka_port_0", "9092",
				"kafka_host_2", "kafka-c.test", "kafka_port_2", "9092"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"messaging"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-a.test:9092"})
			},
		},
		{
			// Group and topics absent from the post are stored blank.
			name:       "TopicsAbsentStoredBlank",
			form:       pinForm("section", "services", "kafka_host_0", "kafka-a.test", "kafka_port_0", "9092"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"messaging"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "group", c.Messaging.Kafka.GroupID, "")
				pinEq(t, "orders", c.Messaging.OrdersTopic, "")
				pinEq(t, "dispatch", c.Messaging.DispatchTopic, "")
			},
			wantYAML: map[string]string{"messaging.orders_topic": ""},
		},
		{
			// R4: the "messaging" alias, same apply, its own redirect.
			name:       "MessagingAlias",
			form:       pinForm(append(append([]string{"section", "messaging"}, topics...), "kafka_host_0", "kafka-a.test", "kafka_port_0", "9092")...),
			wantStatus: http.StatusSeeOther, wantLocation: "/config?saved=messaging",
			wantCalls: []string{"messaging"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "brokers", c.Messaging.Kafka.Brokers, []string{"kafka-a.test:9092"})
			},
		},
	}
	runConfigSaveCases(t, cases)
}

// --- fire_alarm -------------------------------------------------------------

func TestPinConfig_FireAlarm(t *testing.T) {
	saved := "/config?saved=fire_alarm"
	cases := []configSaveCase{
		{
			// No Reconfigure* call for fire_alarm: the flags are read at use.
			name: "BothOn", form: pinForm("section", "fire_alarm", "fa_enabled", "on", "fa_auto_resume", "on"),
			mutate: func(c *config.Config) {
				c.FireAlarm.Enabled = false
				c.FireAlarm.AutoResumeDefault = false
			},
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: nil, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "enabled", c.FireAlarm.Enabled, true)
				pinEq(t, "auto_resume", c.FireAlarm.AutoResumeDefault, true)
			},
			wantYAML: map[string]string{"fire_alarm.enabled": "true", "fire_alarm.auto_resume_default": "true"},
		},
		{
			name: "AbsentIsOff", form: pinForm("section", "fire_alarm"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: nil, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "enabled", c.FireAlarm.Enabled, false)
				pinEq(t, "auto_resume", c.FireAlarm.AutoResumeDefault, false)
			},
			wantYAML: map[string]string{"fire_alarm.enabled": "false", "fire_alarm.auto_resume_default": "false"},
		},
		{
			// Only the checkbox value "on" counts.
			name: "TrueIsNotOn", form: pinForm("section", "fire_alarm", "fa_enabled", "true", "fa_auto_resume", "1"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: nil, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "enabled", c.FireAlarm.Enabled, false)
				pinEq(t, "auto_resume", c.FireAlarm.AutoResumeDefault, false)
			},
		},
	}
	runConfigSaveCases(t, cases)
}

// --- notifications ----------------------------------------------------------

func TestPinConfig_Notifications(t *testing.T) {
	full := func(over ...string) url.Values {
		v := pinForm(
			"section", "notifications",
			"notif_enabled", "on",
			"notif_smtp_host", "smtp-new.test",
			"notif_smtp_port", "2525",
			"notif_smtp_tls", "on",
			"notif_smtp_user", "new-smtp-user",
			"notif_smtp_password", "new-smtp-secret",
			"notif_from_address", "new-from@test.invalid",
			"notif_throttle_minutes", "30",
			"notif_recipient_0", "r0@test.invalid",
			"notif_recipient_1", "r1@test.invalid",
		)
		for i := 0; i+1 < len(over); i += 2 {
			v.Set(over[i], over[i+1])
		}
		return v
	}
	saved := "/config?saved=notifications"
	cases := []configSaveCase{
		{
			name: "HappyPath", form: full(),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"notifications"}, wantWritten: true,
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
		},
		{
			// C4: a blank SMTP password ERASES the saved one (unlike pg).
			name: "BlankSMTPPasswordErases", form: full("notif_smtp_password", ""),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"notifications"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "password", c.Notifications.SMTPPassword, "")
			},
			wantYAML: map[string]string{"notifications.smtp_password": ""},
		},
		{
			// C2: a non-numeric SMTP port is dropped.
			name: "NonNumericSMTPPort", form: full("notif_smtp_port", "smtp"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"notifications"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "port", c.Notifications.SMTPPort, 587)
			},
			wantYAML: map[string]string{"notifications.smtp_port": "587"},
		},
		{
			// C2: an unparseable or zero throttle is dropped.
			name: "BadThrottle", form: full("notif_throttle_minutes", "0"),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"notifications"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "throttle", c.Notifications.ThrottleMinutes, 15)
			},
			wantYAML: map[string]string{"notifications.throttle_minutes": "15"},
		},
		{
			// C1: row 0 removed -> the saved recipient list is empty.
			name: "IndexGapRemoveFirst",
			form: func() url.Values {
				v := full()
				v.Del("notif_recipient_0")
				v.Set("notif_recipient_2", "r2@test.invalid")
				return v
			}(),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"notifications"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEqList(t, "recipients", c.Notifications.Recipients, nil)
			},
			wantYAML: map[string]string{"notifications.recipients": "[]"},
		},
		{
			// Checkboxes absent are off; a posted "true" is not "on".
			name: "CheckboxesAbsentAreOff",
			form: func() url.Values {
				v := full()
				v.Del("notif_enabled")
				v.Set("notif_smtp_tls", "true")
				return v
			}(),
			wantStatus: http.StatusSeeOther, wantLocation: saved,
			wantCalls: []string{"notifications"}, wantWritten: true,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "enabled", c.Notifications.Enabled, false)
				pinEq(t, "tls", c.Notifications.SMTPTLS, false)
			},
			wantYAML: map[string]string{"notifications.enabled": "false", "notifications.smtp_tls": "false"},
		},
	}
	runConfigSaveCases(t, cases)
}

// --- door-level -------------------------------------------------------------

func TestPinConfig_UnknownSection(t *testing.T) {
	runConfigSaveCases(t, []configSaveCase{
		{
			name: "NoSuchSection", form: pinForm("section", "no-such", "pg_host", "x"),
			wantStatus: http.StatusBadRequest, wantBody: "unknown section",
			wantCalls: nil, wantWritten: false,
			wantCfg: func(t *testing.T, c *config.Config) {
				pinEq(t, "host", c.Database.Postgres.Host, "db-old.test")
			},
		},
		{
			name: "NoSection", form: pinForm("pg_host", "x"),
			wantStatus: http.StatusBadRequest, wantBody: "unknown section",
			wantCalls: nil, wantWritten: false,
		},
	})
}

// A failed Save answers 500 with no reconfigure call, but the live config has
// already been mutated: the apply ran before the write.
func TestPinConfig_SaveFailsLiveConfigAlreadyMutated(t *testing.T) {
	cfg := pinBaselineConfig()
	h, eng := newConfigPinHandlers(t, cfg)
	if err := os.Remove(eng.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eng.path, 0755); err != nil {
		t.Fatal(err)
	}

	rec := pinPost(t, h.handleConfigSave, "/config/save",
		pinForm("section", "general", "fleet_base_url", "http://fleet-new.test:8088", "fleet_poll_interval", "10s"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Failed to save") {
		t.Errorf("body: got %q", rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Errorf("Location: got %q, want none", rec.Header().Get("Location"))
	}
	if len(eng.calls) != 0 {
		t.Errorf("reconfigure calls: got %v, want none", eng.calls)
	}
	pinEq(t, "live base_url", cfg.RDS.BaseURL, "http://fleet-new.test:8088")
	pinEq(t, "live poll", cfg.RDS.PollInterval, 10*time.Second)
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

type testSendCase struct {
	name    string
	query   string // test-alert only
	form    url.Values
	mutate  func(c *config.Config, host string, port int)
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
			h, eng := newConfigPinHandlers(t, cfg)

			body := ""
			if tc.form != nil {
				body = tc.form.Encode()
			}
			req := httptest.NewRequest(http.MethodPost, path+tc.query, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			handler(h)(rec, req)

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
			// Neither door writes config or reloads anything.
			if raw, _ := os.ReadFile(eng.path); string(raw) != configPinSeed {
				t.Errorf("config file written by a test door")
			}
			if len(eng.calls) != 0 {
				t.Errorf("reconfigure calls: got %v", eng.calls)
			}
		})
	}
}

func TestPinConfig_TestEmail(t *testing.T) {
	missing := "SMTP host, from address, and at least one recipient are required"
	runTestSendCases(t, func(h *Handlers) http.HandlerFunc { return h.handleConfigTestEmail }, "/config/test-email", []testSendCase{
		{
			// C5: Enabled is NOT required; the send is attempted and its
			// failure is a 200 with ok:false.
			name:   "DisabledStillSends",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.Enabled = false },
			status: http.StatusOK, ok: false, message: "smtp client", json: true,
		},
		{
			name:   "BlankHost",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.SMTPHost = "" },
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			name:   "BlankFrom",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.FromAddress = "" },
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			name:   "NoRecipients",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.Recipients = nil },
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
		},
		{
			// C5: it reads the SAVED config; a posted draft is ignored.
			name: "PostedDraftIgnored",
			form: pinForm("notif_smtp_host", "smtp-draft.test", "notif_from_address", "d@test.invalid", "notif_recipient_0", "d@test.invalid"),
			mutate: func(c *config.Config, _ string, _ int) {
				c.Notifications.SMTPHost = ""
			},
			status: http.StatusBadRequest, ok: false, message: missing, json: true,
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
			// C5: test-alert requires Enabled, test-email does not.
			name: "DisabledRefused", query: "?type=fault",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.Enabled = false },
			status: http.StatusBadRequest, ok: false, message: "Notifications are not enabled", json: true,
		},
		{
			// Enabled is checked before the SMTP fields.
			name: "DisabledAndBlankHost", query: "?type=fault",
			mutate: func(c *config.Config, _ string, _ int) {
				c.Notifications.Enabled = false
				c.Notifications.SMTPHost = ""
			},
			status: http.StatusBadRequest, ok: false, message: "Notifications are not enabled", json: true,
		},
		{
			name: "BlankHost", query: "?type=fail",
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.SMTPHost = "" },
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
			// C5: reads the saved config; a posted draft is ignored.
			name: "PostedDraftIgnored", query: "?type=fault",
			form:   pinForm("notif_enabled", "on"),
			mutate: func(c *config.Config, _ string, _ int) { c.Notifications.Enabled = false },
			status: http.StatusBadRequest, ok: false, message: "Notifications are not enabled", json: true,
		},
	})
}

func pinPost(t *testing.T, handler http.HandlerFunc, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}
