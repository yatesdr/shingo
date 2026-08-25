package www

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingoedge/engine"
)

// N1-d — the changeover-wide release has a door again, and this time it has a
// caller. Its predecessor was retired for being a route nothing called; the
// test that matters is therefore that the shape a UI can actually use works:
// one POST, the operator named, the counts back.

func changeoverReleaseRouter(h *Handlers) chi.Router {
	r := chi.NewRouter()
	r.Post("/api/processes/{id}/changeover/release", h.apiReleaseChangeoverProcess)
	return r
}

func postRelease(t *testing.T, r chi.Router, path, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec, rec.Body.String()
}

func TestReleaseChangeoverProcess_ReleasesEveryLegAndReportsCounts(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandlers(t)
	eng := h.orchestration.(*stubEngine)
	eng.changeoverReleaseResult = engine.ReleaseChangeoverWaitResult{Released: 4, Pending: 0}

	rec, body := postRelease(t, changeoverReleaseRouter(h),
		"/api/processes/7/changeover/release", `{"called_by":"press-1-ops"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}
	var got engine.ReleaseChangeoverWaitResult
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if got.Released != 4 {
		t.Errorf("released = %d, want 4 — the operator needs to see how much moved, because "+
			"a leg that could not be released yet is one they have to come back for", got.Released)
	}
	if eng.lastChangeoverReleaseProcessID != 7 {
		t.Errorf("released process %d, want 7", eng.lastChangeoverReleaseProcessID)
	}
	if eng.lastChangeoverReleaseDisposition == nil ||
		eng.lastChangeoverReleaseDisposition.CalledBy != "press-1-ops" {
		t.Errorf("called_by did not reach the engine: %+v", eng.lastChangeoverReleaseDisposition)
	}
}

// The shared release-body guard applies here too: a bare POST is the
// disposition-bypass fingerprint from the 2026-04-27 plant test.
func TestReleaseChangeoverProcess_RequiresABody(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandlers(t)

	rec, body := postRelease(t, changeoverReleaseRouter(h), "/api/processes/7/changeover/release", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — body = %s", rec.Code, body)
	}
}

// A partial failure must not read as success: the engine joins the failing
// nodes and the handler surfaces them, or one cell's material stays where it
// was while the operator is told everything moved.
func TestReleaseChangeoverProcess_SurfacesPartialFailure(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandlers(t)
	eng := h.orchestration.(*stubEngine)
	eng.changeoverReleaseErr = errors.New("node PLN_002 (supply): core refused")

	rec, body := postRelease(t, changeoverReleaseRouter(h),
		"/api/processes/7/changeover/release", `{"called_by":"press-1-ops"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(body, "PLN_002") {
		t.Errorf("the failing node is not named in %s", body)
	}
}
