//go:build docker

package www

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
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
	t.Parallel()
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
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TELEM-1")

	rec := getPlain(t, h.apiTelemetryNodeBins,
		"/api/telemetry/node-bins?nodes="+sd.StorageNode.Name+","+sd.LineNode.Name)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var entries []map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&entries), "decode")
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
	t.Parallel()
	h, _ := testHandlers(t)

	// No chi param → code == "".
	req := chiReq(http.MethodGet, "/api/telemetry/payload//manifest", map[string]string{"code": ""})
	rec := httptest.NewRecorder()
	h.apiTelemetryPayloadManifest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp["uop_capacity"].(float64) != 0 {
		t.Errorf("uop_capacity: got %v, want 0", resp["uop_capacity"])
	}
}

func TestApiTelemetryPayloadManifest_UnknownCode(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	req := chiReq(http.MethodGet, "/api/telemetry/payload/NO-SUCH/manifest",
		map[string]string{"code": "NO-SUCH"})
	rec := httptest.NewRecorder()
	h.apiTelemetryPayloadManifest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp["uop_capacity"].(float64) != 0 {
		t.Errorf("uop_capacity: got %v, want 0", resp["uop_capacity"])
	}
}

func TestApiTelemetryPayloadManifest_KnownWithManifest(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	// Give the payload a capacity and a manifest template.
	sd.Payload.UOPCapacity = 50
	testutil.MustNoErr(t, db.UpdatePayload(sd.Payload), "update payload uop")
	if err := db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: sd.Payload.ID, PartNumber: "P-X", PartsPerCycle: 10, Description: "desc",
	}, ""); err != nil {
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
			PartNumber    string `json:"part_number"`
			PartsPerCycle int64  `json:"parts_per_cycle"`
			Description   string `json:"description"`
		} `json:"items"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.UOPCapacity != 50 {
		t.Errorf("uop_capacity: got %d, want 50", resp.UOPCapacity)
	}
	if len(resp.Items) != 1 || resp.Items[0].PartNumber != "P-X" {
		t.Fatalf("items: got %+v", resp.Items)
	}
	// The ratio is the whole point of the endpoint and it is what Edge
	// multiplies by. A drifted JSON key decodes to zero in silence.
	if resp.Items[0].PartsPerCycle != 10 {
		t.Errorf("parts_per_cycle: got %d, want 10", resp.Items[0].PartsPerCycle)
	}
}

func TestApiTelemetryPayloadManifest_KnownNoManifest_FallbackEntry(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	sd.Payload.UOPCapacity = 30
	testutil.MustNoErr(t, db.UpdatePayload(sd.Payload), "update payload uop")

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
			PartNumber    string `json:"part_number"`
			PartsPerCycle int64  `json:"parts_per_cycle"`
		} `json:"items"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.UOPCapacity != 30 {
		t.Errorf("uop_capacity: got %d, want 30", resp.UOPCapacity)
	}
	// Fallback entry: one item with part_number = code and a ratio of 1 —
	// "a bin of this payload holds uop_remaining of it". Before the
	// parts_per_cycle rename this said quantity = uop_capacity, which is the
	// same claim expressed as a full-bin count; 1 x capacity is that count, so
	// the fallback still describes the same bin.
	if len(resp.Items) != 1 || resp.Items[0].PartNumber != sd.Payload.Code || resp.Items[0].PartsPerCycle != 1 {
		t.Errorf("fallback item: got %+v, want one line of %s at 1 per cycle", resp.Items, sd.Payload.Code)
	}
}

// --- apiBinLoad -------------------------------------------------------------

