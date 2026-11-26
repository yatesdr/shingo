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
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/domain"
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

// TestStationTileCancelShots — the station's tile for a position whose inbound
// order has this station's cancel out and unanswered, at the kiosk's 1280x800
// (brief G D, DOCKET R146c): its pill should read "Cancel sent"
// (operator-render.js appendETAPills). The page is the real one on the seeded
// plant (scenefixtures A, Press A1), as TestStationToastShots draws it, with
// the same stand-in event stream. Its first position gets an order on its way
// that Core has told at a version, and the one cancel door asks Core for it;
// no Core answers, so the cancel stays sent.
//
//	edge-tile-cancel-sent-after-1280.png
//
// Run:
//
//	cd shingo-edge && TOAST_SHOTS_OUT=<dir> go test -tags shots -count=1 \
//	    -run '^TestStationTileCancelShots$' -v ./www/
func TestStationTileCancelShots(t *testing.T) {
	t.Parallel()
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
	nodes, err := db.ListProcessNodesByStation(station)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("the station's positions: %v (%d)", err, len(nodes))
	}
	pos := nodes[0]
	cfg := config.Defaults()
	cfg.StationUID = "shots.tile"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)

	id, err := db.CreateOrder("5a1f0c2e-0000-4000-8000-000000000021", orders.TypeRetrieve, &pos.ID, false, 1,
		pos.CoreNodeName, "", "SYN-MARKET-1", protocol.Height{}, false, "SYN-PART-A", "", "")
	if err != nil {
		t.Fatalf("create the order: %v", err)
	}
	if _, err := db.DB.Exec(`UPDATE orders SET status = ?, core_version = 1 WHERE id = ?`,
		string(protocol.StatusInTransit), id); err != nil {
		t.Fatalf("the order on its way, at Core's word: %v", err)
	}
	if err := eng.OrderManager().AskCancel(id, orders.CancelAsk{}); err != nil {
		t.Fatalf("ask Core for the cancel: %v", err)
	}
	if o, err := db.GetOrder(id); err != nil || !o.CancelSent {
		t.Fatalf("the order's cancel is not out: %v", err)
	}

	router := shotsRouter(t, eng)
	srv := shotsServer(t, router, func(mux *http.ServeMux) {
		mux.HandleFunc("/__shots/tile-driver.js", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = w.Write([]byte(tileShotDriver))
		})
		mux.HandleFunc("/operator/station/", func(w http.ResponseWriter, r *http.Request) {
			plain := r.Clone(r.Context())
			plain.Header.Del("Accept-Encoding")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, plain)
			page := rec.Body.String()
			if !strings.Contains(page, "</body>") {
				t.Errorf("the station page has no </body> to put the driver before:\n%.400s", page)
			}
			page = strings.Replace(page, "</body>", `<script src="/__shots/tile-driver.js"></script></body>`, 1)
			for k, v := range rec.Header() {
				if k != "Content-Length" && k != "Etag" {
					w.Header()[k] = v
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write([]byte(page))
		})
	})

	u := fmt.Sprintf("%s/operator/station/%d", srv.URL, station)
	target := filepath.Join(out, "edge-tile-cancel-sent-after-1280.png")
	_ = os.Remove(target)
	shotChrome(t, chrome, out, u, "--screenshot="+target)
	info, err := os.Stat(target)
	if err != nil || info.Size() < 10_000 {
		t.Fatalf("%s missing or blank: %v", target, err)
	}
	dom := shotChrome(t, chrome, out, u, "--dump-dom")
	if !strings.Contains(dom, `data-shot-done="tile"`) {
		t.Errorf("the driver did not finish (no data-shot-done)")
	}
	if !strings.Contains(dom, `os-node-eta-pill">Cancel sent<`) {
		t.Errorf("%s's tile has no \"Cancel sent\" pill", pos.CoreNodeName)
	}
	t.Logf("wrote %s (%d bytes); %s's tile pills: %d \"Cancel sent\"", target, info.Size(), pos.CoreNodeName,
		strings.Count(dom, `os-node-eta-pill">Cancel sent<`))
}

