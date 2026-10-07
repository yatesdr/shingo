package www

// handlers_config_pins_test.go — U0 pins for the Edge configuration save
// doors, at the pre-change tree (ui-cleanup, 2026-10-07).
//
// Each case pins, for one posted body: the response, what the live config
// holds afterwards, whether cfg.Save wrote the file (and the keys it wrote,
// read back from disk), how many backups were requested and with what reason,
// and which apply call fired (ReconnectKafka, ApplyWarLinkConfig).
//
// These pin TODAY's behaviour, including the parts the config-page rebuild
// changes on purpose (E1, E8, the blank timezone, the empty password). Each
// such case names its label; the predicted post-change value is written
// beside it in the evidence folder (predictions/u0-edge.md), not here.
//
// BACKUP REQUESTS ARE COUNTED THROUGH THE REAL SERVICE. backup.Service's
// trigger queue is unexported, but a full queue makes RequestBackup log one
// "dropped trigger %q" line per request through the logf the test supplies.
// So the rig fills the queue (64) with a service that is never started, and
// every request a handler makes after that is one line naming its reason.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"shingo/protocol/auth"
	"shingoedge/backup"
	"shingoedge/config"
	"shingoedge/engine"
)

// pinSpyEngine is the shared stubEngine with the two apply calls counted.
type pinSpyEngine struct {
	*stubEngine
	reconnects   int
	reconnectErr error
	applies      int
}

func (s *pinSpyEngine) ReconnectKafka() error { s.reconnects++; return s.reconnectErr }
func (s *pinSpyEngine) ApplyWarLinkConfig()   { s.applies++ }

// pinBackupLog records the reason of every backup request dropped by a full
// queue — which, with the queue pre-filled, is every request.
type pinBackupLog struct {
	mu      sync.Mutex
	reasons []string
}

func (l *pinBackupLog) logf(format string, args ...any) {
	if !strings.HasPrefix(format, "backup: dropped trigger") || len(args) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reasons = append(l.reasons, fmt.Sprint(args[0]))
}

func (l *pinBackupLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.reasons...)
}

type pinRig struct {
	h       *Handlers
	r       *chi.Mux
	spy     *pinSpyEngine
	cfg     *config.Config
	cfgPath string
	cookie  *http.Cookie
	backups *pinBackupLog
}

// newPinRig builds handlers over the shared stub with the spy in both engine
// seats, a config file path that does not exist yet (so "was it written" is
// "does it exist"), and the production paths behind adminMiddleware.
func newPinRig(t *testing.T, withBackup bool) *pinRig {
	t.Helper()
	h, r := newTestHandlers(t)
	stub := h.engine.(*stubEngine)
	spy := &pinSpyEngine{stubEngine: stub}
	h.engine = spy
	h.orchestration = spy

	rig := &pinRig{h: h, r: r, spy: spy, cfg: stub.cfg, cfgPath: stub.cfgPath, backups: &pinBackupLog{}}
	if withBackup {
		svc := backup.NewService(testDB, stub.cfg, stub.cfgPath, "pin", rig.backups.logf)
		for i := 0; i < 64; i++ {
			svc.RequestBackup("prefill")
		}
		h.backup = svc
	}

	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(h.adminMiddleware)
			r.Put("/config/warlink", h.apiUpdateWarLink)
			r.Put("/shifts", h.apiSaveShifts)
			r.Put("/config/core-api", h.apiUpdateCoreAPI)
			r.Put("/config/messaging", h.apiUpdateMessaging)
			r.Put("/config/station-id", h.apiUpdateStationID)
			r.Put("/config/timezone", h.apiUpdateTimezone)
			r.Put("/config/auto-confirm", h.apiUpdateAutoConfirm)
			r.Post("/config/password", h.apiChangePassword)
			r.Get("/backups", h.apiListBackups)
			r.Put("/backups/config", h.apiUpdateBackupConfig)
		})
	})
	rig.cookie = authCookie(t, h)
	return rig
}

// saved reads the config file back. ok=false: Save never wrote it.
func (rig *pinRig) saved(t *testing.T) (map[string]any, bool) {
	t.Helper()
	raw, err := os.ReadFile(rig.cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse saved config: %v\n%s", err, raw)
	}
	return m, true
}

