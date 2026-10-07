package www

// handlers_config_pins_test.go — the U0 pins for the Edge configuration save
// doors (ui-cleanup, 2026-10-07), RE-POINTED BY U3 at the one door that
// replaced them, PUT /api/config, each case posting its old body as that
// door's section and asserting the predicted "after"
// (predictions/u0-edge.md). Every case that moved names its label beside it.
//
// Each case pins, for one posted body: the response, what the live config
// holds afterwards, whether cfg.Save wrote the file (and the keys it wrote,
// read back from disk), how many backups were requested and with what reason,
// and which apply call fired (ReconnectKafka, ApplyWarLinkConfig).
//
// BACKUP REQUESTS ARE COUNTED THROUGH THE REAL SERVICE. backup.Service's
// trigger queue is unexported, but a full queue makes RequestBackup log one
// "dropped trigger %q" line per request through the logf the test supplies.
// So the rig fills the queue (64) with a service that is never started, and
// every request a handler makes after that is one line naming its reason.
//
// ONE SAVE, ONE WRITE, ONE BACKUP: the door requests one backup per save, with
// the reason "config", where each old door requested its own (and core-api
// none).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"shingo/protocol/testutil"
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
			r.Put("/config", h.apiSaveConfig)
			r.Put("/shifts", h.apiSaveShifts)
			r.Post("/config/password", h.apiChangePassword)
			r.Get("/backups", h.apiListBackups)
		})
	})
	rig.cookie = authCookie(t, h)
	return rig
}

