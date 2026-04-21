//go:build docker

package www

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// Characterization tests for handlers_telemetry.go — node/bin telemetry
// endpoints, payload manifest template lookups, and the e-maint robot
// telemetry report.

// chiReq constructs an HTTP GET with a chi URL-param context.
func chiReq(method, target string, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// --- apiTelemetryNodeBins ---------------------------------------------------

func TestApiTelemetryNodeBins_EmptyParam(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiTelemetryNodeBins, "/api/telemetry/node-bins")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Empty nodes param → empty JSON array.
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("body: got %q, want '[]'", body)
	}
}

func TestApiTelemetryNodeBins_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TELEM-1")

	rec := getPlain(t, h.apiTelemetryNodeBins,
		"/api/telemetry/node-bins?nodes="+sd.StorageNode.Name+","+sd.LineNode.Name)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var entries []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: got %d, want 2: %+v", len(entries), entries)
	}
	// First entry should be for the storage node and be occupied.
	if entries[0]["node_name"] != sd.StorageNode.Name {
		t.Errorf("entries[0] node_name: got %v", entries[0]["node_name"])
	}
	if entries[0]["occupied"] != true {
		t.Errorf("storage node should be occupied: %+v", entries[0])
	}
	if entries[0]["bin_label"] != bin.Label {
		t.Errorf("bin_label: got %v, want %q", entries[0]["bin_label"], bin.Label)
	}
	// Line node should NOT be occupied.
	if entries[1]["occupied"] != false {
		t.Errorf("line node should not be occupied: %+v", entries[1])
	}
}

// --- apiTelemetryPayloadManifest --------------------------------------------

func TestApiTelemetryPayloadManifest_EmptyCode(t *testing.T) {
	h, _ := testHandlers(t)

	// No chi param → code == "".
	req := chiReq(http.MethodGet, "/api/telemetry/payload//manifest", map[string]string{"code": ""})
	rec := httptest.NewRecorder()
	h.apiTelemetryPayloadManifest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["uop_capacity"].(float64) != 0 {
		t.Errorf("uop_capacity: got %v, want 0", resp["uop_capacity"])
	}
}

func TestApiTelemetryPayloadManifest_UnknownCode(t *testing.T) {
	h, _ := testHandlers(t)

	req := chiReq(http.MethodGet, "/api/telemetry/payload/NO-SUCH/manifest",
		map[string]string{"code": "NO-SUCH"})
	rec := httptest.NewRecorder()
	h.apiTelemetryPayloadManifest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["uop_capacity"].(float64) != 0 {
		t.Errorf("uop_capacity: got %v, want 0", resp["uop_capacity"])
	}
}