// yamlAt walks a dotted path ("backup.s3.secret_key") through the saved file.
func yamlAt(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// pinCase is one posted body through one door.
type pinCase struct {
	name     string
	noBackup bool // h.backup == nil
	seed     func(c *config.Config)
	method   string
	path     string
	body     any    // marshalled as JSON
	rawBody  string // sent verbatim when set
	status   int
	resp     map[string]any // response keys that must be present with these values
	wrote    bool
	saved    map[string]any // dotted yaml path → value read back from the file
	backups  []string       // reasons, in order; nil = none
	reconn   int
	applies  int
	live     func(t *testing.T, c *config.Config)
}

func runPinCases(t *testing.T, cases []pinCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPinRig(t, !tc.noBackup)
			if tc.seed != nil {
				rig.cfg.Lock()
				tc.seed(rig.cfg)
				rig.cfg.Unlock()
			}
			var resp *http.Response
			if tc.rawBody != "" {
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.rawBody))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(rig.cookie)
				w := httptest.NewRecorder()
				rig.r.ServeHTTP(w, req)
				resp = w.Result()
			} else {
				resp = doRequest(t, rig.r, tc.method, tc.path, tc.body, rig.cookie)
			}
			assertStatus(t, resp, tc.status)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(tc.resp) > 0 {
				var got map[string]any
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("response is not a JSON object: %v\n%s", err, body)
				}
				for k, want := range tc.resp {
					if !reflect.DeepEqual(got[k], want) {
						t.Errorf("response %q: got %#v, want %#v (body %s)", k, got[k], want, body)
					}
				}
			}

			m, wrote := rig.saved(t)
			if wrote != tc.wrote {
				t.Errorf("config file written: got %v, want %v", wrote, tc.wrote)
			}
			for path, want := range tc.saved {
				got, ok := yamlAt(m, path)
				if !ok {
					t.Errorf("saved %s: key absent", path)
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("saved %s: got %#v, want %#v", path, got, want)
				}
			}
			if got := rig.backups.got(); !reflect.DeepEqual(got, tc.backups) && !(len(got) == 0 && len(tc.backups) == 0) {
				t.Errorf("backup requests: got %q, want %q", got, tc.backups)
			}
			if rig.spy.reconnects != tc.reconn {
				t.Errorf("ReconnectKafka calls: got %d, want %d", rig.spy.reconnects, tc.reconn)
			}
			if rig.spy.applies != tc.applies {
				t.Errorf("ApplyWarLinkConfig calls: got %d, want %d", rig.spy.applies, tc.applies)
			}
			if tc.live != nil {
				rig.cfg.RLock()
				defer rig.cfg.RUnlock()
				tc.live(t, rig.cfg)
			}
		})
	}
}

const restartNote = "written to shingoedge.yaml — RESTART shingoedge for it to take effect"
const timezoneNote = "written to shingoedge.yaml — RESTART shingoedge for display and hourly bucketing to pick it up"

var okResp = map[string]any{"status": "ok"}

// ── Core API ─────────────────────────────────────────────────────────────

func TestPinEdgeConfig_CoreAPI(t *testing.T) {
	runPinCases(t, []pinCase{
		{
			// No backup request and no apply: the one config door of seven
			// that does not request a backup. The address is restart-only (E2).
			name: "set", method: "PUT", path: "/api/config/core-api",
			body:   map[string]string{"core_api": "http://core.test:8080"},
			status: 200, resp: okResp, wrote: true,
			saved: map[string]any{"core_api": "http://core.test:8080"},
			live: func(t *testing.T, c *config.Config) {
				if c.CoreAPI != "http://core.test:8080" {
					t.Errorf("live CoreAPI = %q", c.CoreAPI)
				}
			},
		},
		{
			name: "blank_accepted", method: "PUT", path: "/api/config/core-api",
			seed:   func(c *config.Config) { c.CoreAPI = "http://old.test" },
			body:   map[string]string{"core_api": ""},
			status: 200, resp: okResp, wrote: true,
			saved: map[string]any{"core_api": ""},
			live: func(t *testing.T, c *config.Config) {
				if c.CoreAPI != "" {
					t.Errorf("live CoreAPI = %q, want blank", c.CoreAPI)
				}
			},
		},
		{
			name: "bad_json", method: "PUT", path: "/api/config/core-api",
			seed:    func(c *config.Config) { c.CoreAPI = "http://old.test" },
			rawBody: `{"core_api": 7}`,
			status:  400, wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.CoreAPI != "http://old.test" {
					t.Errorf("live CoreAPI = %q, want untouched", c.CoreAPI)
				}
			},
		},
	})
}

