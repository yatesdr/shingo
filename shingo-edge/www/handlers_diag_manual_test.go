package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingo/protocol"

	"github.com/go-chi/chi/v5"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — covers handlers_diagnostics.go and
// handlers_manual_message.go.
//
// Diagnostics: handleDiagnostics, apiReplayOutbox,
// apiRequestOrderStatusSync all touch engine subsystems that the
// stubEngine returns nil for (Reconciliation(), CoreSync()). Only the
// early input-validation branches of apiReplayOutbox are testable
// without expanding the stub surface.
//
// Manual message: handleManualMessage renders a template; the API
// endpoint apiSendManualMessage is fully testable because SendEnvelope
// on the stubEngine is a no-op returning nil.
// ═══════════════════════════════════════════════════════════════════════

func newDiagnosticsManualRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	r.Route("/api", func(r chi.Router) {
		r.Post("/manual-message", h.apiSendManualMessage)
		r.Post("/replay-outbox", h.apiReplayOutbox)
	})
	return h, r
}

// ═══════════════════════════════════════════════════════════════════════
// apiReplayOutbox — only the input-validation branches are covered;
// the RequeueOutbox path requires a real *engine.ReconciliationService.
// ═══════════════════════════════════════════════════════════════════════

func TestApiReplayOutbox_MissingID(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)
	resp := doRequest(t, router, "POST", "/api/replay-outbox", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiReplayOutbox_InvalidID(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)
	resp := doRequest(t, router, "POST", "/api/replay-outbox?id=notanumber", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// apiSendManualMessage — switch-by-type. stubEngine.SendEnvelope is a
// no-op, so each branch is covered end-to-end through protocol envelope
// construction.
// ═══════════════════════════════════════════════════════════════════════

// sendManualPayload wraps the top-level {type, payload} shape and POSTs it.
func sendManualPayload(t *testing.T, router *chi.Mux, msgType string, payload interface{}) *http.Response {
	t.Helper()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	body := map[string]interface{}{
		"type":    msgType,
		"payload": json.RawMessage(payloadBytes),
	}
	return doRequest(t, router, "POST", "/api/manual-message", body, nil)
}

func TestApiSendManualMessage_EdgeRegister_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "edge.register", map[string]interface{}{
		"version":  "test",
		"line_ids": []string{"line-1"},
	})
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiSendManualMessage_EdgeHeartbeat_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "edge.heartbeat", map[string]interface{}{
		"uptime": 60,
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_ProductionReport_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "production.report", map[string]interface{}{
		"entries": []protocol.ProductionReportEntry{},
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_NodeListRequest_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	// node.list_request ignores the payload entirely.
	resp := sendManualPayload(t, router, "node.list_request", map[string]interface{}{})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderRequest_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.request", protocol.OrderRequest{
		OrderUUID:    "man-msg-1",
		OrderType:    "retrieve",
		Quantity:     1,
		DeliveryNode: "NODE-A",
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderCancel_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.cancel", protocol.OrderCancel{
		OrderUUID: "man-msg-2",
		Reason:    "test",
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderReceipt_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.receipt", protocol.OrderReceipt{
		OrderUUID:   "man-msg-3",
		ReceiptType: "confirmed",
		FinalCount:  5,
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderRedirect_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.redirect", protocol.OrderRedirect{
		OrderUUID:       "man-msg-4",
		NewDeliveryNode: "NEW",
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderStorageWaybill_Success(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.storage_waybill", protocol.OrderStorageWaybill{
		OrderUUID:   "man-msg-5",
		OrderType:   "store",
		SourceNode:  "CELL",
		FinalCount:  3,
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_UnknownType(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)
	resp := sendManualPayload(t, router, "does.not.exist", map[string]interface{}{})
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "unknown message type: does.not.exist")
}

func TestApiSendManualMessage_InvalidOuterJSON(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	// type must be a string; sending an int breaks decoding the outer struct.
	body := map[string]interface{}{"type": 42}
	resp := doRequest(t, router, "POST", "/api/manual-message", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiSendManualMessage_InvalidInnerPayload(t *testing.T) {
	_, router := newDiagnosticsManualRouter(t)

	// production.report requires entries to unmarshal as []ProductionReportEntry.
	// Sending a string should trigger the inner "invalid payload" branch.
	body := map[string]interface{}{
		"type":    "production.report",
		"payload": json.RawMessage([]byte(`{"entries":"not-an-array"}`)),
	}
	resp := doRequest(t, router, "POST", "/api/manual-message", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}