// tileShotDriver is TestStationTileCancelShots's: the stand-in stream, then
// the page marked done once the board has drawn (1.5 s of virtual time).
const tileShotDriver = shotStream + `(function () {
  setTimeout(function () { document.body.setAttribute('data-shot-done', 'tile'); }, 1500);
})();
`

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

// TestCancelAskShots — the Edge's Orders page (/orders) at 1280x800 for the
// final brief's Part 5 (a cancel asks Core), taken before and after by the
// same command. No Core answers here: Core is unreachable, unless the harness
// answers as Core would. "before" leaves the orders with no versioned word of
// Core's, so the one door takes its old body, the one every cancel took until
// 2026-11-25 (the row ended at the tap); "after" gives them Core's word at a
// version, so the cancel asks.
//
//	edge-cancel-sent-<tag>-1280.png        the in-transit row's Cancel pressed
//	                                       and OK'd, Core silent: the tap's
//	                                       answer and the row
//	edge-cancel-refused-after-1280.png     the same, Core answering that the
//	                                       cart is set down (its delivered, then
//	                                       its refusal): the tap's answer and
//	                                       the row with Core's sentence
//	edge-cancel-changeover-<tag>-1280.png  the page after a changeover's CANCEL
//	                                       (the station's route), Core
//	                                       unreachable: what its leg reads
//
// Synthetic names only. Run:
//
//	cd shingo-edge && ORDERS_SHOTS_OUT=<dir> ORDERS_SHOTS_TAG=after go test -tags shots \
//	    -count=1 -run '^TestCancelAskShots$' -v ./www/
func TestCancelAskShots(t *testing.T) {
	t.Parallel()
	out := os.Getenv("ORDERS_SHOTS_OUT")
	if out == "" {
		t.Skip("ORDERS_SHOTS_OUT not set")
	}
	tag := os.Getenv("ORDERS_SHOTS_TAG")
	if tag == "" {
		tag = "shot"
	}
	asks := tag != "before"
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}

	db := testdb.Open(t)
	version := func(id int64) {
		v := 0
		if asks {
			v = 1
		}
		if _, err := db.DB.Exec(`UPDATE orders SET core_version = ? WHERE id = ?`, v, id); err != nil {
			t.Fatalf("Core's word on order %d: %v", id, err)
		}
	}
	procID, err := db.CreateProcess("SYN-PROC-1", "synthetic process", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: "SYN-LINE-01",
		Code: "SYN-LINE-01", Name: "SYN-LINE-01", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	moving, err := db.CreateOrder("5a1f0c2e-0000-4000-8000-000000000011", orders.TypeRetrieve, &nodeID, false, 1,
		"SYN-LINE-01", "", "SYN-MARKET-1", protocol.Height{}, false, "SYN-PART-A", "", "")
	if err != nil {
		t.Fatalf("create the order: %v", err)
	}

	// The changeover: a line on its own process, two styles, a single-robot
	// claim each; START makes its leg, and the station's CANCEL ends it below.
	coProc, err := db.CreateProcess("SYN-PROC-2", "synthetic changeover", "", "", false)
	if err != nil {
		t.Fatalf("create the changeover's process: %v", err)
	}
	coNode, err := db.CreateProcessNode(processes.NodeInput{ProcessID: coProc, CoreNodeName: "SYN-LINE-02",
		Code: "SYN-LINE-02", Name: "SYN-LINE-02", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create the changeover's node: %v", err)
	}
	var styles []int64
	for _, s := range []struct{ name, part string }{{"SYN-STYLE-A", "SYN-PART-A"}, {"SYN-STYLE-B", "SYN-PART-B"}} {
		sid, err := db.CreateStyle(s.name, "", coProc)
		if err != nil {
			t.Fatalf("create style %s: %v", s.name, err)
		}
		if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: "SYN-LINE-02", Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSingleRobot, PayloadCode: s.part, UOPCapacity: 40,
			InboundSource: "SYN-MARKET-1", OutboundDestination: "SYN-MARKET-1",
			InboundStaging: "SYN-STAGE-01", OutboundStaging: "SYN-STAGE-02",
		}); err != nil {
			t.Fatalf("claim for %s: %v", s.name, err)
		}
		styles = append(styles, sid)
	}
	if err := db.SetActiveStyle(coProc, &styles[0]); err != nil {
		t.Fatalf("active style: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(coNode); err != nil {
		t.Fatalf("runtime: %v", err)
	}

	cfg := config.Defaults()
	cfg.StationUID = "shots.cancel"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)
	router := shotsRouter(t, eng)
	const refusal = "Not cancelled: its cart was set down at SYN-LINE-01 on Nov 25 at 12:00 UTC. Confirm it at the station."
	var refusing atomic.Bool
	// answerAsCore waits for the tap's cancel to be on the row, then answers
	// as Core does for a cart set down: its delivered tell, then its refusal.
	answerAsCore := func() {
		for deadline := time.Now().Add(engine.AskWait); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			o, err := db.GetOrder(moving)
			if err != nil || !o.CancelSent {
				continue
			}
			in, err := domain.DecodeCancelIntent(o.CancelIntent)
			if err != nil || in == nil {
				continue
			}
			mgr := eng.OrderManager()
			if err := mgr.ApplyCoreWord(o.UUID, orders.CoreWord{Status: protocol.StatusDelivered, Version: 2}); err != nil {
				t.Errorf("Core's delivered: %v", err)
			}
			mgr.RefuseCancel(in.Env, o.UUID, refusal)
			return
		}
		t.Errorf("the tap's cancel never reached the row")
	}

	srv := shotsServer(t, router, func(mux *http.ServeMux) {
		mux.HandleFunc("/__shots/cancel-driver.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			fmt.Fprintf(w, cancelShotDriver, r.URL.Query().Get("shot"), strconv.FormatInt(moving, 10))
		})
		// Core's answer to the cancel, on the refused shot, while the tap waits:
		// the cart set down (its delivered tell), then its refusal of the
		// cancel's envelope, as Core sends them. The page's timers do not run
		// while the tap's request is out, so the harness answers from here.
		mux.HandleFunc("/api/orders/", func(w http.ResponseWriter, r *http.Request) {
			if refusing.Load() && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
				go answerAsCore()
			}
			router.ServeHTTP(w, r)
		})
		// The Orders page with the driver after its last tag. Each load starts
		// with the first order on its way, its cancel unasked: every shot runs
		// the page twice (the picture, then the DOM).
		mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
			if _, err := db.DB.Exec(`UPDATE orders SET status = ?, cancel_intent = '' WHERE id = ?`,
				string(protocol.StatusInTransit), moving); err != nil {
				t.Errorf("order %d back on its way: %v", moving, err)
			}
			version(moving)
			refusing.Store(r.URL.Query().Get("shot") == "refused")
			plain := r.Clone(r.Context())
			plain.Header.Del("Accept-Encoding")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, plain)
			page := rec.Body.String()
			if !strings.Contains(page, "</body>") {
				t.Errorf("the Orders page has no </body> to put the driver before:\n%.400s", page)
			}
			page = strings.Replace(page, "</body>",
				`<script src="/__shots/cancel-driver.js?shot=`+r.URL.Query().Get("shot")+`"></script></body>`, 1)
			for k, v := range rec.Header() {
				if k != "Content-Length" && k != "Etag" {
					w.Header()[k] = v
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write([]byte(page))
		})
	})

	shoot := func(shot, file string) {
		u := fmt.Sprintf("%s/orders?shot=%s", srv.URL, shot)
		target := filepath.Join(out, file)
		_ = os.Remove(target)
		shotChrome(t, chrome, out, u, "--screenshot="+target)
		info, err := os.Stat(target)
		if err != nil || info.Size() < 10_000 {
			t.Fatalf("%s missing or blank: %v", target, err)
		}
		dom := shotChrome(t, chrome, out, u, "--dump-dom")
		if !strings.Contains(dom, `data-shot-done="`+shot+`"`) {
			t.Errorf("%s: the driver did not finish (no data-shot-done)", shot)
		}
		// Core's refusal is an answer, not an error (brief G D, R146b): its
		// toast is Core's sentence alone.
		if up := toastsUp(dom); shot == "refused" && (!strings.Contains(up, refusal) || strings.Contains(up, "Error:")) {
			t.Errorf("refused: the toasts read %s, want Core's sentence with no \"Error:\"", up)
		}
		t.Logf("wrote %s (%d bytes); rows offer: %s; toasts up: %s; cancel sent rows: %d", target, info.Size(),
			rowActions(dom), toastsUp(dom), strings.Count(dom, "Cancel sent"))
	}
	shoot("sent", "edge-cancel-sent-"+tag+"-1280.png")
	if asks {
		shoot("refused", "edge-cancel-refused-"+tag+"-1280.png")
	}

	// The changeover, started and then cancelled at the station's route.
	if _, err := eng.StartProcessChangeover(coProc, styles[1], "shots", ""); err != nil {
		t.Fatalf("START: %v", err)
	}
	legs, err := db.ListActiveOrdersByProcess(coProc)
	if err != nil || len(legs) == 0 {
		t.Fatalf("the changeover's legs: %v (%d)", err, len(legs))
	}
	for _, l := range legs {
		version(l.ID)
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/processes/%d/changeover/cancel", srv.URL, coProc), "application/json",
		strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("the station's CANCEL: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the station's CANCEL answered %d", resp.StatusCode)
	}
	for _, l := range legs {
		o, err := db.GetOrder(l.ID)
		if err != nil {
			t.Fatalf("read leg %d: %v", l.ID, err)
		}
		t.Logf("after the CANCEL, leg %d reads %s, cancel sent %v", o.ID, o.Status, o.CancelSent)
	}
	shoot("changeover", "edge-cancel-changeover-"+tag+"-1280.png")
}

