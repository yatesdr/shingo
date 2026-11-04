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
