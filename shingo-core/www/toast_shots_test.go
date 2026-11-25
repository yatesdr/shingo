//go:build shots

package www

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/domain"
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

// TestOrderControlsShots — Core's order pop-up (/orders?open=N) at 1360x900,
// its Controls, for the final brief's word Cancel (Part 3), taken before and
// after the change by the same command: an order the fleet is carrying, whose
// Controls offer the cancel, and an order Core cancelled that the fleet still
// runs, whose Controls offer the cancel's re-send and whose Notes say one was
// re-sent.
//
//	core-order-controls-<tag>-1360.png  the live order's pop-up
//	core-order-prompt-<tag>-1360.png    its cancel pressed: the dialog
//	core-order-resend-<tag>-1360.png    the cancelled order's pop-up
//
// <tag> is ORDER_SHOTS_TAG (before, after; "shot" when unset). THE PAGE IS THE
// REAL ONE: its template, static files and modules, on the config shots' stub
// engine (configShotsEngine), as TestTestOrdersToastShots serves the Test
// Orders page. Its rows are the two synthetic orders. The pop-up's answer
// (/api/orders/enriched) is the harness's, with each order's route and strip,
// its cancel gate and its Notes as the handler computes them (domain.RouteStops,
// domain.RouteViewOf, canCancelStatus, orderNotes); the re-send gate, which the
// handler asks of the fleet, is set. The stream says connected and nothing
// more. Run:
//
//	cd shingo-core && ORDER_SHOTS_OUT=<dir> ORDER_SHOTS_TAG=after go test -tags shots -count=1 \
//	    -run '^TestOrderControlsShots$' -v ./www/
func TestOrderControlsShots(t *testing.T) {
	out := os.Getenv("ORDER_SHOTS_OUT")
	if out == "" {
		t.Skip("ORDER_SHOTS_OUT not set")
	}
	tag := os.Getenv("ORDER_SHOTS_TAG")
	if tag == "" {
		tag = "shot"
	}
	chrome := coreChromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 25, 9, 40, 0, 0, time.UTC)
	live := &domain.Order{ID: 5101, EdgeUUID: "5a1f0c2e-0000-4000-8000-000000005101", StationUID: "syn.line-1",
		OrderType: protocol.OrderTypeRetrieve, Status: protocol.StatusInTransit, Quantity: 1,
		SourceNode: "SYN-MARKET-1", DeliveryNode: "SYN-LINE-01", VendorOrderID: "syn-5101", RobotID: "SYN-AMR-1",
		PayloadCode: "SYN-PART-A", CreatedAt: at, UpdatedAt: at.Add(2 * time.Minute), Version: 4}
	ended := &domain.Order{ID: 5102, EdgeUUID: "9b7d3e61-0000-4000-8000-000000005102", StationUID: "syn.line-2",
		OrderType: protocol.OrderTypeRetrieve, Status: protocol.StatusCancelled, Quantity: 1,
		SourceNode: "SYN-MARKET-1", DeliveryNode: "SYN-LINE-02", VendorOrderID: "syn-5102", RobotID: "SYN-AMR-2",
		PayloadCode: "SYN-PART-A", ErrorDetail: "cancelled by shots", CreatedAt: at, UpdatedAt: at.Add(3 * time.Minute),
		Version: 5}
	popUp := map[int64]map[string]any{}
	for _, o := range []*domain.Order{live, ended} {
		route := domain.RouteStops(o)
		popUp[o.ID] = map[string]any{
			"order": o, "can_cancel": canCancelStatus(o.Status), "can_hard_release": false,
			"can_reissue_terminate": o == ended, "route": route, "route_view": domain.RouteViewOf(route, o.Status),
		}
	}
	popUp[ended.ID]["notes"] = orderNotes([]*domain.AuditEntry{{EntityType: "order", EntityID: ended.ID,
		Action: "recovery.reissue_terminate", NewValue: "shots", Actor: "shots", CreatedAt: at.Add(4 * time.Minute)}})

	srv := testOrdersShotsServer(t, func(h *Handlers, r chi.Router) {
		base := template.Must(template.New("").Funcs(templateFuncs(nil)).ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
		h.tmpls["orders.html"] = template.Must(template.Must(base.Clone()).ParseFS(templateFS, "templates/orders.html"))
		r.Get("/orders", func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			h.render(rec, r, "orders.html", map[string]any{"Page": "orders", "Orders": []*domain.Order{live, ended},
				"OrderProcess": map[int64]string{}, "FilterStatus": "", "Search": "", "PickedIDs": 0,
				"QueueCodeCounts": map[string]int{}, "QueueCodeLabels": queueCodeLabels(), "FaultedNoticeCount": 0,
				"WaitSince": map[int64]string{}})
			page := rec.Body.String()
			if !strings.Contains(page, "</body>") {
				t.Errorf("the Orders page has no </body> to put the driver before:\n%.400s", page)
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write([]byte(strings.Replace(page, "</body>", orderControlsShotDriver+"</body>", 1)))
		})
		r.Get("/api/orders/enriched", func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			if err != nil || popUp[id] == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(popUp[id]); err != nil {
				t.Errorf("encode order %d's pop-up: %v", id, err)
			}
		})
	})

	for _, s := range []struct {
		file string
		id   int64
		shot string
	}{
		{"core-order-controls-" + tag + "-1360.png", live.ID, "controls"},
		{"core-order-prompt-" + tag + "-1360.png", live.ID, "prompt"},
		{"core-order-resend-" + tag + "-1360.png", ended.ID, "controls"},
	} {
		target := filepath.Join(out, s.file)
		_ = os.Remove(target)
		to := fmt.Sprintf("/orders?open=%d&shot=%s", s.id, s.shot)
		u := srv.URL + "/__shots/theme?t=dark&to=" + url.QueryEscape(to)
		testOrdersChrome(t, chrome, 0, u, "--screenshot="+target)
		if info, err := os.Stat(target); err != nil || info.Size() < 10_000 {
			t.Fatalf("%s missing or blank: %v", target, err)
		}
		dom := testOrdersChrome(t, chrome, 0, u, "--dump-dom")
		if !strings.Contains(dom, `data-shot-done="`+s.shot+`"`) {
			t.Errorf("%s: the driver did not finish (no data-shot-done)", s.file)
		}
		t.Logf("wrote %s; controls: %s", target, controlWords(dom))
	}
}

