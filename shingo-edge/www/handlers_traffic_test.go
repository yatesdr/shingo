package www

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"shingoedge/config"

	"github.com/go-chi/chi/v5"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — mirrors routes from router.go that bind to
// handlers_traffic.go. All four API endpoints and the /traffic page are
// admin-gated in production; this harness preserves that gating so we
// can exercise both the auth-redirect path and the authenticated path.
//
// handleTraffic itself is not invoked from tests: it calls
// PLCManager().PLCNames() and renderTemplate, neither of which the
// stubEngine supports. The page is covered only via its admin-middleware
// gate (unauthenticated → 303).
// ═══════════════════════════════════════════════════════════════════════

func newTrafficRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	// Admin page group
	r.Group(func(r chi.Router) {
		r.Use(h.adminMiddleware)
		r.Get("/traffic", h.handleTraffic)
	})

	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(h.adminMiddleware)
			r.Get("/traffic/bindings", h.apiTrafficBindings)
			r.Put("/traffic/heartbeat", h.apiTrafficSaveHeartbeat)
			r.Post("/traffic/bindings", h.apiTrafficAddBinding)
			r.Post("/traffic/bindings/delete", h.apiTrafficDeleteBinding)
		})
	})

	return h, r
}

// ═══════════════════════════════════════════════════════════════════════
// Admin gating
// ═══════════════════════════════════════════════════════════════════════

func TestApiTraffic_Page_RedirectsWhenUnauthenticated(t *testing.T) {
	_, router := newTrafficRouter(t)

	resp := doRequest(t, router, "GET", "/traffic", nil, nil)
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Errorf("expected 303/302 redirect to login, got %d", resp.StatusCode)
	}
}