func TestApiBinLoad_MissingNodeName(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{"node_name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "node_name is required")
}

func TestApiBinLoad_InvalidJSON(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiBinLoad, "/api/telemetry/bin-load", []byte("not-json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiBinLoad_UnknownNode(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{"node_name": "NO-SUCH-NODE", "payload_code": "X"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiBinLoad_NodeWithoutBin(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
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

// TestApiBinLoad_UOPFallsBackToCapacity pins the fallback's UNIT. With no
// declared count the bin is assumed full, and a full bin is uop_capacity
// CYCLES — not the sum of the manifest's part counts, which is a different
// unit and agrees only while every payload is one part per cycle.
//
// The fixture makes the two disagree on purpose: capacity 50, manifest lines
// summing 300. Reading 300 would be the old behaviour passing.
func TestApiBinLoad_UOPFallsBackToCapacity(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	sd.Payload.UOPCapacity = 50
	testutil.MustNoErr(t, db.UpdatePayload(sd.Payload), "set capacity")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LOAD-FALLBACK")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{
			"node_name":    sd.StorageNode.Name,
			"payload_code": sd.Payload.Code,
			// uop_count omitted — the operator declared none.
			"manifest": []map[string]any{
				{"part_number": "P1", "quantity": 100},
				{"part_number": "P2", "quantity": 200},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 50 {
		t.Errorf("uop_remaining = %d, want 50 (the payload's capacity in cycles, not the manifest's 300 parts)",
			got.UOPRemaining)
	}
}

// TestApiBinLoad_UOPFallbackWithNoTemplateIsRefused: an undeclared count on a
// payload Core has no template for is an unanswered question, not a full bin —
// and not an empty one either. This used to answer 200 and write uop 0, which
// is a ledger entry indistinguishable from "somebody counted none": the guess
// that looks like a measurement. Refusing is what actually makes the operator
// answer it, and it is how BinService.LoadPayload has always treated an
// unknown code. The bin is left as it was.
func TestApiBinLoad_UOPFallbackWithNoTemplateIsRefused(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LOAD-NOTMPL")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{
			"node_name":    sd.StorageNode.Name,
			"payload_code": "PAYLOAD-CORE-HAS-NEVER-SEEN",
			"manifest":     []map[string]any{{"part_number": "P1", "quantity": 100}},
		})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "not found")

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != bin.PayloadCode || got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("refused load wrote the bin: payload %q uop %d, was %q uop %d",
			got.PayloadCode, got.UOPRemaining, bin.PayloadCode, bin.UOPRemaining)
	}
}

// TestApiBinLoad_DeclaredZeroIsHonoured is the third wire state, and the one
// the old `uop <= 0` arm could not express: somebody counted the bin and it
// held nothing. Absence asks for the standard pack; zero is an answer, and
// spending it on a full carrier hands the line parts nobody put there.
func TestApiBinLoad_DeclaredZeroIsHonoured(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	sd.Payload.UOPCapacity = 50
	testutil.MustNoErr(t, db.UpdatePayload(sd.Payload), "set capacity")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LOAD-ZERO")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load",
		map[string]any{
			"node_name":    sd.StorageNode.Name,
			"payload_code": sd.Payload.Code,
			"uop_count":    0, // declared, and declared empty
			"manifest":     []map[string]any{{"part_number": "P1", "quantity": 100}},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 0 {
		t.Errorf("uop_remaining = %d, want 0 — a declared zero is a count, not a missing one; "+
			"capacity %d means the standard-pack arm ate it", got.UOPRemaining, sd.Payload.UOPCapacity)
	}
}

// --- apiBinClear ------------------------------------------------------------

func TestApiBinClear_MissingNodeName(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "node_name is required")
}

func TestApiBinClear_InvalidJSON(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiBinClear, "/api/telemetry/bin-clear", []byte("{"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiBinClear_UnknownNode(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": "NOPE"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiBinClear_HappyPath(t *testing.T) {
	t.Parallel()
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
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Status != "ok" || resp.BinID != bin.ID || resp.BinLabel != bin.Label {
		t.Errorf("response: got %+v", resp)
	}

	// The manifest should be cleared on the bin.
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("payload_code should be cleared: got %q", got.PayloadCode)
	}
}

// TestApiBinClear_AmbiguousMultipleBins_Returns409 verifies that when a node
// holds more than one bin, a node_name-only clear is ambiguous and must be
// refused (409) rather than silently clearing an arbitrary bins[0].
func TestApiBinClear_AmbiguousMultipleBins_Returns409(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-AMB-1")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-AMB-2")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.StorageNode.Name})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "specify bin_id")
}

