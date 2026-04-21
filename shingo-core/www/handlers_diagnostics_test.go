//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingocore/internal/testdb"
)

// Characterization tests for handlers_diagnostics.go — health check, recon
// summary, outbox, recovery actions, replay, and repair-anomaly dispatch.
// handleDiagnostics (HTML) is also covered.

// --- handleDiagnostics ------------------------------------------------------

func TestHandleDiagnostics_RendersHTML(t *testing.T) {
	h, _ := testHandlersForPages(t)

	req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
	rec := httptest.NewRecorder()
	h.handleDiagnostics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Body should be real HTML — we don't need to pin exact text, but at least
	// the layout should render without error.
	body := rec.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("expected HTML output, got %q", body[:min(200, len(body))])
	}
}

// --- apiHealthCheck ---------------------------------------------------------

func TestApiHealthCheck_HappyPath(t *testing.T) {
	h, _ := testHandlersForPages(t)

	rec := getPlain(t, h.apiHealthCheck, "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Expected fields present.
	for _, field := range []string{"status", "fleet", "messaging", "database", "reconciliation"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("health response missing field %q: %+v", field, resp)
		}
	}
	// Simulator's Ping() returns nil → fleet == true.
	if resp["fleet"] != true {
		t.Errorf("fleet: got %v, want true", resp["fleet"])
	}
	// DB is up via testdb.Open.
	if resp["database"] != true {
		t.Errorf("database: got %v, want true", resp["database"])
	}
	// Messaging client was created but not connected → false.
	if resp["messaging"] != false {
		t.Errorf("messaging: got %v, want false", resp["messaging"])
	}
}

// --- apiReconciliation ------------------------------------------------------

func TestApiReconciliation_HappyPath(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiReconciliation, "/api/reconciliation")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Summary   map[string]any   `json:"summary"`
		Anomalies []map[string]any `json:"anomalies"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Summary == nil {
		t.Error("summary missing")
	}
}

// --- apiListDeadLetterOutbox ------------------------------------------------

func TestApiListDeadLetterOutbox_HappyPath(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListDeadLetterOutbox, "/api/outbox/deadletters")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Body must decode as JSON; shape is []*OutboxMessage or null.
	var list []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// --- apiListRecoveryActions -------------------------------------------------

func TestApiListRecoveryActions_HappyPath(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiListRecoveryActions, "/api/recovery/actions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// --- apiReplayOutbox --------------------------------------------------------

func TestApiReplayOutbox_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiReplayOutbox, "/api/outbox/replay?id=nope")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiReplayOutbox_UnknownID(t *testing.T) {
	h, _ := testHandlers(t)
	// Valid integer but no matching outbox row — UPDATE affects 0 rows and
	// returns no error, so the handler responds 200 (idempotent no-op).
	rec := getPlain(t, h.apiReplayOutbox, "/api/outbox/replay?id=9999999")
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// --- apiRepairAnomaly -------------------------------------------------------

func TestApiRepairAnomaly_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiRepairAnomaly, "/api/recovery/repair", []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiRepairAnomaly_UnknownAction(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiRepairAnomaly, "/api/recovery/repair",
		map[string]any{"action": "no-such-action"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "unknown recovery action")
}

// Each sub-action requires its corresponding id field — missing id returns 400.
func TestApiRepairAnomaly_MissingIDs(t *testing.T) {
	h, _ := testHandlers(t)

	cases := []struct {
		name   string
		body   map[string]any
		substr string
	}{
		{
			name:   "reapply_completion-missing-order",
			body:   map[string]any{"action": "reapply_completion"},
			substr: "order_id is required",
		},
		{
			name:   "release_terminal_claim-missing-bin",
			body:   map[string]any{"action": "release_terminal_claim"},
			substr: "bin_id is required",
		},
		{
			name:   "release_staged_bin-missing-bin",
			body:   map[string]any{"action": "release_staged_bin"},
			substr: "bin_id is required",
		},
		{
			name:   "cancel_stuck_order-missing-order",
			body:   map[string]any{"action": "cancel_stuck_order"},
			substr: "order_id is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, h.apiRepairAnomaly, "/api/recovery/repair", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertJSONError(t, rec.Body.Bytes(), tc.substr)
		})
	}
}

func TestApiRepairAnomaly_ReleaseStagedBin_NotStaged(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-NOSTAGE-1")

	// Bin is "available", not "staged" — ReleaseStagedBin returns an error
	// that the handler forwards as 400.
	rec := postJSON(t, h.apiRepairAnomaly, "/api/recovery/repair",
		map[string]any{"action": "release_staged_bin", "bin_id": bin.ID})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

