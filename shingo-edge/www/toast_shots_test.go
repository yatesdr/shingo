//go:build shots

package www

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
)

// toast_shots_test.go — the station's toasts on the real station page, at the
// kiosk's 1280x800, for the toasts' move onto shared/notice.js: one success,
// one error, three stacked (four shown, the oldest plain one gone to make
// room) and an exit toast.
//
//	station-toast-success-1280.png  a success: green, with its ×
//	station-toast-error-1280.png    a refusal, in its words: red, with its ×
//	station-toast-three-1280.png    four toasts shown, three up
//	station-toast-exit-1280.png     a refusal with a way out: its button and ×
//
// THE PAGE IS THE REAL ONE: the engine's router on a seeded plant
// (scenefixtures A, Press A1), the real template and modules. Two things are
// the harness's. The event stream is a stand-in that says connected and then
// nothing (toastShotDriver): an open EventSource keeps headless Chrome's
// virtual time from ever running out, and a closed one puts the lost-connection
// banner over the footer. And the toasts are raised through the station's own
// doors once the board has drawn: postAction against /__shots/ answers that
// refuse (plain, with an exit) or hold (202 pending), and the success the load
// modal says, with the options every station toast passes (STATION_TOASTS).
//
// Run:
//
//	cd shingo-edge && TOAST_SHOTS_OUT=<dir> go test -tags shots -count=1 \
//	    -run '^TestStationToastShots$' -v ./www/
func TestStationToastShots(t *testing.T) {
	out := os.Getenv("TOAST_SHOTS_OUT")
	if out == "" {
		t.Skip("TOAST_SHOTS_OUT not set")
	}
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}

	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	var station int64
	for _, id := range seeded.Stations {
		if station == 0 || id < station {
			station = id
		}
	}
	if station == 0 {
		t.Fatal("the seeded plant has no station")
	}
	cfg := config.Defaults()
	cfg.StationUID = "shots.press400"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)
	router := shotsRouter(t, eng)

	srv := shotsServer(t, router, func(mux *http.ServeMux) {
		mux.HandleFunc("/__shots/toast-driver.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			fmt.Fprintf(w, toastShotDriver, r.URL.Query().Get("shot"))
		})
		refuse := func(status int, body string) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}
		}
		mux.HandleFunc("/__shots/refuse", refuse(http.StatusConflict,
			`{"error":"No empty bin of SYN-BIN-A in storage: Core could not send one"}`))
		mux.HandleFunc("/__shots/refuse-exit", refuse(http.StatusConflict,
			`{"error":"A changeover to STYLE-B is armed on this process","exit":{"kind":"cancel_changeover","url":"/__shots/refuse","label":"Cancel changeover to STYLE-B"}}`))
		mux.HandleFunc("/__shots/pending", refuse(http.StatusAccepted,
			`{"status":"pending","pending":true,"notice":"sent — waiting for Core"}`))
		// The station page with the driver after its last tag: a classic
		// script, so it runs before the page's modules (which defer) and its
		// stand-in stream is the one the bus opens.
		mux.HandleFunc("/operator/station/", func(w http.ResponseWriter, r *http.Request) {
			// Asked for plain bytes, so the driver can be put into them.
			plain := r.Clone(r.Context())
			plain.Header.Del("Accept-Encoding")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, plain)
			page := rec.Body.String()
			tag := `<script src="/__shots/toast-driver.js?shot=` + r.URL.Query().Get("shot") + `"></script>`
			if !strings.Contains(page, "</body>") {
				t.Errorf("the station page has no </body> to put the driver before:\n%.400s", page)
			}
			page = strings.Replace(page, "</body>", tag+"</body>", 1)
			for k, v := range rec.Header() {
				if k != "Content-Length" && k != "Etag" {
					w.Header()[k] = v
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write([]byte(page))
		})
	})

	for _, s := range []struct{ shot, file, want string }{
		{"success", "station-toast-success-1280.png", "Bin loaded"},
		{"error", "station-toast-error-1280.png", "No empty bin of SYN-BIN-A"},
		{"three", "station-toast-three-1280.png", "Type the count first"},
		{"exit", "station-toast-exit-1280.png", "Cancel changeover to STYLE-B"},
	} {
		u := fmt.Sprintf("%s/operator/station/%d?shot=%s", srv.URL, station, s.shot)
		target := filepath.Join(out, s.file)
		_ = os.Remove(target)
		shotChrome(t, chrome, out, u, "--screenshot="+target)
		info, err := os.Stat(target)
		if err != nil || info.Size() < 10_000 {
			t.Fatalf("%s missing or blank: %v", target, err)
		}
		dom := shotChrome(t, chrome, out, u, "--dump-dom")
		if !strings.Contains(dom, `data-shot-done="`+s.shot+`"`) {
			t.Errorf("%s: the driver did not finish (no data-shot-done)", s.shot)
		}
		if !strings.Contains(dom, s.want) {
			t.Errorf("%s: the glass does not say %q", s.shot, s.want)
		}
		t.Logf("wrote %s (%d bytes); toasts up: %s", target, info.Size(), toastsUp(dom))
	}
}