// cancelShotDriver is TestCancelAskShots's page driver (after the stand-in
// stream), with the shot's name at %[1]q and the first order's id at %[2]q.
// It waits for the page's modules (1.5 s of virtual time); the changeover shot
// then only marks the page done. The others press the row's Cancel and the
// dialog's OK (on the refused shot the harness answers as Core while the tap
// waits); each marks the page done once the answer and the redraw are in.
const cancelShotDriver = shotStream + `(function () {
  var still = document.createElement('style');
  still.textContent = '.notice-toast { animation: none !important; }';
  document.head.appendChild(still);
  var shot = %[1]q, id = %[2]q;
  var done = function () { document.body.setAttribute('data-shot-done', shot); };
  if (shot === 'changeover') { setTimeout(done, 1500); return; }
  setTimeout(function () {
    var btn = document.querySelector('[data-action="cancelOrder:' + id + '"]');
    if (!btn) { document.body.setAttribute('data-shot-done', 'no-cancel-button'); return; }
    btn.click();
    setTimeout(function () {
      var ok = document.querySelector('[data-layer-ok]');
      if (ok) ok.click();
      setTimeout(done, 3500);
    }, 300);
  }, 1500);
})();
`

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
    var btn = document.querySelector('[data-action="cancelOrder:' + id + '"]');
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