// boot takes the restart notice's snapshot of the rig's config as it stands
// (NewRouter does this at process start).
func (rig *pinRig) boot() { rig.h.boot = newConfigBoot(rig.cfg) }

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
	noResp   []string       // response keys that must be absent
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
			rig.boot()
			method, path := tc.method, tc.path
			if method == "" {
				method, path = "PUT", "/api/config"
			}
			var resp *http.Response
			if tc.rawBody != "" {
				req := httptest.NewRequest(method, path, strings.NewReader(tc.rawBody))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(rig.cookie)
				w := httptest.NewRecorder()
				rig.r.ServeHTTP(w, req)
				resp = w.Result()
			} else {
				resp = doRequest(t, rig.r, method, path, tc.body, rig.cookie)
			}
			assertStatus(t, resp, tc.status)
			body, err := io.ReadAll(resp.Body)
			testutil.MustNoErr(t, err, "read response body")
			resp.Body.Close()
			if len(tc.resp) > 0 || len(tc.noResp) > 0 {
				var got map[string]any
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("response is not a JSON object: %v\n%s", err, body)
				}
				for k, want := range tc.resp {
					if !reflect.DeepEqual(got[k], want) {
						t.Errorf("response %q: got %#v, want %#v (body %s)", k, got[k], want, body)
					}
				}
				for _, k := range tc.noResp {
					if _, ok := got[k]; ok {
						t.Errorf("response has %q, want it absent (body %s)", k, body)
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

// okSaved is the L5 success body's fixed part.
func okSaved(applied, restart []any) map[string]any {
	if applied == nil {
		applied = []any{}
	}
	if restart == nil {
		restart = []any{}
	}
	return map[string]any{"ok": true, "status": "ok", "applied": applied, "restart": restart, "failed": []any{}}
}

// refused is the L5 failure body: the first message on top, the field errors
// beside it.
func refused(field, msg string) map[string]any {
	return map[string]any{"ok": false, "error": msg, "errors": map[string]any{field: msg}}
}

var oneBackup = []string{"config"}

// stationBody: the Station section carries the UID (a station post with
// neither id is refused, E1), so a pin of one of its other fields posts the
// seeded UID with it.
func stationBody(extra map[string]any) map[string]any {
	s := map[string]any{"station_uid": "pin.uid"}
	for k, v := range extra {
		s[k] = v
	}
	return map[string]any{"station": s}
}

func seedPinUID(c *config.Config) { c.StationUID = "pin.uid" }

// ── Core API ─────────────────────────────────────────────────────────────

func TestPinEdgeConfig_CoreAPI(t *testing.T) {
	runPinCases(t, []pinCase{
		{
			// E2 (R20): restart-only, and the notice says so. R4: one save is
			// one backup request (the old door requested none).
			name:   "set",
			body:   map[string]any{"core": map[string]string{"core_api": "http://core.test:8080"}},
			status: 200, resp: okSaved(nil, []any{"Core address"}), wrote: true,
			saved:   map[string]any{"core_api": "http://core.test:8080"},
			backups: oneBackup,
			live: func(t *testing.T, c *config.Config) {
				if c.CoreAPI != "http://core.test:8080" {
					t.Errorf("live CoreAPI = %q", c.CoreAPI)
				}
			},
		},
		{
			// L8: a blank Core address is still saved.
			name:   "blank_accepted",
			seed:   func(c *config.Config) { c.CoreAPI = "http://old.test" },
			body:   map[string]any{"core": map[string]string{"core_api": ""}},
			status: 200, resp: okSaved(nil, []any{"Core address"}), wrote: true,
			saved:   map[string]any{"core_api": ""},
			backups: oneBackup,
			live: func(t *testing.T, c *config.Config) {
				if c.CoreAPI != "" {
					t.Errorf("live CoreAPI = %q, want blank", c.CoreAPI)
				}
			},
		},
		{
			name:    "bad_json",
			seed:    func(c *config.Config) { c.CoreAPI = "http://old.test" },
			rawBody: `{"core": {"core_api": 7}}`,
			status:  400, resp: map[string]any{"ok": false}, wrote: false,
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
			name:   "set_two",
			body:   map[string]any{"messaging": map[string]any{"brokers": []string{"broker1:9092", "broker2:9092"}}},
			status: 200, resp: okSaved([]any{"Messaging"}, nil), wrote: true,
			saved:   map[string]any{"messaging.kafka.brokers": []any{"broker1:9092", "broker2:9092"}},
			backups: oneBackup, reconn: 1,
		},
		{
			// L8: a broker with no port is stored as typed; nothing validates it.
			name:   "no_port_stored_as_typed",
			body:   map[string]any{"messaging": map[string]any{"brokers": []string{"broker1"}}},
			status: 200, resp: okSaved([]any{"Messaging"}, nil), wrote: true,
			saved:   map[string]any{"messaging.kafka.brokers": []any{"broker1"}},
			backups: oneBackup, reconn: 1,
		},
		{
			name:   "empty_list",
			seed:   func(c *config.Config) { c.Messaging.Kafka.Brokers = []string{"old:9092"} },
			body:   map[string]any{"messaging": map[string]any{"brokers": []string{}}},
			status: 200, resp: okSaved([]any{"Messaging"}, nil), wrote: true,
			saved:   map[string]any{"messaging.kafka.brokers": []any{}},
			backups: oneBackup, reconn: 1,
		},
	})
	// R4 (Core's contract): a failed reconnect after the write is reported in
	// `failed`; the file stays. The spy has to be armed before the request,
	// so this one is not in the table.
	t.Run("reconnect_fails_reported", func(t *testing.T) {
		rig := newPinRig(t, true)
		rig.spy.reconnectErr = errors.New("broker down")
		resp := doRequest(t, rig.r, "PUT", "/api/config",
			map[string]any{"messaging": map[string]any{"brokers": []string{"broker1:9092"}}}, rig.cookie)
		assertStatus(t, resp, 200)
		var got map[string]any
		decodeJSON(t, resp, &got)
		if got["ok"] != true || got["status"] != "ok" || !reflect.DeepEqual(got["failed"], []any{"Messaging"}) {
			t.Errorf("response = %v, want ok with failed [Messaging]", got)
		}
		if _, wrote := rig.saved(t); !wrote {
			t.Error("config file not written")
		}
		if rig.spy.reconnects != 1 {
			t.Errorf("ReconnectKafka calls = %d, want 1", rig.spy.reconnects)
		}
		if got := rig.backups.got(); !reflect.DeepEqual(got, oneBackup) {
			t.Errorf("backup requests = %q", got)
		}
	})
}

// ── Station UID (E1) ─────────────────────────────────────────────────────

func TestPinEdgeConfig_StationID(t *testing.T) {
	seedUID := func(c *config.Config) { c.StationUID = "seed.uid"; c.Messaging.StationID = "seed.legacy" }
	runPinCases(t, []pinCase{
		{
			// The RESTART toast's note goes; the restart notice names the field.
			// The live config still swaps (some readers take it live).
			name:   "uid",
			seed:   seedUID,
			body:   map[string]any{"station": map[string]string{"station_uid": "plant.line-9"}},
			status: 200, resp: okSaved([]any{"Station"}, []any{"Station UID"}), noResp: []string{"note"},
			wrote:   true,
			saved:   map[string]any{"station_uid": "plant.line-9", "messaging.station_id": "seed.legacy"},
			backups: oneBackup,
			live: func(t *testing.T, c *config.Config) {
				if c.StationUID != "plant.line-9" || c.Messaging.StationID != "seed.legacy" {
					t.Errorf("live uid=%q legacy=%q", c.StationUID, c.Messaging.StationID)
				}
			},
		},
		{
			// E1 check: TestApiConfig_UpdateStationID's case, legacy station_id
			// only, is still saved.
			name:   "legacy_station_id_only",
			seed:   seedUID,
			body:   map[string]any{"station": map[string]string{"station_id": "plant-a.line-1"}},
			status: 200, resp: okSaved([]any{"Station"}, nil), noResp: []string{"note"},
			wrote:   true,
			saved:   map[string]any{"station_uid": "seed.uid", "messaging.station_id": "plant-a.line-1"},
			backups: oneBackup,
			live: func(t *testing.T, c *config.Config) {
				if c.StationUID != "seed.uid" || c.Messaging.StationID != "plant-a.line-1" {
					t.Errorf("live uid=%q legacy=%q", c.StationUID, c.Messaging.StationID)
				}
			},
		},
		{
			// E1: a blank uid with no legacy id is a 400: no write, no backup.
			name:   "blank_uid",
			seed:   seedUID,
			body:   map[string]any{"station": map[string]string{"station_uid": ""}},
			status: 400, resp: refused("station_uid", "Station UID is required."), wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.StationUID != "seed.uid" {
					t.Errorf("live uid=%q, want untouched", c.StationUID)
				}
			},
		},
		{
			// E1: both blank — the same 400.
			name:   "both_blank",
			seed:   seedUID,
			body:   map[string]any{"station": map[string]string{"station_uid": "", "station_id": ""}},
			status: 400, resp: refused("station_uid", "Station UID is required."), wrote: false,
		},
	})
}

// ── Plant timezone ───────────────────────────────────────────────────────

func TestPinEdgeConfig_Timezone(t *testing.T) {
	runPinCases(t, []pinCase{
		{
			name:   "valid",
			seed:   seedPinUID,
			body:   stationBody(map[string]any{"timezone": "America/Chicago"}),
			status: 200, resp: okSaved([]any{"Station"}, []any{"Plant timezone"}), noResp: []string{"note"},
			wrote:   true,
			saved:   map[string]any{"timezone": "America/Chicago"},
			backups: oneBackup,
		},
		{
			// U3 Timezone: a blank is stored blank ("take the zone Core
			// offers"), where the old door stored "UTC".
			name:   "blank_stored_blank",
			seed:   func(c *config.Config) { seedPinUID(c); c.Timezone = "America/Chicago" },
			body:   stationBody(map[string]any{"timezone": ""}),
			status: 200, resp: okSaved([]any{"Station"}, []any{"Plant timezone"}), wrote: true,
			saved:   map[string]any{"timezone": ""},
			backups: oneBackup,
			live: func(t *testing.T, c *config.Config) {
				if c.Timezone != "" {
					t.Errorf("live Timezone = %q, want blank", c.Timezone)
				}
			},
		},
		{
			name:   "not_iana",
			seed:   func(c *config.Config) { seedPinUID(c); c.Timezone = "America/Chicago" },
			body:   stationBody(map[string]any{"timezone": "Mars/Base"}),
			status: 400, resp: refused("timezone", `not an IANA timezone: "Mars/Base"`), wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.Timezone != "America/Chicago" {
					t.Errorf("live Timezone = %q, want untouched", c.Timezone)
				}
			},
		},
	})
}