// TestApiBinClear_AmbiguousWithBinID_ClearsNamedBin pins the disambiguation
// path: with multiple bins at a node, an explicit bin_id clears exactly that
// bin and leaves the others untouched.
func TestApiBinClear_AmbiguousWithBinID_ClearsNamedBin(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	keep := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-KEEP")
	target := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-TARGET")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.StorageNode.Name, "bin_id": target.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		BinID int64 `json:"bin_id"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.BinID != target.ID {
		t.Errorf("cleared bin_id: got %d, want %d (the named bin)", resp.BinID, target.ID)
	}
	if got, _ := db.GetBin(target.ID); got.PayloadCode != "" {
		t.Errorf("target payload_code should be cleared, got %q", got.PayloadCode)
	}
	if got, _ := db.GetBin(keep.ID); got.PayloadCode == "" {
		t.Error("the other bin must be untouched, but its payload_code was cleared")
	}
}

// TestApiBinClear_UnknownBinTypeCode_Returns400 sends a bin_type_code that
// does not exist in the database; the handler must reject it with 400.
func TestApiBinClear_UnknownBinTypeCode_Returns400(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-UNK-CODE")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.StorageNode.Name, "bin_type_code": "DOES-NOT-EXIST"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "unknown bin type code")
}

// TestApiBinClear_BinTypeCodeKnownNotInNodeSet_Returns200 verifies that a
// bin_type_code that exists globally but is not in the node's node_bin_types
// set is now accepted (200). The node-membership check was dropped in the
// dunnage redesign: the picker derives options from payload→dunnage mappings,
// not node_bin_types, so node-membership is no longer the gate. Blast radius
// of a mis-pick is "starves out, repair via re-type" (R3), not data corruption.
func TestApiBinClear_BinTypeCodeKnownNotInNodeSet_Returns200(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RELAXED")

	// Register the default bin type on the node, then use a second type
	// that the node_bin_types table does NOT list — must now succeed.
	testutil.MustNoErr(t, db.SetNodeBinTypes(sd.StorageNode.ID, []int64{sd.BinType.ID}), "set node bin types")
	other := &bins.BinType{Code: "45x48-OTHER", Description: "not in node set"}
	if err := db.CreateBinType(other); err != nil {
		t.Fatalf("create other bin type: %v", err)
	}

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.StorageNode.Name, "bin_type_code": "45x48-OTHER"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiBinClear_NoBinTypeCode_LeavesTypeUnchanged verifies that omitting
// bin_type_code is a no-op for the bin's type: the clear succeeds (200) but
// the bin_type_id on the bin remains whatever it was before.
func TestApiBinClear_NoBinTypeCode_LeavesTypeUnchanged(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-NOTYPE")
	origTypeID := bin.BinTypeID

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.StorageNode.Name})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	after, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin post-clear: %v", err)
	}
	if after.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (manifest must clear)", after.PayloadCode)
	}
	if after.BinTypeID != origTypeID {
		t.Errorf("BinTypeID = %d, want %d (nil path must not change type)", after.BinTypeID, origTypeID)
	}
}

// TestApiBinClear_BinIDNotAtNode_Returns409 pins the guard that a supplied
// bin_id must actually be at the named node.
func TestApiBinClear_BinIDNotAtNode_Returns409(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-ONLY")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.StorageNode.Name, "bin_id": 999999})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "not at node")
}

// --- apiEMaintRobotTelemetry (+ download) -----------------------------------

func TestApiEMaintRobotTelemetry_EmptyFleet(t *testing.T) {
	t.Parallel()
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
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
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
	t.Parallel()
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
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode body")
	if resp["source"] != "shingo-core" {
		t.Errorf("source: got %v", resp["source"])
	}
}

// --- door pins: the shared instruments ---------------------------------------
//
// The write doors an Edge calls (bin-load, bin-count, bin-clear, clear-home,
// bin-quality-hold) and the read doors it asks (held-partials, release points,
// preflight) are pinned arm by arm: the status, the body byte for byte, and
// what the call wrote. The door bodies are to be lifted out of their handlers
// wire-free, with each handler kept as an adapter over its body; these pins are
// what that move must reproduce.

// doorErr is the body jsonError writes for msg, byte for byte.
func doorErr(msg string) string {
	var b strings.Builder
	_ = json.NewEncoder(&b).Encode(map[string]string{"error": msg})
	return b.String()
}