// TestLoaderHomeStagedShots — the loader board's card for a dedicated loader's
// home whose cart a line's robot is staged under (brief H H3), at the kiosk's
// 1280x800, and what its CLEAR BIN answers there; then the same with the robot
// on its way (brief I I1).
//
//	h3-loader-home-card-staged-1280.png    the home's card: LOADED (40 UoP), No
//	                                       active orders, CLEAR BIN; nothing on
//	                                       it says a robot stands under the cart
//	h3-loader-home-clear-refused-1280.png  CLEAR BIN pressed: Core's refusal, as
//	                                       the Edge's door answers it
//	i1-loader-home-clear-refused-on-its-way-1280.png
//	                                       the supply in transit, CLEAR BIN
//	                                       pressed: Core's on-its-way refusal
//
// The page, the view and the card are the real ones: the engine's router, the
// loader's screen made by its own door (CreateLoaderBoard) from Core's node
// list, Core's bins copy with the cart on the home, the Edge's count for the
// home at 40 (what a LOAD seats), and the line's supply staged at the home on
// another process (its row is the line's, born with no source, as the Edge's
// complex orders are). One thing is the harness's: no Core answers here, so
// the CLEAR BIN's door is answered as the Edge's door answers when Core
// refuses; TestScenario_TwoRobotLine_ThroughItsHome_LoaderCardWhileStaged
// (integration/scenarios) holds the staged answer across the two modules, and
// Core's TestBinClearDoor_Arms both sentences. The stand-in event stream is
// TestStationToastShots's. Synthetic names only.
//
// Run:
//
//	cd shingo-edge && TOAST_SHOTS_OUT=<dir> go test -tags shots -count=1 \
//	    -run '^TestLoaderHomeStagedShots$' -v ./www/
func TestLoaderHomeStagedShots(t *testing.T) {
	t.Parallel()
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
	const home, buffer, line, cart, part = "SYN-HOME-01", "SYN-BUF-01", "SYN-LINE-01", "SYN-CART-01", "SYN-PART-A"

	db := testdb.Open(t)
	cfg := config.Defaults()
	cfg.StationUID = "shots.loader"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)
	eng.ApplyNodeList(&protocol.NodeListResponse{
		Nodes: []protocol.NodeInfo{{Name: line}, {Name: home}, {Name: buffer}},
		Loaders: []protocol.LoaderInfo{{Name: "SYN Loader", LoaderKey: "loader:1", Role: "produce",
			Layout: protocol.LoaderLayoutDedicatedPositions, Replenishment: protocol.LoaderReplenishmentOperator,
			Positions: []protocol.LoaderPosition{
				{CoreNodeName: home, PayloadCode: part, HomeKind: protocol.LoaderHomeKindHome, Ordinal: 1},
				{CoreNodeName: buffer, HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 2},
			}}},
	})
	loadProc, err := db.CreateProcess("SYN-LOAD-OPS", "the loader's screen", "", "", false)
	if err != nil {
		t.Fatalf("the loader's process: %v", err)
	}
	station, err := eng.StationService().CreateLoaderBoard("loader:1", loadProc)
	if err != nil {
		t.Fatalf("the loader's screen: %v", err)
	}
	windows, err := db.ListProcessNodesByStation(station)
	if err != nil {
		t.Fatalf("the screen's windows: %v", err)
	}
	var homeID int64
	for _, n := range windows {
		if n.CoreNodeName == home {
			homeID = n.ID
		}
	}
	if homeID == 0 {
		t.Fatalf("the loader's screen has no window at %s (%d windows)", home, len(windows))
	}
	if _, err := db.EnsureProcessNodeRuntime(homeID); err != nil {
		t.Fatalf("the home's runtime: %v", err)
	}
	if err := db.SetProcessNodeRuntime(homeID, nil, 40); err != nil {
		t.Fatalf("the home's count after its LOAD: %v", err)
	}
	lineProc, err := db.CreateProcess("SYN-LINE", "the line", "", "", false)
	if err != nil {
		t.Fatalf("the line's process: %v", err)
	}
	lineNode, err := db.CreateProcessNode(processes.NodeInput{ProcessID: lineProc, CoreNodeName: line, Code: line,
		Name: line, Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("the line's position: %v", err)
	}
	supply, err := db.CreateOrder("5a1f0c2e-0000-4000-8000-000000000031", orders.TypeComplex, &lineNode, false, 1, line,
		"", "", protocol.Height{}, false, part, "", "")
	if err != nil {
		t.Fatalf("the line's supply: %v", err)
	}
	if err := db.UpdateOrderStatus(supply, string(protocol.StatusStaged)); err != nil {
		t.Fatalf("the supply staged: %v", err)
	}
	var rows []protocol.NodeBinsRow
	for _, n := range eng.StationService().WatchedBinNodes() {
		row := protocol.NodeBinsRow{Node: n, State: protocol.BinsStateEmpty}
		if n == home {
			row = protocol.NodeBinsRow{Node: n, State: protocol.BinsStateOccupied, Occupied: true, BinID: 1,
				BinLabel: cart, BinTypeCode: "SYN-CARRIER", PayloadCode: part, ManifestConfirmed: true, Stocked: true,
				UOPCapacity: 40}
		}
		rows = append(rows, row)
	}
	snap := protocol.NodeBinsSnapshot{Rows: rows, Groups: []protocol.NodeBinsGroup{}}
	if snap.Digest, err = protocol.NodeBinsDigest(snap); err != nil {
		t.Fatalf("the bins copy's digest: %v", err)
	}
	if err := eng.ApplyNodeBinsSnapshot(snap); err != nil {
		t.Fatalf("Core's bins copy: %v", err)
	}

	const staged = "clear bin: Not cleared: a robot is staged at " + home + " to take " + cart + " to " + line + "."
	const onItsWay = "clear bin: Not cleared: a robot is on its way to take " + cart + " to " + line + "."
	var refusal atomic.Value // the shot's answer, read by the stand-in door below
	refusal.Store(staged)
	router := shotsRouter(t, eng)
	srv := shotsServer(t, router, func(mux *http.ServeMux) {
		mux.HandleFunc("/__shots/loader-driver.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			fmt.Fprintf(w, loaderShotDriver, r.URL.Query().Get("shot"), homeID)
		})
		mux.HandleFunc(fmt.Sprintf("/api/process-nodes/%d/clear-bin", homeID), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "{\"error\":%q}\n", refusal.Load().(string))
		})
		mux.HandleFunc("/operator/station/", func(w http.ResponseWriter, r *http.Request) {
			plain := r.Clone(r.Context())
			plain.Header.Del("Accept-Encoding")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, plain)
			page := rec.Body.String()
			if !strings.Contains(page, "</body>") {
				t.Errorf("the station page has no </body> to put the driver before:\n%.400s", page)
			}
			page = strings.Replace(page, "</body>",
				`<script src="/__shots/loader-driver.js?shot=`+r.URL.Query().Get("shot")+`"></script></body>`, 1)
			for k, v := range rec.Header() {
				if k != "Content-Length" && k != "Etag" {
					w.Header()[k] = v
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write([]byte(page))
		})
	})

	for _, s := range []struct {
		shot, file string
		supply     protocol.Status
		refusal    string
	}{
		{"card", "h3-loader-home-card-staged-1280.png", protocol.StatusStaged, staged},
		{"refused", "h3-loader-home-clear-refused-1280.png", protocol.StatusStaged, staged},
		{"refused", "i1-loader-home-clear-refused-on-its-way-1280.png", protocol.StatusInTransit, onItsWay},
	} {
		if err := db.UpdateOrderStatus(supply, string(s.supply)); err != nil {
			t.Fatalf("the supply %s: %v", s.supply, err)
		}
		refusal.Store(s.refusal)
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
		for _, want := range []string{"LOADED (40 UoP)", "No active orders", ">CLEAR BIN<"} {
			if !strings.Contains(dom, want) {
				t.Errorf("%s: the card does not say %q", s.shot, want)
			}
		}
		if up := toastsUp(dom); s.shot == "refused" && !strings.Contains(up, s.refusal) {
			t.Errorf("refused: the toasts read %s, want the door's refusal", up)
		}
		t.Logf("wrote %s (%d bytes); toasts up: %s", target, info.Size(), toastsUp(dom))
	}
}

// loaderShotDriver is TestLoaderHomeStagedShots's page driver (after the
// stand-in stream), with the shot's name at %[1]q and the home's process node
// at %[2]d. Once the board has drawn (1.5 s of virtual time) it opens the
// home's card through the board's own module; the refused shot then presses
// the card's CLEAR BIN and waits for the answer's toast.
const loaderShotDriver = shotStream + `(function () {
  var still = document.createElement('style');
  still.textContent = '.notice-toast { animation: none !important; }';
  document.head.appendChild(still);
  var shot = %[1]q, node = %[2]d;
  var done = function () { document.body.setAttribute('data-shot-done', shot); };
  setTimeout(async function () {
    var modal = await import('/static/operator-station/operator-modal.js');
    modal.openModal(node);
    setTimeout(function () {
      if (shot === 'card') { done(); return; }
      var btn = document.querySelector('[data-action="/api/process-nodes/' + node + '/clear-bin"]');
      if (!btn) { document.body.setAttribute('data-shot-done', 'no-clear-button'); return; }
      btn.click();
      setTimeout(done, 1500);
    }, 500);
  }, 1500);
})();
`