func TestApiTelemetryPayloadManifest_KnownWithManifest(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	// Give the payload a capacity and a manifest template.
	sd.Payload.UOPCapacity = 50
	if err := db.UpdatePayload(sd.Payload); err != nil {
		t.Fatalf("update payload uop: %v", err)
	}
	if err := db.CreatePayloadManifestItem(&store.PayloadManifestItem{
		PayloadID: sd.Payload.ID, PartNumber: "P-X", Quantity: 10, Description: "desc",
	}); err != nil {
		t.Fatalf("seed manifest item: %v", err)
	}

	req := chiReq(http.MethodGet, "/api/telemetry/payload/"+sd.Payload.Code+"/manifest",
		map[string]string{"code": sd.Payload.Code})
	rec := httptest.NewRecorder()
	h.apiTelemetryPayloadManifest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		UOPCapacity int `json:"uop_capacity"`
		Items       []struct {
			PartNumber  string `json:"part_number"`
			Quantity    int64  `json:"quantity"`
			Description string `json:"description"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.UOPCapacity != 50 {
		t.Errorf("uop_capacity: got %d, want 50", resp.UOPCapacity)
	}
	if len(resp.Items) != 1 || resp.Items[0].PartNumber != "P-X" {
		t.Errorf("items: got %+v", resp.Items)
	}
}

func TestApiTelemetryPayloadManifest_KnownNoManifest_FallbackEntry(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	sd.Payload.UOPCapacity = 30
	if err := db.UpdatePayload(sd.Payload); err != nil {
		t.Fatalf("update payload uop: %v", err)
	}

	req := chiReq(http.MethodGet, "/api/telemetry/payload/"+sd.Payload.Code+"/manifest",
		map[string]string{"code": sd.Payload.Code})
	rec := httptest.NewRecorder()
	h.apiTelemetryPayloadManifest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		UOPCapacity int `json:"uop_capacity"`
		Items       []struct {
			PartNumber string `json:"part_number"`
			Quantity   int64  `json:"quantity"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.UOPCapacity != 30 {
		t.Errorf("uop_capacity: got %d, want 30", resp.UOPCapacity)
	}
	// Fallback entry: one item with part_number = code, quantity = uop_capacity.
	if len(resp.Items) != 1 || resp.Items[0].PartNumber != sd.Payload.Code || resp.Items[0].Quantity != 30 {
		t.Errorf("fallback item: got %+v", resp.Items)
	}
}

// --- apiTelemetryNodeChildren -----------------------------------------------

func TestApiTelemetryNodeChildren_EmptyName(t *testing.T) {
	h, _ := testHandlers(t)
	req := chiReq(http.MethodGet, "/api/telemetry/node//children", map[string]string{"name": ""})
	rec := httptest.NewRecorder()
	h.apiTelemetryNodeChildren(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("body: got %q, want '[]'", body)
	}
}

func TestApiTelemetryNodeChildren_UnknownName(t *testing.T) {
	h, _ := testHandlers(t)
	req := chiReq(http.MethodGet, "/api/telemetry/node/NO-SUCH/children",
		map[string]string{"name": "NO-SUCH"})
	rec := httptest.NewRecorder()
	h.apiTelemetryNodeChildren(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("body: got %q, want '[]'", body)
	}
}

// --- apiBinLoad -------------------------------------------------------------

func TestApiBinLoad_MissingNodeName(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{"node_name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "node_name is required")
}

func TestApiBinLoad_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiBinLoad, "/api/telemetry/bin-load", []byte("not-json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiBinLoad_UnknownNode(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{"node_name": "NO-SUCH-NODE", "payload_code": "X"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiBinLoad_NodeWithoutBin(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	// Line node has no bin in the standard fixture.
	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{"node_name": sd.LineNode.Name, "payload_code": "PART-A"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "no bin at node")
}

func TestApiBinLoad_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LOAD-1")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{
			"node_name":    sd.StorageNode.Name,
			"payload_code": sd.Payload.Code,
			"uop_count":    25,
			"manifest": []map[string]any{
				{"part_number": "P1", "quantity": 10},
				{"part_number": "P2", "quantity": 15},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string `json:"status"`
		BinID        int64  `json:"bin_id"`
		BinLabel     string `json:"bin_label"`
		PayloadCode  string `json:"payload_code"`
		UOPRemaining int    `json:"uop_remaining"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" || resp.BinID != bin.ID || resp.UOPRemaining != 25 {
		t.Errorf("response: got %+v", resp)
	}

	// Verify manifest was persisted and confirmed.
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != sd.Payload.Code {
		t.Errorf("payload_code on bin: got %q", got.PayloadCode)
	}
	if got.UOPRemaining != 25 {
		t.Errorf("uop_remaining on bin: got %d, want 25", got.UOPRemaining)
	}
	if !got.ManifestConfirmed {
		t.Error("manifest should be confirmed after bin-load")
	}
}

// --- apiBinClear ------------------------------------------------------------

func TestApiBinClear_MissingNodeName(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "node_name is required")
}

func TestApiBinClear_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiBinClear, "/api/telemetry/bin-clear", []byte("{"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiBinClear_UnknownNode(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": "NOPE"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiBinClear_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CLEAR-1")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.StorageNode.Name})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status   string `json:"status"`
		BinID    int64  `json:"bin_id"`
		BinLabel string `json:"bin_label"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" || resp.BinID != bin.ID || resp.BinLabel != bin.Label {
		t.Errorf("response: got %+v", resp)
	}

	// The manifest should be cleared on the bin.
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("payload_code should be cleared: got %q", got.PayloadCode)
	}
}

// --- apiEMaintRobotTelemetry (+ download) -----------------------------------

func TestApiEMaintRobotTelemetry_EmptyFleet(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiEMaintRobotTelemetry, "/api/telemetry/e-maint")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ReportID    string           `json:"report_id"`
		GeneratedAt string           `json:"generated_at"`
		Source      string           `json:"source"`
		RobotCount  int              `json:"robot_count"`
		Robots      []map[string]any `json:"robots"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Source != "shingo-core" {
		t.Errorf("source: got %q, want shingo-core", resp.Source)
	}
	if resp.ReportID == "" {
		t.Error("report_id should be a UUID")
	}
	if resp.RobotCount != 0 || len(resp.Robots) != 0 {
		t.Errorf("expected empty robot list, got %d: %+v", resp.RobotCount, resp.Robots)
	}
}

func TestApiEMaintRobotTelemetryDownload_ContentDisposition(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiEMaintRobotTelemetryDownload, "/api/telemetry/e-maint/download")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "robot-telemetry-") {
		t.Errorf("Content-Disposition: got %q", cd)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	// Body should still be valid JSON matching the report shape.
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["source"] != "shingo-core" {
		t.Errorf("source: got %v", resp["source"])
	}
}
