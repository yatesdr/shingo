//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
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

// Characterization tests for handlers_payloads.go — JSON endpoints that read
// payload templates, manage per-bin manifest items, and list/register bins.
// The HTTP surface is the contract; tests drive through the handler methods
// with postJSON + parseIDParam-style GETs and assert on body + DB state.

// --- shared test helpers (used by handlers_payloads_test.go and the rest of
// the cluster: payload_templates, config, corrections, diagnostics, dashboard,
// telemetry).

// getPlain drives a GET handler at the given URL (path + query) and returns
// the recorder. The handler itself reads URL.Query() and chi route params; for
// chi-bound paths use postJSONWithChi or its sibling getPlainWithChi helper.
func getPlain(t *testing.T, handler http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// postRaw posts a raw body (string of bytes) to a handler — useful for the
// invalid-JSON branch where postJSON would otherwise marshal valid JSON.
func postRaw(t *testing.T, handler http.HandlerFunc, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// assertJSONError fails the test if the body doesn't decode to {"error": <substr>}
// containing the given substring.
func assertJSONError(t *testing.T, body []byte, wantSubstr string) {
	t.Helper()
	var env map[string]string
	if err := json.Unmarshal(body, &env); err != nil {
		t.Errorf("decode json error envelope: %v; body=%s", err, body)
		return
	}
	if got := env["error"]; got == "" {
		t.Errorf("missing 'error' field in body=%s", body)
	} else if !strings.Contains(got, wantSubstr) {
		t.Errorf("error message: got %q, want substring %q", got, wantSubstr)
	}
}

// assertJSONStatus fails the test if the body doesn't decode to {"status": <want>}.
func assertJSONStatus(t *testing.T, body []byte, want string) {
	t.Helper()
	var env map[string]string
	if err := json.Unmarshal(body, &env); err != nil {
		t.Errorf("decode json status envelope: %v; body=%s", err, body)
		return
	}
	if env["status"] != want {
		t.Errorf("status: got %q, want %q", env["status"], want)
	}
}

// testHandlersForPages is testHandlers with a non-nil (but unconnected)
// messaging client and the full templates loaded. Required by page handlers
// (handleDashboard, handleDiagnostics, handleConfig, handlePayloadsPage) and
// apiHealthCheck, which call MsgClient().IsConnected() (not nil-safe) and/or
// h.render.
func testHandlersForPages(t *testing.T) (*Handlers, *store.DB) {
	t.Helper()

	db := testdb.Open(t)
	sim := simulator.New()

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"
	msgClient := messaging.NewClient(&cfg.Messaging)

	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: msgClient,
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
		sessions: newSessionStore("test-secret"),
		tmpls:    make(map[string]*template.Template),
		eventHub: hub,
		debugLog: dbgLog,
	}
	loadTestTemplates(t, h)
	return h, db
}

// loadTestTemplates mirrors the template parsing in router.NewRouter so that
// page handlers (handleDashboard, handleDiagnostics, handleConfig, handlePayloadsPage)
// can render without a full router setup. Call this after testHandlers(t) when
// the handler under test renders HTML.
func loadTestTemplates(t *testing.T, h *Handlers) {
	t.Helper()
	base := template.New("").Funcs(templateFuncs())
	base = template.Must(base.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	for _, p := range pages {
		name := p[len("templates/"):]
		if name == "layout.html" {
			continue
		}
		clone := template.Must(base.Clone())
		clone = template.Must(clone.ParseFS(templateFS, p))
		h.tmpls[name] = clone
	}
}

// --- apiListPayloads --------------------------------------------------------

func TestApiListPayloads_ReturnsAll(t *testing.T) {
	h, db := testHandlers(t)
	// Seed two payloads. testdb.SetupStandardData creates PART-A.
	sd := testdb.SetupStandardData(t, db)
	extra := &store.Payload{Code: "PART-B", Description: "second"}
	if err := db.CreatePayload(extra); err != nil {
		t.Fatalf("seed payload: %v", err)
	}

	rec := getPlain(t, h.apiListPayloads, "/api/payloads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var payloads []*store.Payload
	if err := json.NewDecoder(rec.Body).Decode(&payloads); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payloads) < 2 {
		t.Errorf("expected at least 2 payloads, got %d", len(payloads))
	}
	seen := map[string]bool{}
	for _, p := range payloads {
		seen[p.Code] = true
	}
	if !seen[sd.Payload.Code] || !seen["PART-B"] {
		t.Errorf("missing expected payloads; got %v", seen)
	}
}

// --- apiGetPayload ----------------------------------------------------------

func TestApiGetPayload_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := getPlain(t, h.apiGetPayload, "/api/payloads/detail?id="+fmt.Sprint(sd.Payload.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got store.Payload
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != sd.Payload.Code {
		t.Errorf("code: got %q, want %q", got.Code, sd.Payload.Code)
	}
}

func TestApiGetPayload_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiGetPayload, "/api/payloads/detail?id=notanint")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "invalid id")
}

