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
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/orders"
	"shingoedge/store/processes"
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

// TestOrdersCancelShots — the Edge's Orders page (/orders) at 1280x800, for the
// final brief's no cancel on a delivered order (1e) and the word Cancel (its
// Part 3), taken before and after the change by the same command. Two orders
// to synthetic lines: one whose cart is on its way (in_transit) and one whose
// cart is set down (delivered).
//
//	edge-orders-<tag>-1280.png          the two rows and what each offers
//	edge-orders-prompt-<tag>-1280.png   the first row's cancel pressed: its dialog
//	edge-orders-refused-<tag>-1280.png  a page drawn while the first order was
//	                                    on its way, its cancel pressed after the
//	                                    order was delivered, and what it answers
//
// <tag> is ORDERS_SHOTS_TAG (before, after; "shot" when unset). The page and
// its doors are the real ones (the engine's router, its template, orders.js
// and the abort door); two things are the harness's. /__shots/deliver writes
// the first order delivered, as Core's word would, without redrawing the page.
// And the driver, put after the page's last tag, presses the row's cancel and
// the dialog's OK once the page has drawn, then marks the page done.
//
// Run:
//
//	cd shingo-edge && ORDERS_SHOTS_OUT=<dir> ORDERS_SHOTS_TAG=after go test -tags shots \
//	    -count=1 -run '^TestOrdersCancelShots$' -v ./www/
func TestOrdersCancelShots(t *testing.T) {
	t.Parallel()
	out := os.Getenv("ORDERS_SHOTS_OUT")
	if out == "" {
		t.Skip("ORDERS_SHOTS_OUT not set")
	}
	tag := os.Getenv("ORDERS_SHOTS_TAG")
	if tag == "" {
		tag = "shot"
	}
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}

	db := testdb.Open(t)
	procID, err := db.CreateProcess("SYN-PROC-1", "synthetic process", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	order := func(uuid, line string, seq int, status protocol.Status) int64 {
		nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: line, Code: line,
			Name: line, Sequence: seq, Enabled: true})
		if err != nil {
			t.Fatalf("create node %s: %v", line, err)
		}
		id, err := db.CreateOrder(uuid, orders.TypeRetrieve, &nodeID, false, 1, line, "", "SYN-MARKET-1",
			protocol.Height{}, false, "SYN-PART-A", "", "")
		if err != nil {
			t.Fatalf("create order to %s: %v", line, err)
		}
		if err := db.UpdateOrderStatus(id, string(status)); err != nil {
			t.Fatalf("order to %s %s: %v", line, status, err)
		}
		return id
	}
	moving := order("5a1f0c2e-0000-4000-8000-000000000001", "SYN-LINE-01", 1, protocol.StatusInTransit)
	order("9b7d3e61-0000-4000-8000-000000000002", "SYN-LINE-02", 2, protocol.StatusDelivered)

	cfg := config.Defaults()
	cfg.StationUID = "shots.orders"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)
	router := shotsRouter(t, eng)

	srv := shotsServer(t, router, func(mux *http.ServeMux) {
		mux.HandleFunc("/__shots/orders-driver.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			fmt.Fprintf(w, ordersShotDriver, r.URL.Query().Get("shot"), strconv.FormatInt(moving, 10))
		})
		mux.HandleFunc("/__shots/deliver", func(w http.ResponseWriter, _ *http.Request) {
			if err := db.UpdateOrderStatus(moving, string(protocol.StatusDelivered)); err != nil {
				t.Errorf("deliver order %d: %v", moving, err)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		// The Orders page with the driver after its last tag. Each load starts
		// with the first order on its way again: every shot runs the page twice
		// (the picture, then the DOM), and the refused shot delivers it.
		mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
			if err := db.UpdateOrderStatus(moving, string(protocol.StatusInTransit)); err != nil {
				t.Errorf("order %d back on its way: %v", moving, err)
			}
			plain := r.Clone(r.Context())
			plain.Header.Del("Accept-Encoding")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, plain)
			page := rec.Body.String()
			if !strings.Contains(page, "</body>") {
				t.Errorf("the Orders page has no </body> to put the driver before:\n%.400s", page)
			}
			page = strings.Replace(page, "</body>",
				`<script src="/__shots/orders-driver.js?shot=`+r.URL.Query().Get("shot")+`"></script></body>`, 1)
			for k, v := range rec.Header() {
				if k != "Content-Length" && k != "Etag" {
					w.Header()[k] = v
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write([]byte(page))
		})
	})

	for _, s := range []struct{ shot, file string }{
		{"rows", "edge-orders-" + tag + "-1280.png"},
		{"prompt", "edge-orders-prompt-" + tag + "-1280.png"},
		{"refused", "edge-orders-refused-" + tag + "-1280.png"},
	} {
		u := fmt.Sprintf("%s/orders?shot=%s", srv.URL, s.shot)
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
		t.Logf("wrote %s (%d bytes); rows offer: %s; toasts up: %s", target, info.Size(), rowActions(dom), toastsUp(dom))
	}
}