func TestApiTraffic_Bindings_Unauthenticated(t *testing.T) {
	_, router := newTrafficRouter(t)

	// HX-Request header flips the middleware from redirect → 401.
	req := httptest.NewRequest("GET", "/api/traffic/bindings", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	resp := w.Result()
	assertStatus(t, resp, http.StatusUnauthorized)
}

// ═══════════════════════════════════════════════════════════════════════
// apiTrafficBindings — returns current config.CountGroups as JSON.
// Engine call site: AppConfig().
// ═══════════════════════════════════════════════════════════════════════

func TestApiTrafficBindings_Success(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	// Seed a binding via direct config mutation so we can assert round-trip.
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.HeartbeatTag = "HB.Tag"
	cfg.CountGroups.HeartbeatPLC = "PLC-1"
	cfg.CountGroups.Bindings = map[string]config.Binding{
		"groupA": {PLC: "PLC-1", RequestTag: "Req.A"},
	}
	cfg.Unlock()

	resp := doRequest(t, router, "GET", "/api/traffic/bindings", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var out struct {
		HeartbeatTag string                    `json:"heartbeat_tag"`
		HeartbeatPLC string                    `json:"heartbeat_plc"`
		Bindings     map[string]config.Binding `json:"bindings"`
	}
	decodeJSON(t, resp, &out)
	if out.HeartbeatTag != "HB.Tag" {
		t.Errorf("heartbeat_tag: got %q, want HB.Tag", out.HeartbeatTag)
	}
	if out.HeartbeatPLC != "PLC-1" {
		t.Errorf("heartbeat_plc: got %q, want PLC-1", out.HeartbeatPLC)
	}
	got, ok := out.Bindings["groupA"]
	if !ok {
		t.Fatal("expected binding groupA in response")
	}
	if got.PLC != "PLC-1" || got.RequestTag != "Req.A" {
		t.Errorf("binding groupA: got %+v", got)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// apiTrafficSaveHeartbeat — PUT /traffic/heartbeat.
// Engine call sites: AppConfig(), ConfigPath() (for cfg.Save).
// ═══════════════════════════════════════════════════════════════════════

func TestApiTrafficSaveHeartbeat_Success(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{
		"heartbeat_tag": "  New.Heartbeat.Tag  ", // whitespace should be trimmed
		"heartbeat_plc": "  NewPLC  ",
	}
	resp := doRequest(t, router, "PUT", "/api/traffic/heartbeat", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Config state updated (post-trim).
	cfg := h.engine.AppConfig()
	cfg.Lock()
	gotTag := cfg.CountGroups.HeartbeatTag
	gotPLC := cfg.CountGroups.HeartbeatPLC
	cfg.Unlock()
	if gotTag != "New.Heartbeat.Tag" {
		t.Errorf("heartbeat_tag in cfg: got %q, want trimmed value", gotTag)
	}
	if gotPLC != "NewPLC" {
		t.Errorf("heartbeat_plc in cfg: got %q, want trimmed value", gotPLC)
	}

	// Config file actually written to disk.
	if _, err := os.Stat(h.engine.ConfigPath()); err != nil {
		t.Errorf("expected config file written to %s: %v", h.engine.ConfigPath(), err)
	}
}

func TestApiTrafficSaveHeartbeat_InvalidJSON(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"heartbeat_tag": 42} // int into string
	resp := doRequest(t, router, "PUT", "/api/traffic/heartbeat", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// apiTrafficAddBinding — POST /traffic/bindings.
// Engine call sites: AppConfig(), ConfigPath().
// ═══════════════════════════════════════════════════════════════════════

func TestApiTrafficAddBinding_Success(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{
		"name":        "groupNew",
		"plc":         "PLC-2",
		"request_tag": "Req.New",
	}
	resp := doRequest(t, router, "POST", "/api/traffic/bindings", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	cfg := h.engine.AppConfig()
	cfg.Lock()
	b, ok := cfg.CountGroups.Bindings["groupNew"]
	cfg.Unlock()
	if !ok {
		t.Fatal("expected binding groupNew to be persisted")
	}
	if b.PLC != "PLC-2" || b.RequestTag != "Req.New" {
		t.Errorf("binding: got %+v", b)
	}
}

func TestApiTrafficAddBinding_TrimsAndRequiresAllFields(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	cases := []struct {
		name string
		body map[string]string
	}{
		{"missing name", map[string]string{
			"name": "  ", "plc": "PLC", "request_tag": "Req",
		}},
		{"missing plc", map[string]string{
			"name": "grp", "plc": "", "request_tag": "Req",
		}},
		{"missing request_tag", map[string]string{
			"name": "grp", "plc": "PLC", "request_tag": "",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRequest(t, router, "POST", "/api/traffic/bindings", tc.body, cookie)
			assertStatus(t, resp, http.StatusBadRequest)
			assertJSONPath(t, resp, "error", "name, plc, and request_tag are required")
		})
	}
}

func TestApiTrafficAddBinding_DuplicateConflict(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	// Seed an existing binding.
	cfg := h.engine.AppConfig()
	cfg.Lock()
	if cfg.CountGroups.Bindings == nil {
		cfg.CountGroups.Bindings = map[string]config.Binding{}
	}
	cfg.CountGroups.Bindings["dup"] = config.Binding{PLC: "P", RequestTag: "R"}
	cfg.Unlock()

	body := map[string]string{"name": "dup", "plc": "P2", "request_tag": "R2"}
	resp := doRequest(t, router, "POST", "/api/traffic/bindings", body, cookie)
	assertStatus(t, resp, http.StatusConflict)
}

func TestApiTrafficAddBinding_InvalidJSON(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"name": 123}
	resp := doRequest(t, router, "POST", "/api/traffic/bindings", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// apiTrafficDeleteBinding — POST /traffic/bindings/delete.
// Engine call sites: AppConfig(), ConfigPath().
// ═══════════════════════════════════════════════════════════════════════

func TestApiTrafficDeleteBinding_Success(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	// Seed a binding.
	cfg := h.engine.AppConfig()
	cfg.Lock()
	if cfg.CountGroups.Bindings == nil {
		cfg.CountGroups.Bindings = map[string]config.Binding{}
	}
	cfg.CountGroups.Bindings["to-delete"] = config.Binding{PLC: "P", RequestTag: "R"}
	cfg.Unlock()

	body := map[string]string{"name": "to-delete"}
	resp := doRequest(t, router, "POST", "/api/traffic/bindings/delete", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	cfg.Lock()
	_, ok := cfg.CountGroups.Bindings["to-delete"]
	cfg.Unlock()
	if ok {
		t.Error("expected binding to-delete to be removed from config")
	}
}

func TestApiTrafficDeleteBinding_MissingBindingSilentlyOK(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	// Delete a name that doesn't exist — handler does not error.
	body := map[string]string{"name": "never-existed"}
	resp := doRequest(t, router, "POST", "/api/traffic/bindings/delete", body, cookie)
	assertStatus(t, resp, http.StatusOK)
}

func TestApiTrafficDeleteBinding_InvalidJSON(t *testing.T) {
	h, router := newTrafficRouter(t)
	cookie := authCookie(t, h)

	body := map[string]interface{}{"name": 99}
	resp := doRequest(t, router, "POST", "/api/traffic/bindings/delete", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}
