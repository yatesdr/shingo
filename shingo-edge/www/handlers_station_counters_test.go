package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// handlers_station_counters_test.go — the station-counter routes over the stub
// engine: a refusal is a 400 carrying its sentence.

func stationDo(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	h, r := newTestHandlers(t)
	r.Get("/api/processes/{id}/station-counters", h.apiListStationCounters)
	r.Post("/api/processes/{id}/station-counters", h.apiCreateStationCounter)
	r.Put("/api/station-counters/{id}", h.apiUpdateStationCounter)
	r.Delete("/api/station-counters/{id}", h.apiDeleteStationCounter)
	r.Put("/api/process-nodes/{id}/counted-at", h.apiSetCountedAt)
	r.Get("/api/process-nodes/{id}/countable", h.apiStationCountable)
	r.Get("/api/processes/{id}/end-of-line", h.apiEndOfLine)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

func TestStationCounterRoutes(t *testing.T) {
	code, body := stationDo(t, http.MethodGet, "/api/processes/1/station-counters", "")
	var list []map[string]any
	if code != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list) != 1 || list[0]["tag_name"] != "STATION_01" {
		t.Errorf("list 1: %d %s", code, body)
	}
	if code, body := stationDo(t, http.MethodGet, "/api/processes/2/station-counters", ""); code != http.StatusOK || body != "[]\n" {
		t.Errorf("list 2: %d %q, want []", code, body)
	}
	if code, _ := stationDo(t, http.MethodPost, "/api/processes/1/station-counters", `{"label":"load station","tag_name":"STATION_02"}`); code != http.StatusOK {
		t.Errorf("create = %d, want 200", code)
	}
	if code, body := stationDo(t, http.MethodPost, "/api/processes/2/station-counters", `{"tag_name":"X"}`); code != http.StatusBadRequest || !strings.Contains(body, "no production counter wired") {
		t.Errorf("create refused = %d %s, want 400 with the sentence", code, body)
	}
	if code, _ := stationDo(t, http.MethodPut, "/api/station-counters/7", `{"label":"load station","tag_name":"STATION_03"}`); code != http.StatusOK {
		t.Errorf("update = %d", code)
	}
	if code, _ := stationDo(t, http.MethodDelete, "/api/station-counters/7", ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
	if code, _ := stationDo(t, http.MethodPut, "/api/process-nodes/1/counted-at", `{"line_counter_id":7}`); code != http.StatusOK {
		t.Errorf("bind = %d", code)
	}
	if code, body := stationDo(t, http.MethodPut, "/api/process-nodes/2/counted-at", `{"line_counter_id":7}`); code != http.StatusBadRequest || !strings.Contains(body, "no live style consumes") {
		t.Errorf("bind refused = %d %s", code, body)
	}
	if code, body := stationDo(t, http.MethodGet, "/api/process-nodes/1/countable", ""); code != http.StatusOK || body != `{"countable":true,"refusal":""}`+"\n" {
		t.Errorf("dry run 1 = %d %q", code, body)
	}
	if code, body := stationDo(t, http.MethodGet, "/api/process-nodes/2/countable", ""); code != http.StatusOK ||
		!strings.Contains(body, `"countable":false`) || !strings.Contains(body, "no live style consumes") {
		t.Errorf("dry run 2 = %d %q, want 200 with the refusal's sentence", code, body)
	}
}

// TestCountingRoutes_CarryTheSlotsAndTheStopLabelled: the Counting block reads
// no route of its own. The end-of-line read carries the MES slots, each
// labelled with where its reading came from (the watch: discovered), the
// counted one marked; a process with none answers [] rather than null. The
// station counters' read carries each card's strokes since its last click and
// whether that is a stop.
func TestCountingRoutes_CarryTheSlotsAndTheStopLabelled(t *testing.T) {
	code, body := stationDo(t, http.MethodGet, "/api/processes/1/end-of-line", "")
	var eol struct {
		Slots []map[string]any `json:"slots"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &eol) != nil || len(eol.Slots) != 2 {
		t.Fatalf("end of line 1: %d %s, want two slots", code, body)
	}
	for _, s := range eol.Slots {
		if s["label"] != "discovered" {
			t.Errorf("slot %v is labelled %v, want discovered", s["slot"], s["label"])
		}
	}
	if c := eol.Slots[0]; c["counted"] != true || c["rising"] != true || c["catid"] != "1001" || c["part"] != true || c["count"] != float64(53) {
		t.Errorf("the counted slot = %v, want counted, rising, CATID 1001 (a part), count 53", c)
	}
	if c := eol.Slots[1]; c["counted"] != false || c["part"] != false || c["rising"] != false {
		t.Errorf("slot 2 = %v, want not counted, no part, not rising", c)
	}
	if code, body := stationDo(t, http.MethodGet, "/api/processes/2/end-of-line", ""); code != http.StatusOK || !strings.Contains(body, `"slots":[]`) {
		t.Errorf("end of line 2: %d %s, want \"slots\":[]", code, body)
	}
	code, body = stationDo(t, http.MethodGet, "/api/processes/1/station-counters", "")
	var cards []map[string]any
	if code != http.StatusOK || json.Unmarshal([]byte(body), &cards) != nil || len(cards) != 1 ||
		cards[0]["strokes_since_click"] != float64(25) || cards[0]["stopped"] != true {
		t.Errorf("station counters 1: %d %s, want the card's 25 strokes since its last click, stopped", code, body)
	}
}