// ── Auto-confirm (the Station section) ───────────────────────────────────

func TestPinEdgeConfig_AutoConfirm(t *testing.T) {
	runPinCases(t, []pinCase{
		{
			name:   "on",
			seed:   seedPinUID,
			body:   stationBody(map[string]any{"auto_confirm": true}),
			status: 200, resp: okSaved([]any{"Station"}, nil), wrote: true,
			saved:   map[string]any{"web.auto_confirm": true},
			backups: oneBackup,
		},
		{
			// An absent key is false (the page always sends the section whole).
			name:   "absent_is_off",
			seed:   func(c *config.Config) { seedPinUID(c); c.Web.AutoConfirm = true },
			body:   stationBody(nil),
			status: 200, resp: okSaved([]any{"Station"}, nil), wrote: true,
			saved:   map[string]any{"web.auto_confirm": false},
			backups: oneBackup,
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
	plc := func(m map[string]any) map[string]any { return map[string]any{"plc": m} }
	runPinCases(t, []pinCase{
		{
			name: "full",
			seed: seed,
			body: plc(map[string]any{"host": "warlink.test", "port": 9090, "enabled": true,
				"poll_rate": "2s", "mode": "sse"}),
			status: 200, resp: okSaved([]any{"PLC link"}, nil), wrote: true,
			saved: map[string]any{"warlink.host": "warlink.test", "warlink.port": 9090,
				"warlink.poll_rate": "2s", "warlink.enabled": true, "warlink.mode": "sse"},
			backups: oneBackup, applies: 1,
		},
		{
			// L8: blank host / zero port / blank poll rate / blank mode keep the
			// old value; `enabled` does not — absent is false.
			name:   "host_only_disables",
			seed:   seed,
			body:   plc(map[string]any{"host": "new.host"}),
			status: 200, resp: okSaved([]any{"PLC link"}, nil), wrote: true,
			saved: map[string]any{"warlink.host": "new.host", "warlink.port": 8080,
				"warlink.poll_rate": "1s", "warlink.enabled": false, "warlink.mode": "poll"},
			backups: oneBackup, applies: 1,
		},
		{
			name:   "bad_mode",
			seed:   seed,
			body:   plc(map[string]any{"host": "new.host", "mode": "invalid"}),
			status: 400, resp: refused("mode", `mode must be "poll" or "sse"`), wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.WarLink.Host != "old.host" {
					t.Errorf("live Host = %q, want untouched", c.WarLink.Host)
				}
			},
		},
		{
			// U1 "one save path": the old door assigned host and port to the
			// LIVE config before refusing the poll rate. Now the copy takes
			// them and is thrown away: the live config is untouched.
			name:   "bad_poll_rate_leaves_live_untouched",
			seed:   seed,
			body:   plc(map[string]any{"host": "new.host", "port": 9191, "poll_rate": "fast"}),
			status: 400, resp: map[string]any{"ok": false}, wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.WarLink.Host != "old.host" || c.WarLink.Port != 8080 {
					t.Errorf("live host:port = %s:%d, want old.host:8080 (untouched)",
						c.WarLink.Host, c.WarLink.Port)
				}
				if !c.WarLink.Enabled || c.WarLink.PollRate != time.Second {
					t.Errorf("live enabled=%v poll=%v, want true/1s", c.WarLink.Enabled, c.WarLink.PollRate)
				}
			},
		},
	})
}

// TestPinEdgeConfig_WarLinkSim: in sim the engine's ApplyWarLinkConfig is a
// logged no-op, and (E4) the answer now says so — `simulated` names the PLC
// link, and `applied` does not. Against the real engine, never started:
// outside sim the apply would dereference the PLC manager Start creates and
// panic, so reaching the log line is the proof that it returned early.
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
	r.Put("/api/config", h.apiSaveConfig)

	resp := doRequest(t, r, "PUT", "/api/config",
		map[string]any{"plc": map[string]any{"host": "warlink.test", "port": 9090, "enabled": true, "mode": "poll"}}, nil)
	assertStatus(t, resp, 200)
	var got map[string]any
	decodeJSON(t, resp, &got)
	if !reflect.DeepEqual(got["simulated"], []any{"PLC link"}) {
		t.Errorf("sim response simulated = %#v, want [PLC link] (E4: the answer says the apply was skipped)", got["simulated"])
	}
	if !reflect.DeepEqual(got["applied"], []any{}) {
		t.Errorf("sim response applied = %#v, want [] (the PLC link was not applied)", got["applied"])
	}
	if got["ok"] != true || got["status"] != "ok" {
		t.Errorf("sim response = %v, want ok", got)
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
		return map[string]any{"backups": map[string]any{"enabled": enabled, "schedule_interval": "1h",
			"endpoint": "http://s3.pin.invalid", "bucket": "pin-bucket", "region": "us-east-1",
			"access_key": "PINKEY", "secret_key": secret}}
	}
	runPinCases(t, []pinCase{
		{
			// E8: a blank posted secret keeps the saved one (it used to erase it).
			name: "disabled_blank_secret_keeps",
			seed: seedStorage, body: storageBody(false, ""),
			status: 200, resp: okSaved([]any{"Backups"}, nil), wrote: true,
			saved:   map[string]any{"backup.s3.secret_key": "OLDSECRET", "backup.enabled": false},
			backups: oneBackup,
			live: func(t *testing.T, c *config.Config) {
				if c.Backup.S3.SecretKey != "OLDSECRET" {
					t.Errorf("live secret = %q, want kept", c.Backup.S3.SecretKey)
				}
			},
		},
		{
			// E8: the blank becomes the saved secret BEFORE the enable check, so
			// enabling with a blank secret is no longer refused; storage is
			// unchanged, so no storage check.
			name: "enable_blank_secret_uses_saved",
			seed: seedStorage, body: storageBody(true, ""),
			status: 200, resp: okSaved([]any{"Backups"}, nil), wrote: true,
			saved:   map[string]any{"backup.s3.secret_key": "OLDSECRET", "backup.enabled": true},
			backups: oneBackup,
			live: func(t *testing.T, c *config.Config) {
				if c.Backup.S3.SecretKey != "OLDSECRET" || !c.Backup.Enabled {
					t.Errorf("live secret=%q enabled=%v, want OLDSECRET/true", c.Backup.S3.SecretKey, c.Backup.Enabled)
				}
			},
		},
		{
			// E8 + L7: a new secret is a storage change, so enabling tests the
			// storage first. This rig's endpoint does not resolve: 400, nothing
			// written.
			name: "enable_same_storage_new_secret",
			seed: seedStorage, body: storageBody(true, "  NEWSECRET  "),
			status: 400, resp: map[string]any{"ok": false}, wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.Backup.S3.SecretKey != "OLDSECRET" || c.Backup.Enabled {
					t.Errorf("live secret=%q enabled=%v, want untouched", c.Backup.S3.SecretKey, c.Backup.Enabled)
				}
			},
		},
		{
			// A blank interval is 1h.
			name:   "blank_interval_is_1h",
			body:   map[string]any{"backups": map[string]any{"enabled": false, "schedule_interval": ""}},
			status: 200, resp: okSaved([]any{"Backups"}, nil), wrote: true,
			saved:   map[string]any{"backup.schedule_interval": "1h0m0s"},
			backups: oneBackup,
		},
		{
			// E8: enabling with changed storage needs the service to test it;
			// with none, 501 and nothing written.
			name: "nil_service_enable_changed_storage_501", noBackup: true,
			seed: seedStorage,
			body: map[string]any{"backups": map[string]any{"enabled": true, "schedule_interval": "1h",
				"endpoint": "http://other.pin.invalid", "bucket": "pin-bucket", "region": "us-east-1",
				"access_key": "PINKEY", "secret_key": "OLDSECRET"}},
			status: 501, resp: map[string]any{"ok": false, "error": "backup service unavailable"}, wrote: false,
			live: func(t *testing.T, c *config.Config) {
				if c.Backup.Enabled || c.Backup.S3.Endpoint != "http://s3.pin.invalid" {
					t.Errorf("live enabled=%v endpoint=%q, want untouched", c.Backup.Enabled, c.Backup.S3.Endpoint)
				}
			},
		},
		{
			// E8: disabling needs no service.
			name: "nil_service_disable_saves", noBackup: true,
			seed: seedStorage, body: storageBody(false, "OLDSECRET"),
			status: 200, resp: okSaved([]any{"Backups"}, nil), wrote: true,
			saved: map[string]any{"backup.enabled": false},
		},
		{
			// R3 / §33 rule 8: the Remove control clears the saved secret.
			name: "remove_secret_clears",
			seed: seedStorage,
			body: map[string]any{"backups": map[string]any{"enabled": false, "schedule_interval": "1h",
				"endpoint": "http://s3.pin.invalid", "bucket": "pin-bucket", "region": "us-east-1",
				"access_key": "PINKEY", "secret_key": "", "remove_secret": true}},
			status: 200, resp: okSaved([]any{"Backups"}, nil), wrote: true,
			saved:   map[string]any{"backup.s3.secret_key": ""},
			backups: oneBackup,
		},
	})
}

