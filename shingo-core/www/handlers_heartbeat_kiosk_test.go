//go:build docker

package www

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /heartbeat is a CHROMELESS route, and its redirect has to stay chromeless.
//
// The handler advertises a bare kiosk — its fallback is renderBare, and the
// route comment says "public, no nav, meant to run full-screen on a floor
// monitor". But when a heartbeat wall display exists it stops rendering and
// redirects to that display instead, and the hop was built without ?kiosk=1.
// It landed on the FRAMED page: Core's nav bar, an iframe border, on a screen
// bolted to a wall.
//
// THE FALLBACK IS UNREACHABLE, which is why this was never a conditional bug.
// engine.Start() calls SeedDefaultDashboards, which creates an enabled
// full-plant "Plant Heartbeat" if no heartbeat display exists — so every Core
// has one from first boot and /heartbeat has ALWAYS redirected. Both plants
// carry a row named exactly "Plant Heartbeat", i.e. the seed, unrenamed. The
// first assertion below pins that: a started engine with nobody having created
// anything still redirects. If a future change makes the bare kiosk reachable
// again, this test says so rather than the fallback quietly rotting further.
//
// The second assertion is the regression. Only it failed.
func TestHeartbeatKiosk_RedirectStaysChromeless(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)

	// Nothing created here on purpose — the seed is the point.
	rec := httptest.NewRecorder()
	h.handleHeartbeatKiosk(rec, httptest.NewRequest(http.MethodGet, "/heartbeat", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302: a started engine seeds an enabled heartbeat "+
			"display, so this route redirects rather than rendering its bare fallback", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/wall-display/") {
		t.Errorf("Location = %q, want a /wall-display/ path", loc)
	}
	if !strings.HasSuffix(loc, "?kiosk=1") {
		t.Errorf("Location = %q — a chromeless route redirected to the FRAMED page. "+
			"Every floor monitor pointed at /heartbeat grows Core's nav bar.", loc)
	}
}