// controlWords is the words on the pop-up's Controls buttons and on its open
// dialog, in a dumped DOM.
func controlWords(dom string) string {
	var out []string
	if i := strings.Index(dom, `class="manifest-controls"`); i >= 0 {
		part := dom[i:]
		if j := strings.Index(part, `order-priority-label`); j >= 0 {
			part = part[:j]
		}
		for _, b := range strings.Split(part, "<button ")[1:] {
			if k, l := strings.Index(b, ">"), strings.Index(b, "</button>"); k >= 0 && l > k {
				out = append(out, b[k+1:l])
			}
		}
	}
	if i := strings.Index(dom, `class="layer-msg">`); i >= 0 {
		msg := dom[i+len(`class="layer-msg">`):]
		if j := strings.Index(msg, "<"); j >= 0 {
			out = append(out, "dialog: "+msg[:j])
		}
	}
	return "[" + strings.Join(out, " | ") + "]"
}

// orderControlsShotDriver is the stand-in stream (connected, then nothing), put
// in the page before its modules run, and the shot: once the pop-up has drawn
// (1.5 s of virtual time), the prompt shot presses its cancel and leaves the
// dialog open; every shot then marks the page done.
const orderControlsShotDriver = `<script>(function () {
  function Stream(url) { this.l = {}; var s = this;
    setTimeout(function () { s.emit('connected', {}); }, 50); }
  Stream.prototype.addEventListener = function (t, fn) { (this.l[t] = this.l[t] || []).push(fn); };
  Stream.prototype.removeEventListener = function () {};
  Stream.prototype.close = function () {};
  Stream.prototype.emit = function (t, d) {
    (this.l[t] || []).forEach(function (fn) { fn({ type: t, data: JSON.stringify(d), lastEventId: '' }); });
  };
  window.EventSource = Stream;
  var shot = new URLSearchParams(location.search).get('shot');
  setTimeout(function () {
    if (shot === 'prompt') {
      var btn = document.querySelector('.manifest-controls [data-action^="cancelOrder:"]');
      if (btn) btn.click();
    }
    setTimeout(function () { document.body.setAttribute('data-shot-done', shot); }, 300);
  }, 1500);
})();</script>`

// testOrdersShotsServer serves the Test Orders page as above, and whatever a
// caller's mount adds beside it on the same handlers and router (a page of its
// own, its answers), before the server starts.
func testOrdersShotsServer(t *testing.T, mount ...func(h *Handlers, r chi.Router)) *httptest.Server {
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
	for _, m := range mount {
		m(h, r)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}