// ── Messaging (brokers) ──────────────────────────────────────────────────

func TestPinEdgeConfig_Messaging(t *testing.T) {
	runPinCases(t, []pinCase{
		{
			name: "set_two", method: "PUT", path: "/api/config/messaging",
			body:   map[string]any{"kafka_brokers": []string{"broker1:9092", "broker2:9092"}},
			status: 200, resp: okResp, wrote: true,
			saved:   map[string]any{"messaging.kafka.brokers": []any{"broker1:9092", "broker2:9092"}},
			backups: []string{"messaging-config"}, reconn: 1,
		},
		{
			// A broker with no port is stored as typed; nothing validates it.
			name: "no_port_stored_as_typed", method: "PUT", path: "/api/config/messaging",
			body:   map[string]any{"kafka_brokers": []string{"broker1"}},
			status: 200, resp: okResp, wrote: true,
			saved:   map[string]any{"messaging.kafka.brokers": []any{"broker1"}},
			backups: []string{"messaging-config"}, reconn: 1,
		},
		{
			name: "empty_list", method: "PUT", path: "/api/config/messaging",
			seed:   func(c *config.Config) { c.Messaging.Kafka.Brokers = []string{"old:9092"} },
			body:   map[string]any{"kafka_brokers": []string{}},
			status: 200, resp: okResp, wrote: true,
			saved:   map[string]any{"messaging.kafka.brokers": []any{}},
			backups: []string{"messaging-config"}, reconn: 1,
		},
	})
	// A failed reconnect is only logged: the answer is still ok. The spy has
	// to be armed before the request, so this one is not in the table.
	t.Run("reconnect_fails_still_ok", func(t *testing.T) {
		rig := newPinRig(t, true)
		rig.spy.reconnectErr = errors.New("broker down")
		resp := doRequest(t, rig.r, "PUT", "/api/config/messaging",
			map[string]any{"kafka_brokers": []string{"broker1:9092"}}, rig.cookie)
		assertStatus(t, resp, 200)
		assertJSONPath(t, resp, "status", "ok")
		if _, wrote := rig.saved(t); !wrote {
			t.Error("config file not written")
		}
		if rig.spy.reconnects != 1 {
			t.Errorf("ReconnectKafka calls = %d, want 1", rig.spy.reconnects)
		}
		if got := rig.backups.got(); !reflect.DeepEqual(got, []string{"messaging-config"}) {
			t.Errorf("backup requests = %q", got)
		}
	})
}

// ── Station UID (E1) ─────────────────────────────────────────────────────