// doorRows is Core's current bins rows for node, read by the snapshot's own
// row code (what a write door's refusal carries), checked against the bins
// table so the pin does not lean on that code alone: the row names the node,
// and names the newest bin standing there (none on an empty or unknown node).
func doorRows(t *testing.T, f *doorFix, node string) []protocol.NodeBinsRow {
	t.Helper()
	rows, err := messaging.NewCoreDataService(f.db, nil, service.EpochAnnounce{}).NodeBinsRows([]string{node})
	testutil.MustNoErr(t, err, "the rows of "+node)
	var newest sql.NullInt64
	testutil.MustNoErr(t, f.db.QueryRow(`SELECT max(b.id) FROM bins b JOIN nodes n ON n.id = b.node_id WHERE n.name = $1`, node).
		Scan(&newest), "the newest bin at "+node)
	if len(rows) == 0 || rows[0].Node != node || rows[0].BinID != newest.Int64 {
		t.Errorf("rows of %s = %+v, want its own row first, naming bin %d", node, rows, newest.Int64)
	}
	return rows
}

// refusalAt is the body a write door writes refusing a request that named
// node: the sentence and Core's current rows for the node (www.refuseAt).
func refusalAt(t *testing.T, f *doorFix, node string, code int, msg string) (int, string) {
	t.Helper()
	return code, doorJSON(t, doorRefusal{Error: msg, Rows: doorRows(t, f, node)})
}

// refusedAt is refusalAt as an arm's answer, the node named by f.
func refusedAt(node func(*doorFix) string, code int, msg string) func(*testing.T, *doorFix) (int, string) {
	return func(t *testing.T, f *doorFix) (int, string) { return refusalAt(t, f, node(f), code, msg) }
}

// atNode names a node an arm refers to by its literal name.
func atNode(name string) func(*doorFix) string { return func(*doorFix) string { return name } }

// doorJSON is the body a json.Encoder writes for v, byte for byte: what jsonOK
// and the clear-home adapter write.
func doorJSON(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	testutil.MustNoErr(t, json.NewEncoder(&b).Encode(v), "encode the expected body")
	return b.String()
}

// assertDoor checks an answer's status and its body, byte for byte.
func assertDoor(t *testing.T, rec *httptest.ResponseRecorder, code int, body string) {
	t.Helper()
	if rec.Code != code || rec.Body.String() != body {
		t.Errorf("answer:\n got %d %s\nwant %d %s", rec.Code, rec.Body.String(), code, body)
	}
}

// doorSnap is what a door can write, read before the call: each named bin's
// whole row, and the row counts of the tables a door appends to.
type doorSnap struct {
	bins   map[int64]map[string]any
	counts map[string]int
}

// doorTables are the tables a door appends to: orders it authors, the bin
// ledger, the audit log, and the outbox its broadcasts ride.
var doorTables = []string{"orders", "bin_uop_ledger", "audit_log", "outbox"}

