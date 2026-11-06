package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// handlers_catid_health_test.go — the two read routes of the part-identity
// health strip, over the stub engine's canned items.

func catidHealthGet(t *testing.T, path string) (int, []byte) {
	t.Helper()
	h, r := newTestHandlers(t)
	r.Get("/api/processes/{id}/catid-health", h.apiCatidHealth)
	r.Get("/api/catid-health", h.apiCatidHealthSummary)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestCatidHealthRoute_ItemsAndAnEmptyArray: a process's items under "items",
// and a process with none answers [] rather than null.
func TestCatidHealthRoute_ItemsAndAnEmptyArray(t *testing.T) {
	code, body := catidHealthGet(t, "/api/processes/1/catid-health")
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || len(got.Items) != 1 ||
		got.Items[0]["kind"] != "wrong_part" || got.Items[0]["severity"] != "red" {
		t.Fatalf("process 1: %d %s, want one red wrong_part item", code, body)
	}
	code, body = catidHealthGet(t, "/api/processes/2/catid-health")
	if code != http.StatusOK || string(body) != "{\"items\":[]}\n" {
		t.Errorf("process 2: %d %q, want {\"items\":[]}", code, body)
	}
	if code, _ := catidHealthGet(t, "/api/processes/x/catid-health"); code != http.StatusBadRequest {
		t.Errorf("a bad id = %d, want 400", code)
	}
}

// TestCatidHealthRoute_SummaryPerProcess: the homepage's line per process.
func TestCatidHealthRoute_SummaryPerProcess(t *testing.T) {
	code, body := catidHealthGet(t, "/api/catid-health")
	var rows []map[string]any
	if code != http.StatusOK || json.Unmarshal(body, &rows) != nil || len(rows) != 1 {
		t.Fatalf("summary: %d %s", code, body)
	}
	r := rows[0]
	if r["process_id"] != float64(1) || r["worst"] != "red" || r["count"] != float64(1) || r["headline"] != "Wrong part" || r["name"] != "P1" {
		t.Errorf("summary row = %v", r)
	}
}

func catidHoldDo(t *testing.T, method, path string) (int, []byte) {
	t.Helper()
	h, r := newTestHandlers(t)
	r.Get("/api/catid-holds/{id}/preview", h.apiPreviewCatidHold)
	r.Post("/api/catid-holds/{id}/charge", h.apiChargeCatidHold)
	r.Post("/api/catid-holds/{id}/write-off", h.apiWriteOffCatidHold)
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestCatidHoldRoutes: the preview, the Charge and the Write off; a released
// hold is a conflict, a bad id a bad request.
func TestCatidHoldRoutes(t *testing.T) {
	code, body := catidHoldDo(t, http.MethodGet, "/api/catid-holds/1/preview")
	var p map[string]any
	if code != http.StatusOK || json.Unmarshal(body, &p) != nil || p["style_name"] != "S1" || p["refusal"] != nil {
		t.Errorf("preview 1: %d %s, want style S1 and no refusal", code, body)
	}
	for _, verb := range []string{"charge", "write-off"} {
		if code, body := catidHoldDo(t, http.MethodPost, "/api/catid-holds/1/"+verb); code != http.StatusOK {
			t.Errorf("%s 1: %d %s, want 200", verb, code, body)
		}
		if code, _ := catidHoldDo(t, http.MethodPost, "/api/catid-holds/2/"+verb); code != http.StatusConflict {
			t.Errorf("%s of a released hold: %d, want 409", verb, code)
		}
		if code, _ := catidHoldDo(t, http.MethodPost, "/api/catid-holds/x/"+verb); code != http.StatusBadRequest {
			t.Errorf("%s with a bad id: %d, want 400", verb, code)
		}
	}
}

// TestCutoverNowRoutes: the offer is a read; the one-tap answers 409 once
// the offer no longer holds.
func TestCutoverNowRoutes(t *testing.T) {
	do := func(method, path string) (int, []byte) {
		h, r := newTestHandlers(t)
		r.Get("/api/processes/{id}/changeover/cutover-now", h.apiCutoverNowOffer)
		r.Post("/api/processes/{id}/changeover/cutover-now", h.apiCutOverNow)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code, rec.Body.Bytes()
	}
	code, body := do(http.MethodGet, "/api/processes/1/changeover/cutover-now")
	var off map[string]any
	if code != http.StatusOK || json.Unmarshal(body, &off) != nil || off["offered"] != true || off["catid"] != "1002" {
		t.Errorf("offer 1: %d %s", code, body)
	}
	if code, body := do(http.MethodGet, "/api/processes/2/changeover/cutover-now"); code != http.StatusOK || string(body) != "{\"offered\":false}\n" {
		t.Errorf("offer 2: %d %q, want offered false", code, body)
	}
	if code, _ := do(http.MethodPost, "/api/processes/1/changeover/cutover-now"); code != http.StatusOK {
		t.Errorf("one-tap 1 = %d, want 200", code)
	}
	if code, _ := do(http.MethodPost, "/api/processes/2/changeover/cutover-now"); code != http.StatusConflict {
		t.Errorf("one-tap not offered = %d, want 409", code)
	}
	if code, _ := do(http.MethodPost, "/api/processes/x/changeover/cutover-now"); code != http.StatusBadRequest {
		t.Errorf("one-tap bad id = %d, want 400", code)
	}
}