func TestPinEdgeConfig_StationID(t *testing.T) {
	seedUID := func(c *config.Config) { c.StationUID = "seed.uid"; c.Messaging.StationID = "seed.legacy" }
	runPinCases(t, []pinCase{
		{
			name: "uid", method: "PUT", path: "/api/config/station-id",
			seed:   seedUID,
			body:   map[string]string{"station_uid": "plant.line-9"},
			status: 200, resp: map[string]any{"status": "ok", "note": restartNote}, wrote: true,
			saved:   map[string]any{"station_uid": "plant.line-9", "messaging.station_id": "seed.legacy"},
			backups: []string{"station-id"},
			live: func(t *testing.T, c *config.Config) {
				if c.StationUID != "plant.line-9" || c.Messaging.StationID != "seed.legacy" {
					t.Errorf("live uid=%q legacy=%q", c.StationUID, c.Messaging.StationID)
				}
			},
		},
		{
			// TestApiConfig_UpdateStationID's case: legacy station_id only.
			name: "legacy_station_id_only", method: "PUT", path: "/api/config/station-id",
			seed:   seedUID,
			body:   map[string]string{"station_id": "plant-a.line-1"},
			status: 200, resp: map[string]any{"status": "ok", "note": restartNote}, wrote: true,
			saved:   map[string]any{"station_uid": "seed.uid", "messaging.station_id": "plant-a.line-1"},
			backups: []string{"station-id"},
			live: func(t *testing.T, c *config.Config) {
				if c.StationUID != "seed.uid" || c.Messaging.StationID != "plant-a.line-1" {
					t.Errorf("live uid=%q legacy=%q", c.StationUID, c.Messaging.StationID)
				}
			},
		},
		{
			// E1: a blank uid skips the assignment but still writes the file,
			// requests a backup and answers ok.
			name: "blank_uid", method: "PUT", path: "/api/config/station-id",
			seed:   seedUID,
			body:   map[string]string{"station_uid": ""},
			status: 200, resp: map[string]any{"status": "ok", "note": restartNote}, wrote: true,
			saved:   map[string]any{"station_uid": "seed.uid", "messaging.station_id": "seed.legacy"},
			backups: []string{"station-id"},
			live: func(t *testing.T, c *config.Config) {
				if c.StationUID != "seed.uid" {
					t.Errorf("live uid=%q, want untouched", c.StationUID)
				}
			},
		},
		{
			// E1: both blank — same as above.
			name: "both_blank", method: "PUT", path: "/api/config/station-id",
			seed:   seedUID,
			body:   map[string]string{"station_uid": "", "station_id": ""},
			status: 200, resp: map[string]any{"status": "ok", "note": restartNote}, wrote: true,
			saved:   map[string]any{"station_uid": "seed.uid", "messaging.station_id": "seed.legacy"},
			backups: []string{"station-id"},
		},
	})
}

// ── Plant timezone ───────────────────────────────────────────────────────

func TestPinEdgeConfig_Timezone(t *testing.T) {
	runPinCases(t, []pinCase{
		{
			name: "valid", method: "PUT", path: "/api/config/timezone",
			body:   map[string]string{"timezone": "America/Chicago"},
			status: 200, resp: map[string]any{"status": "ok", "note": timezoneNote}, wrote: true,
			saved:   map[string]any{"timezone": "America/Chicago"},
			backups: []string{"timezone"},
		},
		{
			// time.LoadLocation("") is UTC with no error and loc.String() is
			// stored, so a blank is saved as "UTC" (only the client blocks it).
			name: "blank_saves_UTC", method: "PUT", path: "/api/config/timezone",
			seed:   func(c *config.Config) { c.Timezone = "America/Chicago" },
			body:   map[string]string{"timezone": ""},
			status: 200, resp: map[string]any{"status": "ok", "note": timezoneNote}, wrote: true,
			saved:   map[string]any{"timezone": "UTC"},
			backups: []string{"timezone"},
			live: func(t *testing.T, c *config.Config) {
				if c.Timezone != "UTC" {
					t.Errorf("live Timezone = %q, want UTC", c.Timezone)
				}
			},
		},
		{
			name: "not_iana", method: "PUT", path: "/api/config/timezone",
			seed:   func(c *config.Config) { c.Timezone = "America/Chicago" },
			body:   map[string]string{"timezone": "Mars/Base"},
			status: 400, resp: map[string]any{"error": `not an IANA timezone: "Mars/Base"`}, wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.Timezone != "America/Chicago" {
					t.Errorf("live Timezone = %q, want untouched", c.Timezone)
				}
			},
		},
	})
}

// ── Auto-confirm ─────────────────────────────────────────────────────────

