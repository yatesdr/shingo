//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// Characterization tests for handlers_payload_templates.go — HTML form
// handlers (POST /payloads/create etc.) and JSON /api/payloads/templates/*
// endpoints that create/update full payload templates (payload + bin types +
// manifest items in one shot).

// postFormPL drives a form-encoded POST handler for payload-template tests.
func postFormPL(t *testing.T, handler http.HandlerFunc, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// --- handlePayloadCreate ----------------------------------------------------

func TestHandlePayloadCreate_HappyPath(t *testing.T) {
	h, db := testHandlers(t)

	form := url.Values{}
	form.Set("code", "FORM-PL-1")
	form.Set("description", "via form")
	form.Set("uop_capacity", "25")

	rec := postFormPL(t, h.handlePayloadCreate, "/payloads/create", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/payloads" {
		t.Errorf("redirect: got %q, want /payloads", loc)
	}

	got, err := db.GetPayloadByCode("FORM-PL-1")
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if got.Description != "via form" || got.UOPCapacity != 25 {
		t.Errorf("payload: got %+v", got)
	}
}

func TestHandlePayloadCreate_DuplicateCode(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	form := url.Values{}
	form.Set("code", sd.Payload.Code) // duplicate of PART-A
	form.Set("description", "dup")
	form.Set("uop_capacity", "1")

	rec := postFormPL(t, h.handlePayloadCreate, "/payloads/create", form)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// --- handlePayloadUpdate ----------------------------------------------------

func TestHandlePayloadUpdate_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	form := url.Values{}
	form.Set("id", fmt.Sprint(sd.Payload.ID))
	form.Set("code", "PART-A-RENAMED")
	form.Set("description", "renamed")
	form.Set("uop_capacity", "99")

	rec := postFormPL(t, h.handlePayloadUpdate, "/payloads/update", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	got, _ := db.GetPayload(sd.Payload.ID)
	if got.Code != "PART-A-RENAMED" || got.Description != "renamed" || got.UOPCapacity != 99 {
		t.Errorf("payload after update: got %+v", got)
	}
}

func TestHandlePayloadUpdate_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)

	form := url.Values{}
	form.Set("id", "notanint")

	rec := postFormPL(t, h.handlePayloadUpdate, "/payloads/update", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlePayloadUpdate_NotFound(t *testing.T) {
	h, _ := testHandlers(t)

	form := url.Values{}
	form.Set("id", "9999999")
	form.Set("code", "X")

	rec := postFormPL(t, h.handlePayloadUpdate, "/payloads/update", form)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- handlePayloadDelete ----------------------------------------------------

func TestHandlePayloadDelete_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	pl := &payloads.Payload{Code: "DELME-1", Description: "to delete"}
	if err := db.CreatePayload(pl); err != nil {
		t.Fatalf("seed: %v", err)
	}

	form := url.Values{}
	form.Set("id", fmt.Sprint(pl.ID))

	rec := postFormPL(t, h.handlePayloadDelete, "/payloads/delete", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := db.GetPayload(pl.ID); err == nil {
		t.Error("payload should be gone after delete")
	}
}

func TestHandlePayloadDelete_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)

	form := url.Values{}
	form.Set("id", "abc")

	rec := postFormPL(t, h.handlePayloadDelete, "/payloads/delete", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiCreatePayloadTemplate -----------------------------------------------

func TestApiCreatePayloadTemplate_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	// Seed a bin type so we can associate it.
	bt := &bins.BinType{Code: "BT-TMPL", Description: "for template"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	rec := postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create",
		map[string]any{
			"code":         "TMPL-1",
			"description":  "template",
			"uop_capacity": 10,
			"bin_type_ids": []int64{bt.ID},
			"manifest": []map[string]any{
				{"part_number": "P1", "quantity": 2},
				{"part_number": "P2", "quantity": 3},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var created payloads.Payload
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == 0 || created.Code != "TMPL-1" {
		t.Errorf("response: got %+v", created)
	}

	// Verify bin types and manifest items were persisted.
	bts, _ := db.ListBinTypesForPayload(created.ID)
	if len(bts) != 1 || bts[0].ID != bt.ID {
		t.Errorf("bin types: got %+v, want single %d", bts, bt.ID)
	}
	items, _ := db.ListPayloadManifest(created.ID)
	if len(items) != 2 {
		t.Errorf("manifest items: got %d, want 2", len(items))
	}
}

func TestApiCreatePayloadTemplate_MinimalBody(t *testing.T) {
	h, db := testHandlers(t)

	// No bin_types, no manifest — just create the payload.
	rec := postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create",
		map[string]any{"code": "TMPL-MIN", "description": "m", "uop_capacity": 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var created payloads.Payload
	_ = json.NewDecoder(rec.Body).Decode(&created)
	got, err := db.GetPayload(created.ID)
	if err != nil {
		t.Fatalf("persisted: %v", err)
	}
	if got.Code != "TMPL-MIN" {
		t.Errorf("code: got %q", got.Code)
	}
}

func TestApiCreatePayloadTemplate_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create", []byte("garbage"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiUpdatePayloadTemplate -----------------------------------------------

func TestApiUpdatePayloadTemplate_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	pl := &payloads.Payload{Code: "UPD-1", Description: "orig", UOPCapacity: 5}
	if err := db.CreatePayload(pl); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bt := &bins.BinType{Code: "BT-UPD", Description: "for update"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update",
		map[string]any{
			"id":           pl.ID,
			"code":         "UPD-1-X",
			"description":  "updated",
			"uop_capacity": 20,
			"bin_type_ids": []int64{bt.ID},
			"manifest": []map[string]any{
				{"part_number": "Q1", "quantity": 4},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	got, _ := db.GetPayload(pl.ID)
	if got.Code != "UPD-1-X" || got.UOPCapacity != 20 {
		t.Errorf("payload: got %+v", got)
	}
	bts, _ := db.ListBinTypesForPayload(pl.ID)
	if len(bts) != 1 || bts[0].ID != bt.ID {
		t.Errorf("bin types: got %+v", bts)
	}
	items, _ := db.ListPayloadManifest(pl.ID)
	if len(items) != 1 || items[0].PartNumber != "Q1" {
		t.Errorf("manifest: got %+v", items)
	}
}

func TestApiUpdatePayloadTemplate_NotFound(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update",
		map[string]any{"id": 9999999, "code": "X"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "not found")
}

// --- apiGetPayloadManifestTemplate ------------------------------------------

func TestApiGetPayloadManifestTemplate_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	if err := db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: sd.Payload.ID, PartNumber: "X", Quantity: 1,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := getPlain(t, h.apiGetPayloadManifestTemplate,
		"/api/payloads/templates/manifest?id="+fmt.Sprint(sd.Payload.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var items []*payloads.ManifestItem
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 || items[0].PartNumber != "X" {
		t.Errorf("items: got %+v", items)
	}
}

func TestApiGetPayloadManifestTemplate_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiGetPayloadManifestTemplate, "/api/payloads/templates/manifest?id=x")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiSavePayloadManifestTemplate -----------------------------------------

func TestApiSavePayloadManifestTemplate_ReplacesItems(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	// Seed one old item to verify it gets replaced.
	if err := db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: sd.Payload.ID, PartNumber: "OLD", Quantity: 1,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := postJSON(t, h.apiSavePayloadManifestTemplate, "/api/payloads/templates/manifest",
		map[string]any{
			"payload_id": sd.Payload.ID,
			"items": []map[string]any{
				{"part_number": "NEW1", "quantity": 2, "description": "d1"},
				{"part_number": "NEW2", "quantity": 3, "description": "d2"},
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	items, _ := db.ListPayloadManifest(sd.Payload.ID)
	if len(items) != 2 {
		t.Fatalf("items: got %d, want 2", len(items))
	}
	if items[0].PartNumber != "NEW1" || items[1].PartNumber != "NEW2" {
		t.Errorf("items: got %+v", items)
	}
}

func TestApiSavePayloadManifestTemplate_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiSavePayloadManifestTemplate, "/api/payloads/templates/manifest", []byte("{"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiGetPayloadBinTypes --------------------------------------------------

func TestApiGetPayloadBinTypes_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	if err := db.SetPayloadBinTypes(sd.Payload.ID, []int64{sd.BinType.ID}); err != nil {
		t.Fatalf("seed bin types: %v", err)
	}

	rec := getPlain(t, h.apiGetPayloadBinTypes,
		"/api/payloads/templates/bin-types?id="+fmt.Sprint(sd.Payload.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var bts []*bins.BinType
	if err := json.NewDecoder(rec.Body).Decode(&bts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bts) != 1 || bts[0].ID != sd.BinType.ID {
		t.Errorf("bin types: got %+v", bts)
	}
}

func TestApiGetPayloadBinTypes_InvalidID(t *testing.T) {
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiGetPayloadBinTypes, "/api/payloads/templates/bin-types?id=abc")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- apiSavePayloadBinTypes -------------------------------------------------

func TestApiSavePayloadBinTypes_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiSavePayloadBinTypes, "/api/payloads/templates/bin-types",
		map[string]any{
			"payload_id":   sd.Payload.ID,
			"bin_type_ids": []int64{sd.BinType.ID},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	bts, _ := db.ListBinTypesForPayload(sd.Payload.ID)
	if len(bts) != 1 || bts[0].ID != sd.BinType.ID {
		t.Errorf("bin types: got %+v", bts)
	}
}

func TestApiSavePayloadBinTypes_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postRaw(t, h.apiSavePayloadBinTypes, "/api/payloads/templates/bin-types", []byte("nope"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// --- handlePayloadsPage -----------------------------------------------------

func TestHandlePayloadsPage_RendersHTML(t *testing.T) {
	h, db := testHandlersForPages(t)
	testdb.SetupStandardData(t, db)

	req := httptest.NewRequest(http.MethodGet, "/payloads", nil)
	rec := httptest.NewRecorder()
	h.handlePayloadsPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "PART-A") {
		t.Errorf("rendered HTML missing 'PART-A'; body len=%d", len(body))
	}
}
