//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// Characterization tests for handlers_rds_explorer.go — handleFleetExplorer
// (renders the RDS explorer page) and apiFleetProxy (forwards requests to a
// vendor base URL exposed by fleet.VendorProxy). The simulator does not
// implement VendorProxy, so a thin wrapper backend is used to point the proxy
// at an httptest.NewServer fake RDS.

// --- fakeVendorProxyFleet ---------------------------------------------------

// fakeVendorProxyFleet wraps a SimulatorBackend and exposes BaseURL() so the
// fleet proxy handler treats it as a VendorProxy-capable backend.
type fakeVendorProxyFleet struct {
	*simulator.SimulatorBackend
	baseURL string
}

var _ fleet.Backend = (*fakeVendorProxyFleet)(nil)
var _ fleet.VendorProxy = (*fakeVendorProxyFleet)(nil)

func (f *fakeVendorProxyFleet) BaseURL() string { return f.baseURL }

// testHandlersWithVendorProxy builds a Handlers backed by a fakeVendorProxyFleet
// pointing at the supplied baseURL.
func testHandlersWithVendorProxy(t *testing.T, baseURL string) (*Handlers, *store.DB) {
	t.Helper()
	db := testdb.Open(t)
	sim := &fakeVendorProxyFleet{
		SimulatorBackend: simulator.New(),
		baseURL:          baseURL,
	}

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"

	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: nil,
		LogFunc:   t.Logf,
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
	return h, db
}

// --- handleFleetExplorer ----------------------------------------------------

// TestHandleFleetExplorer_RendersHTML pins that the explorer page renders.
// With the simulator backend (no VendorProxy), FleetBaseURL is empty but the
// page still renders.
func TestHandleFleetExplorer_RendersHTML(t *testing.T) {
	h, _ := testHandlersForPages(t)

	req := httptest.NewRequest(http.MethodGet, "/fleet-explorer", nil)
	rec := httptest.NewRecorder()
	h.handleFleetExplorer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Fleet Explorer") {
		t.Errorf("expected 'Fleet Explorer' heading; len=%d", len(body))
	}
}

// TestHandleFleetExplorer_ExposesBaseURL pins that when the backend
// satisfies VendorProxy, its BaseURL() flows into the rendered template via
// FleetBaseURL.
func TestHandleFleetExplorer_ExposesBaseURL(t *testing.T) {
	h, _ := testHandlersWithVendorProxy(t, "http://fake-rds.local:9999")

	req := httptest.NewRequest(http.MethodGet, "/fleet-explorer", nil)
	rec := httptest.NewRecorder()
	h.handleFleetExplorer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "http://fake-rds.local:9999") {
		t.Errorf("expected vendor base URL in HTML; not found")
	}
}

// --- apiFleetProxy ----------------------------------------------------------

// TestApiFleetProxy_RejectsGET pins the method guard: only POST is allowed.
func TestApiFleetProxy_RejectsGET(t *testing.T) {
	h, _ := testHandlers(t)

	req := httptest.NewRequest(http.MethodGet, "/api/fleet/proxy", nil)
	rec := httptest.NewRecorder()
	h.apiFleetProxy(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "POST required")
}

// TestApiFleetProxy_BackendNotSupported pins the 501 path: simulator does not
// implement VendorProxy.
func TestApiFleetProxy_BackendNotSupported(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiFleetProxy, "/api/fleet/proxy",
		map[string]any{"method": "GET", "path": "/anything"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "does not support API proxy")
}