func TestApiGetPayload_NotFound(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiGetPayload, "/api/payloads/detail?id=9999999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "not found")
}

// --- apiListManifest --------------------------------------------------------

func TestApiListManifest_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	// Seed 2 manifest items.
	if err := db.CreatePayloadManifestItem(&store.PayloadManifestItem{
		PayloadID: sd.Payload.ID, PartNumber: "P1", Quantity: 3,
	}); err != nil {
		t.Fatalf("create manifest item: %v", err)
	}
	if err := db.CreatePayloadManifestItem(&store.PayloadManifestItem{
		PayloadID: sd.Payload.ID, PartNumber: "P2", Quantity: 5,
	}); err != nil {
		t.Fatalf("create manifest item: %v", err)
	}

	rec := getPlain(t, h.apiListManifest, "/api/payloads/manifest?id="+fmt.Sprint(sd.Payload.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var items []*store.PayloadManifestItem
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("got %d items, want 2", len(items))
	}
}

func TestApiListManifest_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiListManifest, "/api/payloads/manifest?id=xyz")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiCreateManifestItem --------------------------------------------------

func TestApiCreateManifestItem_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiCreateManifestItem, "/api/payloads/manifest/create",
		map[string]any{
			"payload_id":  sd.Payload.ID,
			"part_number": "PART-X",
			"quantity":    7,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var created store.PayloadManifestItem
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == 0 || created.PartNumber != "PART-X" || created.Quantity != 7 {
		t.Errorf("created: got %+v", created)
	}

	// Verify persistence.
	items, err := db.ListPayloadManifest(sd.Payload.ID)
	if err != nil {
		t.Fatalf("list manifest: %v", err)
	}
	found := false
	for _, it := range items {
		if it.PartNumber == "PART-X" && it.Quantity == 7 {
			found = true
		}
	}
	if !found {
		t.Errorf("manifest item PART-X not persisted; got %d items", len(items))
	}
}