// TestPinEdgeConfig_BackupEnableChangedStorage: enabling with a changed
// endpoint now tests the storage first (E8). The endpoint is a local server
// that counts every request and answers 500, so the test fails: 400, nothing
// written, no backup requested.
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
	resp := doRequest(t, rig.r, "PUT", "/api/config", map[string]any{"backups": map[string]any{
		"enabled": true, "schedule_interval": "1h", "endpoint": s3.URL, "bucket": "pin-bucket",
		"region": "us-east-1", "access_key": "PINKEY", "secret_key": "OLDSECRET", "use_path_style": true,
	}}, rig.cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	var got map[string]any
	decodeJSON(t, resp, &got)
	if got["ok"] != false {
		t.Errorf("response = %v, want ok:false", got)
	}
	if _, wrote := rig.saved(t); wrote {
		t.Error("config file written although the storage test failed")
	}
	if n := hits.Load(); n < 1 {
		t.Errorf("storage contacted %d times during the save, want ≥1 (storage check first)", n)
	}
	if got := rig.backups.got(); len(got) != 0 {
		t.Errorf("backup requests = %q, want none", got)
	}
	rig.cfg.RLock()
	defer rig.cfg.RUnlock()
	if rig.cfg.Backup.Enabled || rig.cfg.Backup.S3.Endpoint != "http://s3.pin.invalid" {
		t.Errorf("live enabled=%v endpoint=%q, want untouched", rig.cfg.Backup.Enabled, rig.cfg.Backup.S3.Endpoint)
	}
}