func snapDoor(t *testing.T, db *store.DB, binIDs ...int64) doorSnap {
	t.Helper()
	s := doorSnap{bins: map[int64]map[string]any{}, counts: map[string]int{}}
	for _, id := range binIDs {
		s.bins[id] = wholeBinRow(t, db, id)
	}
	for _, tbl := range doorTables {
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM `+tbl).Scan(&n), "count "+tbl)
		s.counts[tbl] = n
	}
	return s
}

// writes is what changed since the snap: per bin, the columns whose values
// moved (sorted), and per table, the rows added. Empty maps when nothing was
// written.
func (s doorSnap) writes(t *testing.T, db *store.DB) (map[int64][]string, map[string]int) {
	t.Helper()
	after := snapDoor(t, db, keysOf(s.bins)...)
	cols := map[int64][]string{}
	for id, before := range s.bins {
		if c := changedCols(before, after.bins[id]); len(c) > 0 {
			cols[id] = c
		}
	}
	added := map[string]int{}
	for tbl, n := range s.counts {
		if d := after.counts[tbl] - n; d != 0 {
			added[tbl] = d
		}
	}
	return cols, added
}

// assertNothingWritten checks a refusal wrote nothing: no column of any named
// bin moved and no table gained a row.
func (s doorSnap) assertNothingWritten(t *testing.T, db *store.DB) {
	t.Helper()
	cols, added := s.writes(t, db)
	if len(cols) != 0 || len(added) != 0 {
		t.Errorf("a refusal wrote: bin columns %v, rows added %v", cols, added)
	}
}

// assertWrote checks an accept wrote exactly these columns of these bins and
// added exactly these rows.
func (s doorSnap) assertWrote(t *testing.T, db *store.DB, wantCols map[int64][]string, wantAdded map[string]int) {
	t.Helper()
	cols, added := s.writes(t, db)
	if !reflect.DeepEqual(cols, wantCols) || !reflect.DeepEqual(added, wantAdded) {
		t.Errorf("writes:\n got bin columns %v, rows added %v\nwant bin columns %v, rows added %v",
			cols, added, wantCols, wantAdded)
	}
}

func keysOf(m map[int64]map[string]any) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBinClearDoor_Arms pins every arm of POST /api/telemetry/bin-clear
// (apiBinClear over the engine's clear, ClearForReuseStampAndBookDeparture):
// the status, the body byte for byte, and what was written — nothing at all on
// a refusal. The stage-1 marker and the bare cart's carrier are
// two_stage_clear_docker_test.go's; the declared height is
// tote_declare_docker_test.go's.
func TestBinClearDoor_Arms(t *testing.T) {
	t.Parallel()
	storage := func(f *doorFix) string { return f.sd.StorageNode.Name }
	// onStorage is a confirmed carrier of PART-A at STORAGE-A1 holding 100.
	onStorage := func(t *testing.T, f *doorFix, label string) *bins.Bin {
		return testdb.CreateBinAtNode(t, f.db, f.sd.Payload.Code, f.sd.StorageNode.ID, label)
	}
	clearBody := func(f *doorFix, fields map[string]any) map[string]any {
		b := map[string]any{"node_name": f.sd.StorageNode.Name}
		for k, v := range fields {
			b[k] = v
		}
		return b
	}
	cleared := func(label, payload string, uop int, typeCode string) func(*testing.T, *doorFix) (int, string) {
		return func(t *testing.T, f *doorFix) (int, string) {
			b := binByLabel(t, f.db, label)
			return http.StatusOK, doorJSON(t, protocol.BinClearResponse{Status: "ok", BinID: b.ID, BinLabel: label,
				DeltaEpoch: b.DeltaEpoch, ClearedPayloadCode: payload, ClearedUOP: uop, ClearedBinTypeCode: typeCode})
		}
	}
	clearCols := []string{"delta_epoch", "loaded_at", "manifest", "manifest_confirmed", "payload_code", "uop_remaining", "updated_at"}
	wroteClear := func(label string, cols ...string) func(*doorFix) (map[int64][]string, map[string]int) {
		return func(f *doorFix) (map[int64][]string, map[string]int) {
			if cols == nil {
				cols = clearCols
			}
			return map[int64][]string{binIDOf(f, label): cols}, map[string]int{"bin_uop_ledger": 1}
		}
	}
	// heldPartial is a carrier of 7 of CL-PPH standing in a partial home of
	// its part (capacity 40).
	heldPartial := func(t *testing.T, f *doorFix, label string) *bins.Bin {
		ppHome(t, f.db, f.sd.StorageNode, "CL-PPH")
		b := testdb.CreateBinAtNode(t, f.db, "CL-PPH", f.sd.StorageNode.ID, label)
		testutil.MustNoErr(t, f.db.SetBinManifest(b.ID, `{"items":[]}`, "CL-PPH", 7), "a partial in its home")
		return b
	}
	runDoorArms(t, func(h *Handlers) http.HandlerFunc { return h.apiBinClear }, "/api/telemetry/bin-clear", []doorArm{
		{name: "an undecodable body is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return "{", nil },
			answer:  refusedWith(http.StatusBadRequest, "unexpected EOF")},
		{name: "no node_name is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "CL-NONAME")
				return clearBody(f, map[string]any{"node_name": ""}), []int64{b.ID}
			},
			answer: refusedWith(http.StatusBadRequest, "node_name is required")},
		{name: "a negative height is refused before the node is read",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "CL-NEG")
				return clearBody(f, map[string]any{"layers": -1}), []int64{b.ID}
			},
			answer: refusedWith(http.StatusBadRequest, "layers -1 is not a height")},
		{name: "an unknown node is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				return clearBody(f, map[string]any{"node_name": "CL-NO-SUCH-NODE"}), nil
			},
			answer: refusedAt(atNode("CL-NO-SUCH-NODE"), http.StatusNotFound, `node "CL-NO-SUCH-NODE" not found`)},
		{name: "a node holding no bin is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) { return clearBody(f, nil), nil },
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusBadRequest, "no bin at node "+storage(f))
			}},
		{name: "a bin_id not at the node is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "CL-FOREIGNID")
				return clearBody(f, map[string]any{"bin_id": 987654321}), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, "bin 987654321 is not at node "+storage(f))
			}},
		{name: "two bins and no bin_id is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				a, b := onStorage(t, f, "CL-AMB-1"), onStorage(t, f, "CL-AMB-2")
				return clearBody(f, nil), []int64{a.ID, b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, "node "+storage(f)+" holds 2 bins; specify bin_id to disambiguate")
			}},
		{name: "a held partial without UNLOAD is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := heldPartial(t, f, "CL-HELD")
				return clearBody(f, nil), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusConflict, "bin CL-HELD holds 7 of CL-PPH, unfinished and held here to be called "+
				"back to its line; UNLOAD to clear it anyway")},
		{name: "a stage 1 with nowhere to send the cart is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				_, _, w1, _ := twoStagePair(t, f.h, f.db, "CLN", []string{"CLN-S1-W1"}, nil)
				b := testdb.CreateBinAtNode(t, f.db, f.sd.Payload.Code, w1[0].ID, "CL-S1")
				return clearBody(f, map[string]any{"node_name": "CLN-S1-W1"}), []int64{b.ID}
			},
			answer: refusedAt(atNode("CLN-S1-W1"), http.StatusConflict, "this station has nowhere to send the cart: set where CLN-S1-W1's "+
				"carts go before clearing")},
		{name: "an unknown bin type code is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "CL-UNKTYPE")
				return clearBody(f, map[string]any{"bin_type_code": "CL-NOPE"}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusBadRequest, `unknown bin type code "CL-NOPE"`)},
		{name: "a write the database refuses is a server failure",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "CL-FAULT")
				failBinWrites(t, f.db, b.ID, "payload_code")
				return clearBody(f, nil), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusInternalServerError, fmt.Sprintf("clear manifest bin %d: ERROR: door fault (SQLSTATE P0001)",
					binIDOf(f, "CL-FAULT")))
			}},
		{name: "a clear empties the one bin",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "CL-OK")
				return clearBody(f, nil), []int64{b.ID}
			},
			answer: cleared("CL-OK", "PART-A", 100, "DEFAULT"),
			wrote:  wroteClear("CL-OK")},
		{name: "UNLOAD clears a held partial",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := heldPartial(t, f, "CL-HELD-UNLOAD")
				return clearBody(f, map[string]any{"unload": true}), []int64{b.ID}
			},
			answer: cleared("CL-HELD-UNLOAD", "CL-PPH", 7, "DEFAULT"),
			// The fixture's partial was declared by a manifest write, which leaves
			// it unconfirmed and stamps no held_since: neither column moves.
			wrote: wroteClear("CL-HELD-UNLOAD", "delta_epoch", "loaded_at", "manifest", "payload_code", "uop_remaining", "updated_at")},
		{name: "a bin type code re-types the carrier",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				testutil.MustNoErr(t, f.db.CreateBinType(&bins.BinType{Code: "CL-DUNNAGE"}), "a dunnage type")
				b := onStorage(t, f, "CL-RETYPE")
				return clearBody(f, map[string]any{"bin_type_code": "CL-DUNNAGE"}), []int64{b.ID}
			},
			answer: cleared("CL-RETYPE", "PART-A", 100, "CL-DUNNAGE"),
			wrote:  wroteClear("CL-RETYPE", append([]string{"bin_type_id"}, clearCols...)...)},
		{name: "a bin_id names one of two bins",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				a, b := onStorage(t, f, "CL-TWO-1"), onStorage(t, f, "CL-TWO-2")
				return clearBody(f, map[string]any{"bin_id": b.ID}), []int64{a.ID, b.ID}
			},
			answer: cleared("CL-TWO-2", "PART-A", 100, "DEFAULT"),
			wrote:  wroteClear("CL-TWO-2")},
	})
}

// clearHomeRig is the smallest dedicated loader a home clear takes: home
// CHW-H pinned to CHW-PART (capacity 10) with a full carrier CHW-E on it,
// buffer CHW-S holding the partial CHW-P (4), buffer CHW-X open. The arms past
// the adapter are engine/clear_home_docker_test.go's.
func clearHomeRig(t *testing.T, f *doorFix) (e, p *bins.Bin) {
	t.Helper()
	testutil.MustNoErr(t, f.db.CreatePayload(&payloads.Payload{Code: "CHW-PART", UOPCapacity: 10}), "the part")
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, f.db.CreateNode(n), "node "+name)
		return n
	}
	home, s, x := mk("CHW-H"), mk("CHW-S"), mk("CHW-X")
	id, err := f.db.CreateLoader(loaders.Loader{Name: "CHW", Role: "produce",
		Layout: protocol.LoaderLayoutDedicatedPositions, Replenishment: "operator"})
	testutil.MustNoErr(t, err, "loader")
	for _, h := range []loaders.Home{
		{LoaderID: id, PositionNodeID: home.ID, PayloadCode: "CHW-PART", Kind: loaders.HomeKindHome},
		{LoaderID: id, PositionNodeID: s.ID, Kind: loaders.HomeKindBuffer, SortOrder: 1},
		{LoaderID: id, PositionNodeID: x.ID, Kind: loaders.HomeKindBuffer, SortOrder: 2},
	} {
		testutil.MustNoErr(t, f.db.UpsertLoaderHome(h), "position")
	}
	stand := func(nodeID int64, label string, uop int) *bins.Bin {
		b := testdb.CreateBinAtNode(t, f.db, "CHW-PART", nodeID, label)
		testutil.MustNoErr(t, f.db.SetBinManifest(b.ID, `{"items":[]}`, "CHW-PART", uop), "manifest "+label)
		testutil.MustNoErr(t, f.db.ConfirmBinManifest(b.ID, ""), "confirm "+label)
		return binByLabel(t, f.db, label)
	}
	return stand(home.ID, "CHW-E", 10), stand(s.ID, "CHW-P", 4)
}

// TestClearHomeDoor_AdapterArms pins POST /api/telemetry/clear-home's own arms
// (apiClearHome): its two refusals, written as the other doors write theirs,
// and the engine's answer passed through with its status and its
// ClearHomeResponse body — a refusal's body carries status "error" beside the
// sentence, unlike the adapter's own.
func TestClearHomeDoor_AdapterArms(t *testing.T) {
	t.Parallel()
	runDoorArms(t, func(h *Handlers) http.HandlerFunc { return h.apiClearHome }, "/api/telemetry/clear-home", []doorArm{
		{name: "an undecodable body is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return "{", nil },
			answer:  refusedWith(http.StatusBadRequest, "unexpected EOF")},
		{name: "no home_node is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				e, p := clearHomeRig(t, f)
				return map[string]any{"station": "edge.chw"}, []int64{e.ID, p.ID}
			},
			answer: refusedWith(http.StatusBadRequest, "home_node and station are required")},
		{name: "no station is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				e, p := clearHomeRig(t, f)
				return map[string]any{"home_node": "CHW-H"}, []int64{e.ID, p.ID}
			},
			answer: refusedWith(http.StatusBadRequest, "home_node and station are required")},
		{name: "the engine's refusal passes through",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				e, p := clearHomeRig(t, f)
				return map[string]any{"home_node": "CHW-NOPE", "station": "edge.chw"}, []int64{e.ID, p.ID}
			},
			// The engine's refusal carries Core's rows for the home it named.
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return http.StatusNotFound, doorJSON(t, protocol.ClearHomeResponse{Status: "error",
					Error: `node "CHW-NOPE" not found`, Rows: doorRows(t, f, "CHW-NOPE")})
			}},
		{name: "the engine's accept passes through",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				e, p := clearHomeRig(t, f)
				return map[string]any{"home_node": "CHW-H", "station": "edge.chw"}, []int64{e.ID, p.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				e := binByLabel(t, f.db, "CHW-E")
				var parent string
				testutil.MustNoErr(t, f.db.QueryRow(`SELECT edge_uuid FROM orders WHERE edge_uuid LIKE 'core-clear-%'`).Scan(&parent), "the parent")
				return http.StatusOK, doorJSON(t, protocol.ClearHomeResponse{Status: "ok", Arm: protocol.ClearArmSpot,
					BinID: e.ID, DeltaEpoch: e.DeltaEpoch, ParentUUID: parent})
			},
			// The carrier is zeroed and claimed by the empty's move, which is
			// dispatched at once; the partial is held by reservation, not on its
			// row. Three orders (the parent, the partial's move, the empty's
			// move), each projected to the station, and the dispatched move's ack.
			wrote: func(f *doorFix) (map[int64][]string, map[string]int) {
				return map[int64][]string{binIDOf(f, "CHW-E"): {"claimed_by", "delta_epoch", "loaded_at", "manifest",
						"manifest_confirmed", "payload_code", "uop_remaining", "updated_at"}},
					map[string]int{"orders": 3, "bin_uop_ledger": 1, "outbox": 4}
			}},
	})
}

// TestHeldPartialsDoor_Arms pins GET /api/telemetry/held-partials
// (apiTelemetryHeldPartials): the refusal, the empty answer (an empty list,
// never null), and the list. A read writes nothing.
func TestHeldPartialsDoor_Arms(t *testing.T) {
	t.Parallel()
	ask := func(t *testing.T, f *doorFix, query string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		f.h.apiTelemetryHeldPartials(rec, httptest.NewRequest(http.MethodGet, "/api/telemetry/held-partials"+query, nil))
		return rec
	}
	t.Run("no payload is refused", func(t *testing.T) {
		t.Parallel()
		f := newDoorFix(t)
		assertDoor(t, ask(t, f, ""), http.StatusBadRequest, doorErr("payload is required"))
		assertDoor(t, ask(t, f, "?payload=%20"), http.StatusBadRequest, doorErr("payload is required"))
	})
	t.Run("a part with no held partial answers an empty list", func(t *testing.T) {
		t.Parallel()
		f := newDoorFix(t)
		ppHome(t, f.db, f.sd.StorageNode, "HP-NONE")
		testdb.CreateBinAtNode(t, f.db, "HP-NONE", f.sd.LineNode.ID, "HP-AT-LINE")
		assertDoor(t, ask(t, f, "?payload=HP-NONE"), http.StatusOK, "[]\n")
	})
	t.Run("the held partials of a part, oldest held first", func(t *testing.T) {
		t.Parallel()
		f := newDoorFix(t)
		ppHome(t, f.db, f.sd.StorageNode, "HP-X")
		var want []protocol.HeldPartial
		for i, at := range []string{"2026-01-02T03:04:05Z", "2026-01-01T03:04:05Z"} {
			label := fmt.Sprintf("HP-X-%d", i+1)
			b := testdb.CreateBinAtNode(t, f.db, "HP-X", f.sd.StorageNode.ID, label)
			testutil.MustNoErr(t, f.db.SetBinManifest(b.ID, `{"items":[]}`, "HP-X", 5+i), "a partial")
			_, err := f.db.DB.Exec(`UPDATE bins SET held_since=$1 WHERE id=$2`, at, b.ID)
			testutil.MustNoErr(t, err, "held since")
			want = append([]protocol.HeldPartial{{BinID: b.ID, BinLabel: label, NodeName: f.sd.StorageNode.Name,
				UOPRemaining: 5 + i, HeldSince: at}}, want...)
		}
		snap := snapDoor(t, f.db, want[0].BinID, want[1].BinID)
		assertDoor(t, ask(t, f, "?payload=HP-X"), http.StatusOK, doorJSON(t, want))
		snap.assertNothingWritten(t, f.db)
	})
}
