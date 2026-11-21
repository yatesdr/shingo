//go:build shots

package www

import (
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingocore/config"
)

// toast_shots_test.go — Core's Test Orders page with an order that failed, for
// the toasts' move onto shared/notice.js: the failure arrives on the event
// stream and stays up, with its ×, until it is closed (the page's own copy
// gave it 6 s).
//
//	core-test-orders-failed-1360.png        light theme
//	core-test-orders-failed-dark-1360.png   dark theme
//
// THE PAGE IS THE REAL ONE: its template, static files and modules, on the
// config shots' stub engine (configShotsEngine), so it needs no database. Its
// lists answer empty. The event stream is a stand-in (testOrdersShotStream)
// that says connected and then sends one order-update for a failed order, as
// Core's /events does: an open EventSource keeps headless Chrome's virtual
// time from running out, and a closed one puts the lost-connection banner up.
// Run:
//
//	cd shingo-core && TOAST_SHOTS_OUT=<dir> go test -tags shots -count=1 \
//	    -run '^TestTestOrdersToastShots$' -v ./www/
func TestTestOrdersToastShots(t *testing.T) {
	out := os.Getenv("TOAST_SHOTS_OUT")
	if out == "" {
		t.Skip("TOAST_SHOTS_OUT not set")
	}
	chrome := coreChromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := testOrdersShotsServer(t)
	for _, s := range []struct {
		file, theme string
		scheme      int // Blink preferredColorScheme: 0 dark, 1 light
	}{
		{"core-test-orders-failed-1360.png", "light", 1},
		{"core-test-orders-failed-dark-1360.png", "dark", 0},
	} {
		target := filepath.Join(out, s.file)
		_ = os.Remove(target)
		u := srv.URL + "/__shots/theme?t=" + s.theme + "&to=%2Ftest-orders"
		testOrdersChrome(t, chrome, s.scheme, u, "--screenshot="+target)
		if info, err := os.Stat(target); err != nil || info.Size() < 10_000 {
			t.Fatalf("%s missing or blank: %v", target, err)
		}
		dom := testOrdersChrome(t, chrome, s.scheme, u, "--dump-dom")
		want := `<div class="notice-toast notice-toast--error is-sticky" role="alert"`
		if !strings.Contains(dom, want) || !strings.Contains(dom, "Order #4182 failed: ") {
			t.Errorf("%s: the failed order is not a sticky error toast on the notice", s.file)
		}
		if strings.Contains(dom, "to-toast") {
			t.Errorf("%s: the page's own toast copy drew", s.file)
		}
		t.Logf("wrote %s", target)
	}
}

// testOrdersChrome runs headless Chrome on u, a profile of its own each run
// (two runs on one profile hand the second URL to the first browser), and
// returns its stdout: the DOM, for --dump-dom. WaitDelay stops a helper
// process that keeps the pipe open from holding the test.
func testOrdersChrome(t *testing.T, chrome string, scheme int, u, flag string) string {
	t.Helper()
	cmd := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir="+t.TempDir(), "--blink-settings=preferredColorScheme="+strconv.Itoa(scheme),
		"--hide-scrollbars", "--window-size=1360,900", "--virtual-time-budget=6000", flag, u)
	cmd.WaitDelay = 10 * time.Second
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("chrome %s: %v", flag, err)
	}
	return string(raw)
}

// testOrdersShotStream is the stand-in EventSource, put in the page before
// its modules run.
const testOrdersShotStream = `<script>(function () {
  function Stream(url) { this.l = {}; var s = this;
    setTimeout(function () { s.emit('connected', {}); }, 50);
    setTimeout(function () { s.emit('order-update', { type: 'failed', order_id: 4182,
      detail: 'no bin of SYN-PART07A in storage' }); }, 1500); }
  Stream.prototype.addEventListener = function (t, fn) { (this.l[t] = this.l[t] || []).push(fn); };
  Stream.prototype.removeEventListener = function () {};
  Stream.prototype.close = function () {};
  Stream.prototype.emit = function (t, d) {
    (this.l[t] || []).forEach(function (fn) { fn({ type: t, data: JSON.stringify(d), lastEventId: '' }); });
  };
  window.EventSource = Stream;
})();</script>`

func testOrdersShotsServer(t *testing.T) *httptest.Server {
	cfg := config.Defaults()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := configShotsEngine{&configPinEngine{cfg: cfg, path: path}}
	h := &Handlers{
		engine: eng, orchestration: eng,
		sessions: newSessionStore("shots-secret"),
		tmpls:    map[string]*template.Template{},
		boot:     takeBootSnapshot(cfg),
	}
	base := template.Must(template.New("").Funcs(templateFuncs(nil)).ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	h.tmpls["test-orders.html"] = template.Must(template.Must(base.Clone()).ParseFS(templateFS, "templates/test-orders.html"))

	r := chi.NewRouter()
	// The real static handler: the layout names every asset by its hashed URL
	// (assetURL), which only it serves.
	h.registerStatic(r)
	empty := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}
	for _, p := range []string{"/api/test-orders", "/api/test-orders/direct", "/api/test-commands",
		"/api/test-orders/robots", "/api/test-orders/scene-points"} {
		r.Get(p, empty)
	}
	r.Get("/test-orders", func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.render(rec, r, "test-orders.html", map[string]any{"Page": "test-orders", "NodesJSON": "[]", "PayloadsJSON": "[]"})
		page := rec.Body.String()
		if !strings.Contains(page, "</body>") {
			t.Errorf("the page has no </body> to put the stream before")
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write([]byte(strings.Replace(page, "</body>", testOrdersShotStream+"</body>", 1)))
	})
	r.Get("/__shots/theme", func(w http.ResponseWriter, r *http.Request) {
		sess, _ := h.sessions.Get(r, sessionName)
		sess.Values["authenticated"] = true
		sess.Values["username"] = "shots"
		_ = sess.Save(r, w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><script>localStorage.setItem('theme', %s); location.replace(%s);</script>`,
			strconv.Quote(r.URL.Query().Get("t")), strconv.Quote(r.URL.Query().Get("to")))
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}