// TestPinEdgeConfig_ListBackupsNoEndpoint: with the service present and no
// storage set, GET /api/backups is still a 400 — the door is unchanged. E6 is
// on the page, which no longer asks until storage is configured and the list
// is opened (and /api/backups/status carries `configured`, pinned in the
// backup package).
func TestPinEdgeConfig_ListBackupsNoEndpoint(t *testing.T) {
	rig := newPinRig(t, true)
	resp := doRequest(t, rig.r, "GET", "/api/backups", nil, rig.cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "backup endpoint is required")
	if _, wrote := rig.saved(t); wrote {
		t.Error("a list wrote the config file")
	}
}

// ── Shifts (database, not the config file) — unchanged ───────────────────

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
		// An empty array is the operator's "no shifts": every shift goes.
		{"empty_list_deletes_all", []row{}, nil},
		// Out-of-range rows beside an in-range one are skipped; the in-range
		// row is the whole desired set.
		{"out_of_range_beside_in_range_skipped", []row{
			{"shift_number": 2, "name": "Swing", "start_time": "14:00", "end_time": "22:00"},
			{"shift_number": 4, "name": "X", "start_time": "00:00", "end_time": "06:00"},
		}, []int{2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset(t)
			rig := newPinRig(t, true)
			resp := doRequest(t, rig.r, "PUT", "/api/shifts", tc.body, rig.cookie)
			assertStatus(t, resp, 200)
			body, err := io.ReadAll(resp.Body)
			testutil.MustNoErr(t, err, "read response body")
			resp.Body.Close()
			if string(body) != "{\"ok\":true}\n" && string(body) != `{"ok":true}` {
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
	// A non-empty body with no in-range shift_number is refused with nothing
	// written (extra, ui-cleanup 2026-10-07). It used to delete every shift,
	// against the handler's own comment (out_of_range_only_deletes_all).
	t.Run("out_of_range_only_refused", func(t *testing.T) {
		reset(t)
		rig := newPinRig(t, true)
		resp := doRequest(t, rig.r, "PUT", "/api/shifts", []row{
			{"shift_number": 4, "name": "X", "start_time": "00:00", "end_time": "06:00"},
			{"shift_number": 0, "name": "Y", "start_time": "", "end_time": ""},
		}, rig.cookie)
		assertStatus(t, resp, 400)
		resp.Body.Close()
		if got := numbers(t); !reflect.DeepEqual(got, []int{1, 2, 3}) {
			t.Errorf("shifts left = %v, want untouched", got)
		}
		if got := rig.backups.got(); len(got) != 0 {
			t.Errorf("backup requests = %q, want none", got)
		}
	})
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
	}{
		{name: "ok", old: "password", next: "newpassword123", status: 200,
			resp: map[string]any{"status": "ok"}, changedTo: "newpassword123"},
		// U3 Account: an empty new password is a 400 server-side; the hash is
		// unchanged (it used to become the hash of "").
		{name: "empty_new_refused", old: "password", next: "", status: 400,
			resp: map[string]any{"error": "new password is required"}},
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

// ── The one save path (U3) ───────────────────────────────────────────────

// TestPinEdgeConfig_OneSaveManySections: a page save touching every section
// is ONE file write and ONE backup request (the old doors made one each), and
// each live apply fires once. The restart notice names every restart-only
// field that moved.
func TestPinEdgeConfig_OneSaveManySections(t *testing.T) {
	rig := newPinRig(t, true)
	rig.cfg.Lock()
	seedPinUID(rig.cfg)
	rig.cfg.Unlock()
	rig.boot()
	resp := doRequest(t, rig.r, "PUT", "/api/config", map[string]any{
		"station":   map[string]any{"station_uid": "pin.uid2", "timezone": "America/Chicago", "auto_confirm": true},
		"core":      map[string]any{"core_api": "http://core.test:8080"},
		"plc":       map[string]any{"host": "wl.test", "port": 9000, "enabled": true, "mode": "sse"},
		"messaging": map[string]any{"brokers": []string{"b1:9092"}},
		"backups":   map[string]any{"enabled": false, "schedule_interval": "2h"},
	}, rig.cookie)
	assertStatus(t, resp, 200)
	var got map[string]any
	decodeJSON(t, resp, &got)
	if !reflect.DeepEqual(got["restart"], []any{"Station UID", "Plant timezone", "Core address"}) {
		t.Errorf("restart = %#v", got["restart"])
	}
	if !reflect.DeepEqual(got["applied"], []any{"Station", "PLC link", "Messaging", "Backups"}) {
		t.Errorf("applied = %#v", got["applied"])
	}
	if got := rig.backups.got(); !reflect.DeepEqual(got, oneBackup) {
		t.Errorf("backup requests = %q, want one", got)
	}
	if rig.spy.reconnects != 1 || rig.spy.applies != 1 {
		t.Errorf("reconnects=%d applies=%d, want 1/1", rig.spy.reconnects, rig.spy.applies)
	}
	m, wrote := rig.saved(t)
	if !wrote {
		t.Fatal("not written")
	}
	for path, want := range map[string]any{"station_uid": "pin.uid2", "timezone": "America/Chicago",
		"core_api": "http://core.test:8080", "warlink.host": "wl.test", "backup.schedule_interval": "2h0m0s"} {
		if v, _ := yamlAt(m, path); v != want {
			t.Errorf("saved %s = %#v, want %#v", path, v, want)
		}
	}
}

// TestPinEdgeConfig_ErrorAnywhereWritesNothing: one bad section refuses the
// whole save; the good sections beside it are not applied either.
func TestPinEdgeConfig_ErrorAnywhereWritesNothing(t *testing.T) {
	rig := newPinRig(t, true)
	rig.cfg.Lock()
	rig.cfg.CoreAPI = "http://old.test"
	rig.cfg.Unlock()
	resp := doRequest(t, rig.r, "PUT", "/api/config", map[string]any{
		"core": map[string]any{"core_api": "http://new.test"},
		"plc":  map[string]any{"mode": "invalid"},
	}, rig.cookie)
	assertStatus(t, resp, 400)
	resp.Body.Close()
	if _, wrote := rig.saved(t); wrote {
		t.Error("written")
	}
	rig.cfg.RLock()
	defer rig.cfg.RUnlock()
	if rig.cfg.CoreAPI != "http://old.test" {
		t.Errorf("live CoreAPI = %q, want untouched", rig.cfg.CoreAPI)
	}
	if n := len(rig.backups.got()); n != 0 || rig.spy.applies != 0 {
		t.Errorf("backups=%d applies=%d, want 0/0", n, rig.spy.applies)
	}
}

// TestPinEdgeConfig_UnknownSection: a section the door does not know is a
// 400 naming it, not a silent drop.
func TestPinEdgeConfig_UnknownSection(t *testing.T) {
	rig := newPinRig(t, true)
	resp := doRequest(t, rig.r, "PUT", "/api/config", map[string]any{"fleet": map[string]any{}}, rig.cookie)
	assertStatus(t, resp, 400)
	assertJSONPath(t, resp, "error", `unknown section "fleet"`)
	if _, wrote := rig.saved(t); wrote {
		t.Error("written")
	}
}

// TestPinEdgeConfig_GroupIDSurvivesSave: KafkaConfig.GroupID is yaml:"-", set
// once at boot. The save's copy is made in Go, so a save keeps it; a yaml
// clone would have emptied the consumer group at the next ReconnectKafka.
func TestPinEdgeConfig_GroupIDSurvivesSave(t *testing.T) {
	rig := newPinRig(t, true)
	rig.cfg.Lock()
	rig.cfg.Messaging.Kafka.GroupID = "shingo-edge-pin"
	rig.cfg.Unlock()
	resp := doRequest(t, rig.r, "PUT", "/api/config",
		map[string]any{"messaging": map[string]any{"brokers": []string{"b1:9092"}}}, rig.cookie)
	assertStatus(t, resp, 200)
	resp.Body.Close()
	rig.cfg.RLock()
	defer rig.cfg.RUnlock()
	if rig.cfg.Messaging.Kafka.GroupID != "shingo-edge-pin" {
		t.Errorf("GroupID after save = %q, want kept", rig.cfg.Messaging.Kafka.GroupID)
	}
}

// TestPinEdgeConfig_SaveWaitsForSaveMutex: the door holds the config's save
// mutex across copy → write → swap, the one adoptPlantTimezone also takes. A
// save started while another holder has it does not write until released.
func TestPinEdgeConfig_SaveWaitsForSaveMutex(t *testing.T) {
	rig := newPinRig(t, true)
	rig.cfg.LockSave()
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("PUT", "/api/config",
			strings.NewReader(`{"core":{"core_api":"http://late.test"}}`)).WithContext(context.Background())
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(rig.cookie)
		w := httptest.NewRecorder()
		rig.r.ServeHTTP(w, req)
		done <- w.Code
	}()
	select {
	case code := <-done:
		rig.cfg.UnlockSave()
		t.Fatalf("save finished (%d) while the save mutex was held", code)
	case <-time.After(150 * time.Millisecond):
	}
	if _, wrote := rig.saved(t); wrote {
		t.Error("written while the save mutex was held")
	}
	rig.cfg.UnlockSave()
	if code := <-done; code != 200 {
		t.Errorf("status after release = %d, want 200", code)
	}
	if _, wrote := rig.saved(t); !wrote {
		t.Error("not written after release")
	}
}

// TestPinEdgeConfig_RestartNoticeAdoptedZone: after adoptPlantTimezone fills a
// blank zone, the notice lists Plant timezone though nobody saved. That is
// correct (the display stays on the boot zone) and is not filtered out. No
// snapshot (a Handlers built without NewRouter): no notice.
func TestPinEdgeConfig_RestartNoticeAdoptedZone(t *testing.T) {
	rig := newPinRig(t, false)
	if got := rig.h.restartPending(); len(got) != 0 {
		t.Errorf("no snapshot: notice = %q, want none", got)
	}
	rig.boot()
	if got := rig.h.restartPending(); len(got) != 0 {
		t.Errorf("fresh boot: notice = %q, want none", got)
	}
	rig.cfg.Lock()
	rig.cfg.Timezone = "America/Chicago" // what adoptPlantTimezone writes
	rig.cfg.Unlock()
	if got := rig.h.restartPending(); !reflect.DeepEqual(got, []string{"Plant timezone"}) {
		t.Errorf("after adoption: notice = %q, want [Plant timezone]", got)
	}
}

// TestPinEdgeConfig_LegacyOnlyStationSaves: an Edge whose only identity is the
// legacy Messaging.StationID saves its Station section (timezone,
// auto-confirm) with that id carried unchanged in the body, as the page sends
// it (lead ruling on E1, 2026-10-07): 200, the legacy id untouched, no UID
// invented.
func TestPinEdgeConfig_LegacyOnlyStationSaves(t *testing.T) {
	runPinCases(t, []pinCase{{
		name: "timezone_with_legacy_id",
		seed: func(c *config.Config) { c.StationUID = ""; c.Messaging.StationID = "plant-a.line-1" },
		body: map[string]any{"station": map[string]any{"station_uid": "", "station_id": "plant-a.line-1",
			"timezone": "America/Chicago", "auto_confirm": true}},
		status: 200, resp: okSaved([]any{"Station"}, []any{"Plant timezone"}), wrote: true,
		saved: map[string]any{"timezone": "America/Chicago", "messaging.station_id": "plant-a.line-1",
			"station_uid": "", "web.auto_confirm": true},
		backups: oneBackup,
		live: func(t *testing.T, c *config.Config) {
			if c.StationUID != "" || c.Messaging.StationID != "plant-a.line-1" {
				t.Errorf("live uid=%q legacy=%q, want blank/plant-a.line-1", c.StationUID, c.Messaging.StationID)
			}
		},
	}})
}

// TestRace_ConfigSaveLeavesUnchangedFieldsAlone: a door save of one section
// while a goroutine reads OTHER fields of the live config lock-free, as the
// engine's loops do (e.cfg.Web.AutoConfirm, e.cfg.WarLink in
// ApplyWarLinkConfig, cfg.StationID()). The swap writes only the leaves the
// save changed, so -race stays clean. Run: go test -race -run TestRace_ ./www/
func TestRace_ConfigSaveLeavesUnchangedFieldsAlone(t *testing.T) {
	rig := newPinRig(t, false)
	rig.cfg.Lock()
	rig.cfg.StationUID = "pin.uid"
	rig.cfg.Messaging.Kafka.Brokers = []string{"b1:9092"}
	rig.cfg.Unlock()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = rig.cfg.Web.AutoConfirm
			_ = rig.cfg.WarLink.Host
			_ = rig.cfg.WarLink.Enabled
			_ = rig.cfg.StationUID
			_ = len(rig.cfg.Messaging.Kafka.Brokers)
		}
	}()
	for i := 0; i < 5; i++ {
		resp := doRequest(t, rig.r, "PUT", "/api/config",
			map[string]any{"core": map[string]string{"core_api": fmt.Sprintf("http://core.test:%d", 8080+i)}}, rig.cookie)
		assertStatus(t, resp, 200)
		resp.Body.Close()
	}
	close(stop)
	wg.Wait()
	rig.cfg.RLock()
	defer rig.cfg.RUnlock()
	if rig.cfg.CoreAPI != "http://core.test:8084" {
		t.Errorf("live CoreAPI = %q", rig.cfg.CoreAPI)
	}
}
