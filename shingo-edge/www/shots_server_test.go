//go:build shots

package www

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"shingo/protocol/debuglog"
	"shingoedge/engine"
)

// shots_server_test.go — what the shots-tagged harnesses share: the real
// router served on a loopback port as a logged-in admin, the Chrome binary,
// and the scan of Chrome's log for a renderer that died.

// shotsRouter is the engine's real router, stopped when the test ends.
func shotsRouter(t *testing.T, eng *engine.Engine) http.Handler {
	t.Helper()
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	return router
}

// shotsServer serves router on a loopback port, with the harness's own pages
// (mount, may be nil) beside it, and logs in once so every request carries an
// admin's session. Closed when the test ends.
func shotsServer(t *testing.T, router http.Handler, mount func(mux *http.ServeMux)) *httptest.Server {
	t.Helper()
	// /events answers 204 here: an EventSource that is left open is a pending
	// network request, and headless Chrome's virtual-time budget waits on
	// pending requests forever, so the screenshot never fires. A 204 closes
	// the EventSource without a retry; the picture is drawn from the view
	// fetch and never needed the stream.
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// The frame driver (real_cells_shots_test.go): any real page, measured by
	// the overlap check. Every shots test may measure what it photographs.
	mux.HandleFunc("/__shots/frame", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(shotFrameDriver(r.URL.Query())))
	})
	if mount != nil {
		mount(mux)
	}
	mux.Handle("/", router)

	// THE DESKTOP PAGE IS ADMIN-GATED, AND HEADLESS CHROME CANNOT LOG IN.
	// There is no CLI flag for a cookie or a header, so the alternative to this
	// shim is driving a login form with a screenshot tool that cannot drive
	// anything. The shim carries the session cookie an admin would have, taken
	// from a real /login round trip against this same router — so the page is
	// rendered by the real handler behind the real middleware, and only the
	// browser's inability to hold a cookie is worked around.
	var adminCookie *http.Cookie
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminCookie != nil && r.URL.Path != "/login" {
			r.AddCookie(adminCookie)
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(authed)
	t.Cleanup(srv.Close)

	// First login creates the admin user (handlers_admin_pages.go handleLogin).
	//
	// A client that does NOT follow redirects: handleLogin answers 303 and sets
	// the session on THAT response, and the default client follows the 303 to a
	// page that (with no cookie yet) bounces back to /login — so resp.Cookies()
	// reads the last hop and comes back empty.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if resp, err := noFollow.PostForm(srv.URL+"/login", url.Values{
		"username": {"shots"}, "password": {"shots"},
	}); err == nil {
		for _, c := range resp.Cookies() {
			adminCookie = c
		}
		resp.Body.Close()
	}
	if adminCookie == nil {
		t.Fatal("no session cookie from /login — the desktop shots would photograph the login page")
	}
	return srv
}

func chromeBinary() string {
	if p := os.Getenv("COMPOSER_SHOTS_CHROME"); p != "" {
		return p
	}
	candidates := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	if runtime.GOOS != "windows" {
		candidates = []string{"google-chrome", "chromium", "chromium-browser"}
	}
	for _, c := range candidates {
		if strings.Contains(c, string(os.PathSeparator)) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// rendererFailure says whether a line of --enable-logging=stderr means the
// PAGE failed, as opposed to Chrome talking about itself.
//
// AN ALLOW-LIST, and it used to be "contains ERROR:". This scan reads the
// BROWSER process's log — the comment where it is set up says so, and says
// why: --headless=new does not put the renderer's console on stderr, so the
// page's own errors are read out of the DOM instead (data-pd-error,
// checkPanelFits). What lands here is Chrome's own infrastructure, and on
// 2026-09-13 two different pieces of it turned the whole shots run red in a
// row: GCM push registration failing with PHONE_REGISTRATION_ERROR because
// the box could not reach Google (a VPN going up is enough), and the default
// web-app installer failing to install chat.google.com. Both were reported as
// "the page logged", against a screenshot of a press.
//
// Denying each one as it appeared would be a list that grows every time
// Chrome adds a background feature. The thing this scan is actually for is a
// renderer that died — a screenshot of a crashed tab is a PNG, and would pass
// the size check — so it asks for that instead.
func rendererFailure(line string) bool {
	for _, fatal := range []string{
		"Uncaught",                 // a page script threw during load
		"Renderer process",         // "Renderer process crashed", "...not responding"
		"renderer process crashed", //
		"STATUS_ACCESS_VIOLATION",  // a renderer segfault on Windows
		"Check failed:",            // a Blink CHECK, which takes the renderer with it
		"Fatal error",
	} {
		if strings.Contains(line, fatal) {
			return true
		}
	}
	return false
}
