package www

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The query string is the whole redirect.
//
// /wall-display/{id} serves two different pages off one route: bare renders the
// framed version with Core's nav, ?kiosk=1 renders the chromeless page. A wall
// monitor is pointed at the kiosk form; a person clicking from the hub gets the
// framed one. So a redirect from the old /dashboard/{id} that forwards the path
// and drops the query does not 404 and does not error — it returns 301 and a
// page, and every floor screen in the plant quietly grows a nav bar. Nobody
// finds out from a log; somebody finds out by walking past a monitor.
//
// This is the same failure /heartbeat already had from the other direction (it
// redirected to the framed page having never attached ?kiosk=1 at all — fixed
// in handlers_cells.go alongside this), which is the argument for pinning it
// rather than trusting the one line to survive.
//
// No engine: handleWallDisplayMoved reads nothing but the URL, so the test
// mounts it on a bare chi router. That also makes the route pattern itself part
// of what is asserted — the {id} param has to come from somewhere.
func TestWallDisplayMoved_PreservesQueryString(t *testing.T) {
	t.Parallel()

	h := &Handlers{}
	r := chi.NewRouter()
	r.Get("/dashboard/{id}", h.handleWallDisplayMoved)

	cases := []struct {
		name     string
		req      string
		wantCode int
		wantLoc  string
	}{{
		name:     "kiosk form survives — the wall-monitor URL",
		req:      "/dashboard/4?kiosk=1",
		wantCode: http.StatusMovedPermanently,
		wantLoc:  "/wall-display/4?kiosk=1",
	}, {
		name:     "bare form stays bare — no stray ? appended",
		req:      "/dashboard/4",
		wantCode: http.StatusMovedPermanently,
		wantLoc:  "/wall-display/4",
	}, {
		name:     "every param carries, not just kiosk",
		req:      "/dashboard/12?kiosk=1&station=plant-a.line-1",
		wantCode: http.StatusMovedPermanently,
		wantLoc:  "/wall-display/12?kiosk=1&station=plant-a.line-1",
	}, {
		// The id is re-parsed rather than pasted through, so the Location this
		// emits is always a number the /wall-display/{id} route could have
		// produced — nothing from the request path reaches the header. A
		// non-numeric id was already broken at the old path, so refusing it
		// here costs nobody a working link. (Traversal is not tested: chi
		// cleans "/dashboard/../x" before routing, so it 404s at the mux and
		// never reaches this handler.)
		name:     "non-numeric id is refused, not forwarded",
		req:      "/dashboard/abc?kiosk=1",
		wantCode: http.StatusBadRequest,
		wantLoc:  "",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.req, nil))

			if rec.Code != tc.wantCode {
				t.Fatalf("GET %s: status = %d, want %d", tc.req, rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("Location"); got != tc.wantLoc {
				t.Errorf("GET %s: Location = %q, want %q", tc.req, got, tc.wantLoc)
			}
		})
	}
}