// TestApiFleetProxy_InvalidJSON pins the 400 path on bad body when proxy is
// supported (we have to hit the post-501 codepath).
func TestApiFleetProxy_InvalidJSON(t *testing.T) {
	h, _ := testHandlersWithVendorProxy(t, "http://unused.local")

	rec := postRaw(t, h.apiFleetProxy, "/api/fleet/proxy", []byte("not-json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiFleetProxy_ForwardsAndReturnsJSON pins the JSON-passthrough happy
// path: the handler forwards the (method, path) pair to the vendor base URL,
// then echoes the upstream JSON body back to the client under "body".
func TestApiFleetProxy_ForwardsAndReturnsJSON(t *testing.T) {
	gotMethod := ""
	gotPath := ""
	gotBody := ""

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok": true, "echo": "hello"}`)
	}))
	t.Cleanup(upstream.Close)

	h, _ := testHandlersWithVendorProxy(t, upstream.URL)

	rec := postJSON(t, h.apiFleetProxy, "/api/fleet/proxy",
		map[string]any{
			"method": "POST",
			"path":   "/some/endpoint",
			"body":   `{"hello":"world"}`,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if gotMethod != "POST" {
		t.Errorf("upstream method: got %q, want POST", gotMethod)
	}
	if gotPath != "/some/endpoint" {
		t.Errorf("upstream path: got %q, want /some/endpoint", gotPath)
	}
	if gotBody != `{"hello":"world"}` {
		t.Errorf("upstream body: got %q", gotBody)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status_code"].(float64) != 200 {
		t.Errorf("status_code: got %v, want 200", resp["status_code"])
	}
	if resp["method"] != "POST" {
		t.Errorf("method echo: got %v", resp["method"])
	}
	if resp["url"] != upstream.URL+"/some/endpoint" {
		t.Errorf("url echo: got %v", resp["url"])
	}
	body, ok := resp["body"].(map[string]any)
	if !ok {
		t.Fatalf("body should be a JSON object; got %T = %v", resp["body"], resp["body"])
	}
	if body["ok"] != true || body["echo"] != "hello" {
		t.Errorf("upstream JSON body did not propagate: %+v", body)
	}
}

// TestApiFleetProxy_NonJSONBody pins the body_text fallback: when the upstream
// returns non-JSON content, the response gets a body_text field instead of body.
func TestApiFleetProxy_NonJSONBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "plain text response")
	}))
	t.Cleanup(upstream.Close)

	h, _ := testHandlersWithVendorProxy(t, upstream.URL)

	rec := postJSON(t, h.apiFleetProxy, "/api/fleet/proxy",
		map[string]any{"method": "GET", "path": "/whatever"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, hasBody := resp["body"]; hasBody {
		t.Errorf("non-JSON upstream should NOT set 'body'; got %+v", resp)
	}
	if resp["body_text"] != "plain text response" {
		t.Errorf("body_text: got %v", resp["body_text"])
	}
}

// TestApiFleetProxy_DefaultsMethodAndPath pins the parser defaults: empty
// method becomes GET and a path without leading "/" gets one prepended.
func TestApiFleetProxy_DefaultsMethodAndPath(t *testing.T) {
	gotMethod := ""
	gotPath := ""

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstream.Close)

	h, _ := testHandlersWithVendorProxy(t, upstream.URL)

	rec := postJSON(t, h.apiFleetProxy, "/api/fleet/proxy",
		map[string]any{"method": "", "path": "no-slash"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotMethod != "GET" {
		t.Errorf("upstream method (default): got %q, want GET", gotMethod)
	}
	if gotPath != "/no-slash" {
		t.Errorf("upstream path (default): got %q, want /no-slash", gotPath)
	}
}

// TestApiFleetProxy_UpstreamUnreachable pins the network-error envelope: the
// handler still returns 200 (the error is in the body) with status_code:0.
func TestApiFleetProxy_UpstreamUnreachable(t *testing.T) {
	// 127.0.0.1:1 is reliably refused on Linux test runners.
	h, _ := testHandlersWithVendorProxy(t, "http://127.0.0.1:1")

	rec := postJSON(t, h.apiFleetProxy, "/api/fleet/proxy",
		map[string]any{"method": "GET", "path": "/x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (error in body); body=%s",
			rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status_code"].(float64) != 0 {
		t.Errorf("status_code on connect error: got %v, want 0", resp["status_code"])
	}
	if _, hasErr := resp["error"]; !hasErr {
		t.Errorf("expected error field in body; got %+v", resp)
	}
}

// --- flattenHeaders helper --------------------------------------------------

// TestFlattenHeaders pins the helper used to convert http.Header into a
// flat map[string]string for JSON response bodies.
func TestFlattenHeaders(t *testing.T) {
	hdr := http.Header{}
	hdr.Add("X-Single", "v1")
	hdr.Add("X-Multi", "a")
	hdr.Add("X-Multi", "b")

	flat := flattenHeaders(hdr)
	if flat["X-Single"] != "v1" {
		t.Errorf("X-Single: got %q, want v1", flat["X-Single"])
	}
	if flat["X-Multi"] != "a, b" {
		t.Errorf("X-Multi: got %q, want 'a, b'", flat["X-Multi"])
	}
}