func TestPinEdgeConfig_AutoConfirm(t *testing.T) {
	runPinCases(t, []pinCase{
		{
			name: "on", method: "PUT", path: "/api/config/auto-confirm",
			body:   map[string]bool{"auto_confirm": true},
			status: 200, resp: okResp, wrote: true,
			saved:   map[string]any{"web.auto_confirm": true},
			backups: []string{"auto-confirm"},
		},
		{
			// An absent key is false.
			name: "absent_is_off", method: "PUT", path: "/api/config/auto-confirm",
			seed:   func(c *config.Config) { c.Web.AutoConfirm = true },
			body:   map[string]any{},
			status: 200, resp: okResp, wrote: true,
			saved:   map[string]any{"web.auto_confirm": false},
			backups: []string{"auto-confirm"},
		},
	})
}

// ── PLC link (WarLink) ───────────────────────────────────────────────────

func TestPinEdgeConfig_WarLink(t *testing.T) {
	seed := func(c *config.Config) {
		c.WarLink.Host = "old.host"
		c.WarLink.Port = 8080
		c.WarLink.PollRate = time.Second
		c.WarLink.Enabled = true
		c.WarLink.Mode = "poll"
	}
	runPinCases(t, []pinCase{
		{
			name: "full", method: "PUT", path: "/api/config/warlink",
			seed: seed,
			body: map[string]any{"host": "warlink.test", "port": 9090, "enabled": true,
				"poll_rate": "2s", "mode": "sse"},
			status: 200, resp: okResp, wrote: true,
			saved: map[string]any{"warlink.host": "warlink.test", "warlink.port": 9090,
				"warlink.poll_rate": "2s", "warlink.enabled": true, "warlink.mode": "sse"},
			backups: []string{"warlink-config"}, applies: 1,
		},
		{
			// Blank host / zero port / blank poll rate / blank mode keep the
			// old value; `enabled` does not — absent is false.
			name: "host_only_disables", method: "PUT", path: "/api/config/warlink",
			seed:   seed,
			body:   map[string]any{"host": "new.host"},
			status: 200, resp: okResp, wrote: true,
			saved: map[string]any{"warlink.host": "new.host", "warlink.port": 8080,
				"warlink.poll_rate": "1s", "warlink.enabled": false, "warlink.mode": "poll"},
			backups: []string{"warlink-config"}, applies: 1,
		},
		{
			name: "bad_mode", method: "PUT", path: "/api/config/warlink",
			seed:   seed,
			body:   map[string]any{"host": "new.host", "mode": "invalid"},
			status: 400, resp: map[string]any{"error": `mode must be "poll" or "sse"`}, wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.WarLink.Host != "old.host" {
					t.Errorf("live Host = %q, want untouched", c.WarLink.Host)
				}
			},
		},
		{
			// A bad poll rate is refused AFTER host and port were assigned to
			// the live config: nothing is written, but the live struct keeps
			// the new host and port.
			name: "bad_poll_rate_leaves_live_host_changed", method: "PUT", path: "/api/config/warlink",
			seed:   seed,
			body:   map[string]any{"host": "new.host", "port": 9191, "poll_rate": "fast"},
			status: 400, wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.WarLink.Host != "new.host" || c.WarLink.Port != 9191 {
					t.Errorf("live host:port = %s:%d, want new.host:9191 (mutated before the refusal)",
						c.WarLink.Host, c.WarLink.Port)
				}
				if !c.WarLink.Enabled || c.WarLink.PollRate != time.Second {
					t.Errorf("live enabled=%v poll=%v, want true/1s", c.WarLink.Enabled, c.WarLink.PollRate)
				}
			},
		},
	})
}

// TestPinEdgeConfig_WarLinkSim: in sim the door answers exactly as it does
// live, and the engine's ApplyWarLinkConfig is a logged no-op (E4). Against
// the real engine, never started: outside sim the apply would dereference the
// PLC manager Start creates and panic, so reaching the log line is the proof
// that it returned early.
func TestPinEdgeConfig_WarLinkSim(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "shingoedge.yaml")
	cfg := config.Defaults()
	cfg.Sim.Enabled = true
	cfg.StationUID = "pin.sim"
	var logs []string
	var mu sync.Mutex
	eng := engine.New(engine.Config{AppConfig: cfg, ConfigPath: cfgPath, DB: testDB,
		LogFunc: func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }})
	h := &Handlers{engine: eng, orchestration: eng, sessions: newSessionStore(""), eventHub: NewEventHub(),
		stationViews: newStationViewGroup()}
	r := chi.NewRouter()
	r.Put("/api/config/warlink", h.apiUpdateWarLink)

	resp := doRequest(t, r, "PUT", "/api/config/warlink",
		map[string]any{"host": "warlink.test", "port": 9090, "enabled": true, "mode": "poll"}, nil)
	assertStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Errorf("sim response = %s, want {\"status\":\"ok\"} (no word that the apply was skipped)", body)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config file not written in sim: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range logs {
		if strings.Contains(l, "[sim] ignoring WarLink config apply") {
			found = true
		}
	}
	if !found {
		t.Errorf("no sim no-op log line; logs: %q", logs)
	}
}