// rowActions is each Orders row's buttons in a dumped DOM: the row's order id
// and the words on its buttons.
func rowActions(dom string) string {
	var out []string
	for _, part := range strings.Split(dom, `<tr data-order-id="`)[1:] {
		row := part
		if i := strings.Index(row, "</tr>"); i >= 0 {
			row = row[:i]
		}
		id := row[:strings.Index(row, `"`)]
		var words []string
		for _, b := range strings.Split(row, "<button ")[1:] {
			if i, j := strings.Index(b, ">"), strings.Index(b, "</button>"); i >= 0 && j > i {
				words = append(words, strings.TrimSpace(b[i+1:j]))
			}
		}
		out = append(out, id+": "+strings.Join(words, ", "))
	}
	return "[" + strings.Join(out, " | ") + "]"
}

// shotStream is the stand-in event stream a driver puts in a page before its
// modules run: it says connected once and then nothing. An open EventSource
// keeps headless Chrome's virtual time from ever running out, and a closed one
// puts the lost-connection banner up. It holds no %: the drivers it starts are
// format strings.
const shotStream = `(function () {
  function Stream(url) { this.url = url; this.l = {}; var s = this;
    setTimeout(function () { s.emit('connected', { build: '' }); }, 50); }
  Stream.prototype.addEventListener = function (t, fn) { (this.l[t] = this.l[t] || []).push(fn); };
  Stream.prototype.removeEventListener = function () {};
  Stream.prototype.close = function () {};
  Stream.prototype.emit = function (t, d) {
    (this.l[t] || []).forEach(function (fn) { fn({ type: t, data: JSON.stringify(d), lastEventId: '' }); });
  };
  window.EventSource = Stream;
})();
`

// ordersShotDriver is the Orders page's shot (after the stand-in stream), with
// the shot's name at %[1]q and the first order's id at %[2]q. Each waits for the
// page's modules (1.5 s of virtual time). The rows shot only marks the page
// done; the prompt shot presses the first row's cancel and leaves its dialog
// open; the refused shot has the harness deliver the first order, presses that
// row's cancel and then the dialog's OK, and marks the page done once the
// answer is in.
const ordersShotDriver = shotStream + `(function () {
  var still = document.createElement('style');
  still.textContent = '.notice-toast { animation: none !important; }';
  document.head.appendChild(still);
  var shot = %[1]q, id = %[2]q;
  var done = function () { document.body.setAttribute('data-shot-done', shot); };
  if (shot === 'rows') { setTimeout(done, 1500); return; }
  setTimeout(async function () {
    if (shot === 'refused') await fetch('/__shots/deliver', { method: 'POST' });
    var btn = document.querySelector('[data-action="abortOrder:' + id + '"]');
    if (!btn) { document.body.setAttribute('data-shot-done', 'no-cancel-button'); return; }
    btn.click();
    setTimeout(function () {
      if (shot === 'prompt') { done(); return; }
      var ok = document.querySelector('[data-layer-ok]');
      if (ok) ok.click();
      setTimeout(done, 1500);
    }, 300);
  }, 1500);
})();
`

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
const toastShotDriver = shotStream + `(function () {
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
