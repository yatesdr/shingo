package www

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStationView_CarriesNoStationClock: the view's copy of the station
// carries no clock nothing on the glass draws. last_seen_at and updated_at
// moved on every throttled Touch (15 s), so an idle board's JSON changed four
// times a minute and the board rebuilt itself for nothing.
func TestStationView_CarriesNoStationClock(t *testing.T) {
	_, router := newOperatorStationsRouter(t)
	pid := seedProcess(t, "ClockLine")
	sid := seedOperatorStation(t, pid, "OS-CLOCK-1", "ClockStation")
	doRequest(t, router, "GET", "/api/operator-stations/"+itoa(sid)+"/view", nil, nil)
	resp := doRequest(t, router, "GET", "/api/operator-stations/"+itoa(sid)+"/view", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	var view struct {
		Station map[string]json.RawMessage `json:"station"`
	}
	decodeJSON(t, resp, &view)
	for _, k := range []string{"last_seen_at", "updated_at", "created_at"} {
		if _, ok := view.Station[k]; ok {
			t.Errorf("the view's station carries %s, a clock nothing on the glass draws", k)
		}
	}
	for _, k := range []string{"id", "name", "health_status", "process_id"} {
		if _, ok := view.Station[k]; !ok {
			t.Errorf("the view's station lost %s", k)
		}
	}
}

// TestStationView_AnswersAConditionalGet: /view sends an ETag of its body, and
// a GET quoting it gets 304 and no body while the view is unchanged — a Touch
// that moves the station's clock included. A change to what the view carries
// gets the new body and a new ETag.
func TestStationView_AnswersAConditionalGet(t *testing.T) {
	_, router := newOperatorStationsRouter(t)
	pid := seedProcess(t, "EtagLine")
	sid := seedOperatorStation(t, pid, "OS-ETAG-1", "EtagStation")
	path := "/api/operator-stations/" + itoa(sid) + "/view"
	get := func(etag string) *http.Response {
		req := httptest.NewRequest("GET", path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Result()
	}
	first := get("")
	assertStatus(t, first, http.StatusOK)
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("/view sends no ETag")
	}
	again := get(etag)
	body, err := io.ReadAll(again.Body)
	if err != nil {
		t.Fatalf("read the conditional answer: %v", err)
	}
	if again.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("unchanged view with its ETag: status %d, %d bytes; want 304 and no body", again.StatusCode, len(body))
	}
	// Touch's clock moves; the view's body does not.
	if _, err := testDB.Exec(`UPDATE operator_stations SET last_seen_at=datetime('now','+1 hour'),
		updated_at=datetime('now','+1 hour') WHERE id=?`, sid); err != nil {
		t.Fatalf("move the station's clock: %v", err)
	}
	if r := get(etag); r.StatusCode != http.StatusNotModified {
		t.Errorf("after the station's clock moved: status %d, want 304", r.StatusCode)
	}
	if _, err := testDB.Exec(`UPDATE operator_stations SET name='EtagStation-2' WHERE id=?`, sid); err != nil {
		t.Fatalf("rename: %v", err)
	}
	changed := get(etag)
	if changed.StatusCode != http.StatusOK || changed.Header.Get("ETag") == etag {
		t.Errorf("after a rename: status %d, ETag %q (was %q); want 200 and a new ETag",
			changed.StatusCode, changed.Header.Get("ETag"), etag)
	}
}