// ── Backups (E6, E7, E8) ─────────────────────────────────────────────────

func seedStorage(c *config.Config) {
	c.Backup.Enabled = false
	c.Backup.ScheduleInterval = time.Hour
	c.Backup.S3 = config.BackupS3Config{Endpoint: "http://s3.pin.invalid", Bucket: "pin-bucket",
		Region: "us-east-1", AccessKey: "PINKEY", SecretKey: "OLDSECRET"}
}

func TestPinEdgeConfig_BackupConfig(t *testing.T) {
	storageBody := func(enabled bool, secret string) map[string]any {
		return map[string]any{"enabled": enabled, "schedule_interval": "1h",
			"endpoint": "http://s3.pin.invalid", "bucket": "pin-bucket", "region": "us-east-1",
			"access_key": "PINKEY", "secret_key": secret}
	}
	runPinCases(t, []pinCase{
		{
			// E8: the posted secret overwrites unconditionally — blank erases.
			name: "disabled_blank_secret_erases", method: "PUT", path: "/api/backups/config",
			seed: seedStorage, body: storageBody(false, ""),
			status: 200, resp: okResp, wrote: true,
			saved:   map[string]any{"backup.s3.secret_key": "", "backup.enabled": false},
			backups: []string{"backup-config-updated"},
			live: func(t *testing.T, c *config.Config) {
				if c.Backup.S3.SecretKey != "" {
					t.Errorf("live secret = %q, want erased", c.Backup.S3.SecretKey)
				}
			},
		},
		{
			// E8: enabling with a blank secret is refused even though one is saved.
			name: "enable_blank_secret_refused", method: "PUT", path: "/api/backups/config",
			seed: seedStorage, body: storageBody(true, ""),
			status: 400, wrote: false,
			resp: map[string]any{"error": "endpoint, bucket, access key, and secret key are required to enable automatic backups"},
			live: func(t *testing.T, c *config.Config) {
				if c.Backup.S3.SecretKey != "OLDSECRET" || c.Backup.Enabled {
					t.Errorf("live secret=%q enabled=%v, want untouched", c.Backup.S3.SecretKey, c.Backup.Enabled)
				}
			},
		},
		{
			name: "enable_same_storage_new_secret", method: "PUT", path: "/api/backups/config",
			seed: seedStorage, body: storageBody(true, "  NEWSECRET  "),
			status: 200, resp: okResp, wrote: true,
			saved: map[string]any{"backup.s3.secret_key": "NEWSECRET", "backup.enabled": true,
				"backup.schedule_interval": "1h0m0s"},
			backups: []string{"backup-config-updated"},
		},
		{
			// A blank interval is 1h.
			name: "blank_interval_is_1h", method: "PUT", path: "/api/backups/config",
			body:   map[string]any{"enabled": false, "schedule_interval": ""},
			status: 200, resp: okResp, wrote: true,
			saved:   map[string]any{"backup.schedule_interval": "1h0m0s"},
			backups: []string{"backup-config-updated"},
		},
		{
			// E8: with no backup service the door still saves today.
			name: "nil_service_enable_changed_storage_saves", noBackup: true,
			method: "PUT", path: "/api/backups/config",
			seed: seedStorage,
			body: map[string]any{"enabled": true, "schedule_interval": "1h",
				"endpoint": "http://other.pin.invalid", "bucket": "pin-bucket", "region": "us-east-1",
				"access_key": "PINKEY", "secret_key": "OLDSECRET"},
			status: 200, resp: okResp, wrote: true,
			saved: map[string]any{"backup.enabled": true, "backup.s3.endpoint": "http://other.pin.invalid"},
		},
		{
			name: "nil_service_disable_saves", noBackup: true,
			method: "PUT", path: "/api/backups/config",
			seed: seedStorage, body: storageBody(false, "OLDSECRET"),
			status: 200, resp: okResp, wrote: true,
			saved: map[string]any{"backup.enabled": false},
		},
	})
}