func TestApiCreateManifestItem_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiCreateManifestItem, "/api/payloads/manifest/create", []byte("not-json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- apiUpdateManifestItem --------------------------------------------------

func TestApiUpdateManifestItem_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	item := &store.PayloadManifestItem{PayloadID: sd.Payload.ID, PartNumber: "OLD", Quantity: 1}
	if err := db.CreatePayloadManifestItem(item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := postJSON(t, h.apiUpdateManifestItem, "/api/payloads/manifest/update",
		map[string]any{"id": item.ID, "part_number": "NEW", "quantity": 9})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	items, _ := db.ListPayloadManifest(sd.Payload.ID)
	if len(items) != 1 || items[0].PartNumber != "NEW" || items[0].Quantity != 9 {
		t.Errorf("after update: got %+v", items)
	}
}

// --- apiDeleteManifestItem --------------------------------------------------

func TestApiDeleteManifestItem_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	item := &store.PayloadManifestItem{PayloadID: sd.Payload.ID, PartNumber: "GONE", Quantity: 1}
	if err := db.CreatePayloadManifestItem(item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := postJSON(t, h.apiDeleteManifestItem, "/api/payloads/manifest/delete",
		map[string]any{"id": item.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	items, _ := db.ListPayloadManifest(sd.Payload.ID)
	if len(items) != 0 {
		t.Errorf("expected 0 items after delete, got %d", len(items))
	}
}

func TestApiDeleteManifestItem_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiDeleteManifestItem, "/api/payloads/manifest/delete", []byte("{"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiConfirmManifest -----------------------------------------------------

func TestApiConfirmManifest_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CONFIRM-1")
	// Unconfirm first.
	if err := db.UnconfirmBinManifest(bin.ID); err != nil {
		t.Fatalf("unconfirm: %v", err)
	}

	rec := postJSON(t, h.apiConfirmManifest, "/api/payloads/confirm-manifest",
		map[string]any{"id": bin.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetBin(bin.ID)
	if !got.ManifestConfirmed {
		t.Error("manifest should be confirmed after apiConfirmManifest")
	}
}

// --- apiListPayloadEvents ---------------------------------------------------

func TestApiListPayloadEvents_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-AUDIT-1")
	// seed audit entry
	if err := db.AppendAudit("bin", bin.ID, "test-action", "old", "new", "tester"); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	rec := getPlain(t, h.apiListPayloadEvents, "/api/payloads/events?id="+fmt.Sprint(bin.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var entries []*store.AuditEntry
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one audit entry")
	}
}

func TestApiListPayloadEvents_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiListPayloadEvents, "/api/payloads/events?id=bogus")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiPayloadsByNode ------------------------------------------------------

func TestApiPayloadsByNode_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-NODE-1")

	rec := getPlain(t, h.apiPayloadsByNode, "/api/payloads/by-node?id="+fmt.Sprint(sd.StorageNode.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var bins []*store.Bin
	if err := json.NewDecoder(rec.Body).Decode(&bins); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bins) != 1 || bins[0].ID != bin.ID {
		t.Errorf("bins at node: got %+v, want single bin id=%d", bins, bin.ID)
	}
}

func TestApiPayloadsByNode_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiPayloadsByNode, "/api/payloads/by-node?id=abc")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiBulkRegisterBins ----------------------------------------------------

func TestApiBulkRegisterBins_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiBulkRegisterBins, "/api/bins/bulk-register",
		map[string]any{
			"bin_type_id": sd.BinType.ID,
			"count":       3,
			"prefix":      "BULK-",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Created int     `json:"created"`
		IDs     []int64 `json:"ids"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Created != 3 || len(resp.IDs) != 3 {
		t.Errorf("response: got %+v, want created=3", resp)
	}

	// Verify DB has labels BULK-0001..BULK-0003
	for i := 0; i < 3; i++ {
		label := fmt.Sprintf("BULK-%04d", i+1)
		got, err := db.GetBinByLabel(label)
		if err != nil || got == nil {
			t.Errorf("bin %q not found: %v", label, err)
		}
	}
}

func TestApiBulkRegisterBins_CountOutOfRange(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	cases := []struct {
		name  string
		count int
	}{
		{"zero", 0},
		{"negative", -1},
		{"too-many", 101},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, h.apiBulkRegisterBins, "/api/bins/bulk-register",
				map[string]any{
					"bin_type_id": sd.BinType.ID,
					"count":       tc.count,
					"prefix":      "BAD-",
				})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertJSONError(t, rec.Body.Bytes(), "count must be 1-100")
		})
	}
}

func TestApiBulkRegisterBins_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiBulkRegisterBins, "/api/bins/bulk-register", []byte("{bad"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}
