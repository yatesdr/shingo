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
// on the stubEngine records the envelope and returns nil.
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
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)
	resp := doRequest(t, router, "POST", "/api/replay-outbox", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiReplayOutbox_InvalidID(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)
	resp := doRequest(t, router, "POST", "/api/replay-outbox?id=notanumber", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// apiSendManualMessage — switch-by-type. stubEngine.SendEnvelope records
// what it was handed and sends nothing, so each branch is covered end-to-end
// through protocol envelope construction.
// ═══════════════════════════════════════════════════════════════════════

// sendManualPayload wraps the top-level {type, payload} shape and POSTs it.
func sendManualPayload(t *testing.T, router *chi.Mux, msgType string, payload any) *http.Response {
	t.Helper()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	body := map[string]any{
		"type":    msgType,
		"payload": json.RawMessage(payloadBytes),
	}
	return doRequest(t, router, "POST", "/api/manual-message", body, nil)
}

func TestApiSendManualMessage_EdgeRegister_Success(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "edge.register", map[string]any{
		"version":  "test",
		"line_ids": []string{"line-1"},
	})
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

// sentDataBody decodes the body of the data envelope the handler last handed
// SendEnvelope into body, checking its subject.
func sentDataBody(t *testing.T, h *Handlers, subject string, body any) {
	t.Helper()
	env := h.orchestration.(*stubEngine).lastSendEnvelope
	if env == nil {
		t.Fatal("nothing was handed to SendEnvelope")
	}
	var d protocol.Data
	if err := json.Unmarshal(env.Payload, &d); err != nil {
		t.Fatalf("decode the data envelope: %v", err)
	}
	if d.Subject != subject {
		t.Fatalf("sent subject %q, want %q", d.Subject, subject)
	}
	if err := json.Unmarshal(d.Body, body); err != nil {
		t.Fatalf("decode the %s body: %v", subject, err)
	}
}

// The page's register carries the Edge's schema version, the one its
// heartbeater's register sends (engine.Engine.SchemaVersion): on a migrated
// database at or above protocol.EdgeSchemaBinLanded. A register without it
// was stored at Core as v0, and Core sent the station a Bound in place of
// each landing until its next real register (end to end:
// TestScenario_TheDebugPagesRegisterKeepsTheStationsLandings).
func TestApiSendManualMessage_EdgeRegister_CarriesTheEdgesSchemaVersion(t *testing.T) {
	t.Parallel()
	h, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "edge.register", map[string]any{"version": "dev"})
	assertStatus(t, resp, http.StatusOK)
	var reg protocol.EdgeRegister
	sentDataBody(t, h, protocol.SubjectEdgeRegister, &reg)
	want := dbOf(h).AppliedSchemaVersion()
	if want < protocol.EdgeSchemaBinLanded {
		t.Fatalf("the test database stands at v%d, below protocol.EdgeSchemaBinLanded (%d): the check would pass on a demoting value",
			want, protocol.EdgeSchemaBinLanded)
	}
	if reg.SchemaVersion != want {
		t.Errorf("the page's register carries schema_version %d, want the Edge's %d (engine SchemaVersion): Core keeps "+
			"it and sends a station below %d a Bound in place of a landing", reg.SchemaVersion, want, protocol.EdgeSchemaBinLanded)
	}
	if reg.Version != "dev" || reg.Instance != "" {
		t.Errorf("the page's register carries version %q and instance %q, want the typed dev and none", reg.Version, reg.Instance)
	}
}

// The page's heartbeat is the heartbeater's own body (engine.Engine.HeartbeatBody),
// feeds, owned map and zone as it builds them: Core's ack to it reaches this
// Edge as the answer to its own digests. The body the page used to build named
// the station alone (end to end:
// TestScenario_TheDebugPagesHeartbeatLeavesTheStationAsItWas).
func TestApiSendManualMessage_EdgeHeartbeat_IsTheHeartbeatersOwn(t *testing.T) {
	t.Parallel()
	h, router := newDiagnosticsManualRouter(t)
	own := &protocol.EdgeHeartbeat{
		StationID: "test.station", Timezone: "America/Chicago",
		Feeds: map[string]string{protocol.FeedBins: "bins-digest"},
		Owned: map[string]string{protocol.OwnedClaims: "claims-digest"},
	}
	h.orchestration.(*stubEngine).heartbeat = own

	resp := sendManualPayload(t, router, "edge.heartbeat", map[string]any{})
	assertStatus(t, resp, http.StatusOK)
	var got protocol.EdgeHeartbeat
	sentDataBody(t, h, protocol.SubjectEdgeHeartbeat, &got)
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode the sent heartbeat: %v", err)
	}
	wantJSON, err := json.Marshal(own)
	if err != nil {
		t.Fatalf("encode the heartbeater's: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("the page's heartbeat is %s, want the heartbeater's %s", gotJSON, wantJSON)
	}
}

func TestApiSendManualMessage_EdgeHeartbeat_Success(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	// The heartbeat carries no hand-set field since uptime left the wire.
	resp := sendManualPayload(t, router, "edge.heartbeat", map[string]any{})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_NodeListRequest_Success(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	// node.list_request ignores the payload entirely.
	resp := sendManualPayload(t, router, "node.list_request", map[string]any{})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderRequest_Success(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.cancel", protocol.OrderCancel{
		OrderUUID: "man-msg-2",
		Reason:    "test",
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderReceipt_Success(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.receipt", protocol.OrderReceipt{
		OrderUUID:   "man-msg-3",
		ReceiptType: "confirmed",
		FinalCount:  5,
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_OrderRedirect_Success(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	resp := sendManualPayload(t, router, "order.redirect", protocol.OrderRedirect{
		OrderUUID:       "man-msg-4",
		NewDeliveryNode: "NEW",
	})
	assertStatus(t, resp, http.StatusOK)
}

func TestApiSendManualMessage_UnknownType(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)
	resp := sendManualPayload(t, router, "does.not.exist", map[string]any{})
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "unknown message type: does.not.exist")
}

func TestApiSendManualMessage_InvalidOuterJSON(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	// type must be a string; sending an int breaks decoding the outer struct.
	body := map[string]any{"type": 42}
	resp := doRequest(t, router, "POST", "/api/manual-message", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiSendManualMessage_InvalidInnerPayload(t *testing.T) {
	t.Parallel()
	_, router := newDiagnosticsManualRouter(t)

	// edge.register requires version to unmarshal as a string. Sending a
	// number should trigger the inner "invalid payload" branch. (This used
	// edge.heartbeat's uptime, which left the wire.)
	body := map[string]any{
		"type":    "edge.register",
		"payload": json.RawMessage([]byte(`{"version":5}`)),
	}
	resp := doRequest(t, router, "POST", "/api/manual-message", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}