// TestPinEdgeConfig_BackupEnableChangedStorage: enabling with a changed
// endpoint saves without ever contacting the storage (E8 adds TestConfig
// first). The endpoint is a local server that counts every request.
func TestPinEdgeConfig_BackupEnableChangedStorage(t *testing.T) {
	var hits atomic.Int64
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer s3.Close()

	rig := newPinRig(t, true)
	rig.cfg.Lock()
	seedStorage(rig.cfg)
	rig.cfg.Unlock()
	resp := doRequest(t, rig.r, "PUT", "/api/backups/config", map[string]any{
		"enabled": true, "schedule_interval": "1h", "endpoint": s3.URL, "bucket": "pin-bucket",
		"region": "us-east-1", "access_key": "PINKEY", "secret_key": "OLDSECRET", "use_path_style": true,
	}, rig.cookie)
	assertStatus(t, resp, 200)
	assertJSONPath(t, resp, "status", "ok")
	m, wrote := rig.saved(t)
	if !wrote {
		t.Fatal("config file not written")
	}
	if got, _ := yamlAt(m, "backup.s3.endpoint"); got != s3.URL {
		t.Errorf("saved endpoint = %v, want %s", got, s3.URL)
	}
	if got, _ := yamlAt(m, "backup.enabled"); got != true {
		t.Errorf("saved enabled = %v, want true", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("storage contacted %d times during the save, want 0 (no TestConfig today)", n)
	}
	if got := rig.backups.got(); !reflect.DeepEqual(got, []string{"backup-config-updated"}) {
		t.Errorf("backup requests = %q", got)
	}
}

// TestPinEdgeConfig_ListBackupsNoEndpoint: with the service present and no
// storage set, GET /api/backups is a 400 — which every config page load makes
// today (E6 stops the page asking).
func TestPinEdgeConfig_ListBackupsNoEndpoint(t *testing.T) {
	rig := newPinRig(t, true)
	resp := doRequest(t, rig.r, "GET", "/api/backups", nil, rig.cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "backup endpoint is required")
	if _, wrote := rig.saved(t); wrote {
		t.Error("a list wrote the config file")
	}
}

// ── Shifts (database, not the config file) ───────────────────────────────

func TestPinEdgeConfig_Shifts(t *testing.T) {
	type row = map[string]any
	reset := func(t *testing.T) {
		t.Helper()
		if _, err := testDB.Exec("DELETE FROM shifts"); err != nil {
			t.Fatalf("clear shifts: %v", err)
		}
		for n, s := range [][3]string{{"Day", "06:00", "14:00"}, {"Swing", "14:00", "22:00"}, {"Night", "22:00", "06:00"}} {
			if err := testDB.UpsertShift(n+1, s[0], s[1], s[2]); err != nil {
				t.Fatalf("seed shift %d: %v", n+1, err)
			}
		}
	}
	numbers := func(t *testing.T) []int {
		t.Helper()
		got, err := testDB.ListShifts()
		if err != nil {
			t.Fatalf("ListShifts: %v", err)
		}
		var ns []int
		for _, s := range got {
			ns = append(ns, s.ShiftNumber)
		}
		return ns
	}
	t.Cleanup(func() { testDB.Exec("DELETE FROM shifts") })

	cases := []struct {
		name string
		body any
		want []int
	}{
		{"edit_two_absent_third_deleted", []row{
			{"shift_number": 1, "name": "Day", "start_time": "06:00", "end_time": "14:30"},
			{"shift_number": 2, "name": "Swing", "start_time": "14:30", "end_time": "22:00"},
		}, []int{1, 2}},
		{"blank_times_delete", []row{
			{"shift_number": 1, "name": "Day", "start_time": "06:00", "end_time": "14:00"},
			{"shift_number": 2, "name": "", "start_time": "", "end_time": ""},
			{"shift_number": 3, "name": "Night", "start_time": "22:00", "end_time": "06:00"},
		}, []int{1, 3}},
		{"empty_list_deletes_all", []row{}, nil},
		// The handler's comment says an out-of-range-only payload "must not
		// mark every real shift absent"; it does — every shift is deleted.
		{"out_of_range_only_deletes_all", []row{
			{"shift_number": 4, "name": "X", "start_time": "00:00", "end_time": "06:00"},
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset(t)
			rig := newPinRig(t, true)
			resp := doRequest(t, rig.r, "PUT", "/api/shifts", tc.body, rig.cookie)
			assertStatus(t, resp, 200)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != `{"ok":true}` {
				t.Errorf("response = %s, want {\"ok\":true}", body)
			}
			if got := numbers(t); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("shifts left = %v, want %v", got, tc.want)
			}
			if _, wrote := rig.saved(t); wrote {
				t.Error("shifts wrote the config file")
			}
			if got := rig.backups.got(); !reflect.DeepEqual(got, []string{"shifts"}) {
				t.Errorf("backup requests = %q, want [shifts]", got)
			}
		})
	}
	t.Run("not_an_array", func(t *testing.T) {
		reset(t)
		rig := newPinRig(t, true)
		resp := doRequest(t, rig.r, "PUT", "/api/shifts", map[string]string{"not": "an array"}, rig.cookie)
		assertStatus(t, resp, 400)
		resp.Body.Close()
		if got := numbers(t); !reflect.DeepEqual(got, []int{1, 2, 3}) {
			t.Errorf("shifts left = %v, want untouched", got)
		}
		if got := rig.backups.got(); len(got) != 0 {
			t.Errorf("backup requests = %q, want none", got)
		}
	})
}