// shotChrome runs headless Chrome at the kiosk's size on u and returns its
// stdout (the DOM, for --dump-dom).
func shotChrome(t *testing.T, chrome, dir, u string, flags ...string) string {
	t.Helper()
	args := append([]string{
		"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir=" + t.TempDir(), "--window-size=1280,800",
		"--blink-settings=preferredColorScheme=0", "--virtual-time-budget=6000",
	}, flags...)
	cmd := exec.Command(chrome, append(args, u)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("chrome %v: %v\n%s", flags, err, stderr.String())
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if rendererFailure(line) {
			t.Errorf("chrome %v: %s", flags, line)
		}
	}
	return stdout.String()
}

// toastsUp is the words of each toast in a dumped DOM, oldest first.
func toastsUp(dom string) string {
	var out []string
	for _, part := range strings.Split(dom, `class="notice-text">`)[1:] {
		if i := strings.Index(part, "<"); i >= 0 {
			out = append(out, part[:i])
		}
	}
	return "[" + strings.Join(out, " | ") + "]"
}

// toastShotDriver is the page's stand-in stream and the shot's taps, with
// the shot's name at %[1]q. The stream says connected once and then nothing.
// Once the board has drawn (1.5 s of virtual time), the shot's toasts are
// raised through the station's own module instances, which a dynamic import
// from the page reaches, and the page is marked done.
const toastShotDriver = `(function () {
  function Stream(url) { this.url = url; this.l = {}; var s = this;
    setTimeout(function () { s.emit('connected', { build: '' }); }, 50); }
  Stream.prototype.addEventListener = function (t, fn) { (this.l[t] = this.l[t] || []).push(fn); };
  Stream.prototype.removeEventListener = function () {};
  Stream.prototype.close = function () {};
  Stream.prototype.emit = function (t, d) {
    (this.l[t] || []).forEach(function (fn) { fn({ type: t, data: JSON.stringify(d), lastEventId: '' }); });
  };
  window.EventSource = Stream;
  // A toast's fade-in is a CSS animation, which headless Chrome's virtual
  // time does not run: without this the picture catches it at its start.
  var still = document.createElement('style');
  still.textContent = '.notice-toast { animation: none !important; }';
  document.head.appendChild(still);
  var shot = %[1]q;
  setTimeout(async function () {
    var util = await import('/static/operator-station/operator-util.js');
    var notice = await import('/static/shared/notice.js');
    var say = function (m, l) { notice.toast(m, l, util.STATION_TOASTS); };
    if (shot === 'success') say('Bin loaded', 'success');
    if (shot === 'error') await util.postAction('/__shots/refuse', {}, null);
    if (shot === 'three') {
      await util.postAction('/__shots/refuse', {}, null);
      say('Bin loaded', 'success');
      await util.postAction('/__shots/pending', {}, null);
      say('Type the count first: a LOAD needs 1 or more', 'warning');
    }
    if (shot === 'exit') await util.postAction('/__shots/refuse-exit', {}, null);
    document.body.setAttribute('data-shot-done', shot);
  }, 1500);
})();
`