// ── Change password (the door that stays) ────────────────────────────────

func TestPinEdgeConfig_ChangePassword(t *testing.T) {
	cases := []struct {
		name      string
		old, next string
		status    int
		resp      map[string]any
		changedTo string // the password the stored hash now checks against; "" = unchanged
		emptyOK   bool
	}{
		{name: "ok", old: "password", next: "newpassword123", status: 200,
			resp: okResp, changedTo: "newpassword123"},
		// Accepted today: the stored hash becomes the hash of "".
		{name: "empty_new_accepted", old: "password", next: "", status: 200,
			resp: okResp, emptyOK: true},
		{name: "wrong_old", old: "nope", next: "newpassword123", status: 400,
			resp: map[string]any{"error": "current password is incorrect"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPinRig(t, true)
			resp := doRequest(t, rig.r, "POST", "/api/config/password",
				map[string]string{"old_password": tc.old, "new_password": tc.next}, rig.cookie)
			assertStatus(t, resp, tc.status)
			var got map[string]any
			decodeJSON(t, resp, &got)
			for k, v := range tc.resp {
				if got[k] != v {
					t.Errorf("response %q = %v, want %v", k, got[k], v)
				}
			}
			u, err := testDB.GetAdminUser("testadmin")
			if err != nil {
				t.Fatalf("GetAdminUser: %v", err)
			}
			switch {
			case tc.emptyOK:
				if !auth.CheckPassword(u.PasswordHash, "") {
					t.Error("stored hash does not check against the empty password")
				}
			case tc.changedTo != "":
				if !auth.CheckPassword(u.PasswordHash, tc.changedTo) {
					t.Errorf("stored hash does not check against %q", tc.changedTo)
				}
			default:
				if !auth.CheckPassword(u.PasswordHash, "password") {
					t.Error("stored hash changed on a refused change")
				}
			}
			if _, wrote := rig.saved(t); wrote {
				t.Error("a password change wrote the config file")
			}
			if got := rig.backups.got(); len(got) != 0 {
				t.Errorf("backup requests = %q, want none", got)
			}
		})
	}
}
