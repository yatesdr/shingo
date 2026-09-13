//go:build shots

package www

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/domain/flowspec"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/service"
)

// composer_shots_test.go — the handoff gate for the cell picture, not CI.
//
// A dev edge seeded from synthetic plant A — the real engine, the real
// router, the real templates — served on a loopback port, and headless
// Chrome asked to render the station page with the flow panel open at
// 1280x800. One PNG per state, named like the reference screenshots so the
// orchestrator can diff them by eye:
//
//	u4-press-index.png      style 7  (two index pairs)
//	u4-two-robot-swap.png   style 11 (two staging moves)
//	u4-schematic.png        style 7, no scene cached
//
// U4's three keep the u4- prefix and NOT the reference's numbers. They are the
// read-only picture — "running" in the header, a Board button, no strip and no
// bar — and while they carried 04/05/06 the composer's own S4 was never
// photographed at all: the folder looked complete and four of its names showed a
// different screen than the one the reference puts under them.
//
// U8 adds the composer's own states, reached through the page's hash entry
// (#compose=<styleId>;state=<S2…S10>[;node=…]) rather than by driving the DOM:
// the hash sets LOCAL UI STATE ONLY and never writes, so a shot cannot leave a
// changeover behind it.
//
//	02-picker.png                    S2, the picker sheet
//	03-setup-card.png                S3, set-up card (was 03-setup-card-run-as-is.png:
//	                                 F6 replaced the button the name described)
//	04-composer-press-index.png      S4, the composer's picture (press-index)
//	05-composer-two-robot-swap.png   S4, two-robot
//	06-position-panel.png            S5, PLN_01's panel over the picture
//	07-dock-panel.png                S6, the IN half
//	08-new-part-blank.png            S7, every card dashed
//	09-finding-on-node-and-bar.png   S8, a position with no choreography
//	10-confirm-rows-and-orders.png   S9, the confirm sheet
//	11-started-auto-return.png       S10, the started screen
//
// Behind the `shots` tag because it needs a Chrome binary; run it through
// scripts/composer-shots.sh, which sets the two environment variables:
//
//	COMPOSER_SHOTS_OUT     directory for the PNGs (created)
//	COMPOSER_SHOTS_CHROME  the Chrome/Chromium binary (defaults per OS)
func TestComposerShots(t *testing.T) {
	out := os.Getenv("COMPOSER_SHOTS_OUT")
	if out == "" {
		t.Skip("COMPOSER_SHOTS_OUT not set; run scripts/composer-shots.sh")
	}
	chrome := chromeBinary()
	if chrome == "" {
		t.Fatal("no Chrome binary: set COMPOSER_SHOTS_CHROME")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}

	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	stationID := seeded.Stations["screen-a4"]

	cfg := config.Defaults()
	// CORE HAS TO LOOK REACHABLE. refusePressIndexWhenCoreUnavailable keys on
	// CoreClient.Available(), which is "CoreAPI is not empty", and with it empty
	// every press-index changeover is refused by name — "cannot determine bin
	// types" — so the preview answers 400 and the bar shows a refusal where the
	// reference shows "Preview OK · N orders". Nothing here calls Core: the
	// preview reads bin types from SetPayloadBinTypes below and its preflight
	// stays "unchecked".
	cfg.CoreAPI = "http://core.invalid"
	cfg.StationUID = "shots.press400"
	cfg.Namespace = "shots"
	cfg.LineID = "press400"
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)

	// What the node-list sync would have delivered: the group membership
	// (as "Group.Child"), the payload→bin-type catalog, and the scene.
	var nodes []protocol.NodeInfo
	for _, n := range fx.Nodes {
		name := n.Name
		if n.ParentName != nil && *n.ParentName != "" {
			name = *n.ParentName + "." + n.Name
		}
		typ := ""
		if n.NodeTypeCode != nil {
			typ = *n.NodeTypeCode
		}
		nodes = append(nodes, protocol.NodeInfo{Name: name, NodeType: typ})
	}
	eng.SetCoreNodes(nodes)
	var pbt []protocol.PayloadBinTypeInfo
	for code, types := range fx.PayloadBinTypeCodes() {
		for _, bt := range types {
			pbt = append(pbt, protocol.PayloadBinTypeInfo{PayloadCode: code, BinTypeCode: bt})
		}
	}
	eng.SetPayloadBinTypes(pbt)
	points, edges := wireScene(fx)

	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	// /events answers 204 here: an EventSource that is left open is a pending
	// network request, and headless Chrome's virtual-time budget waits on
	// pending requests forever, so the screenshot never fires. A 204 closes
	// the EventSource without a retry; the picture is drawn from the view
	// fetch and never needed the stream.
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	// ── ONE REAL CLICK ──────────────────────────────────────────────────
	//
	// Everything else in this file renders. A screenshot never clicks, which
	// is exactly how window.DesktopBodies could be undefined for a whole
	// release — two classic scripts declaring one top-level name, the second
	// dead before its last line — while every shot came out looking right and
	// every write button on the page threw (2026-09-13).
	//
	// Chrome's --screenshot and --dump-dom cannot drive a mouse, and a
	// ?click= door in the page would be a state only the harness can reach,
	// which this file's own rule forbids. So the driver is a page of its own,
	// served by the HARNESS and not by the product: it puts the real page in a
	// same-origin iframe, sets a real control, dispatches a real click on the
	// real button, and writes what happened into its own DOM for --dump-dom to
	// read. The product is not touched and the button is the button.
	mux.HandleFunc("/__shots/click-d5", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(clickD5Driver(r.URL.Query().Get("process"))))
	})
	// The apply modal's pickers, answered by clicking. See clickApplyDriver.
	mux.HandleFunc("/__shots/click-apply", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		preset, _ := strconv.ParseInt(r.URL.Query().Get("preset"), 10, 64)
		style, _ := strconv.ParseInt(r.URL.Query().Get("style"), 10, 64)
		_, _ = w.Write([]byte(clickApplyDriver(r.URL.Query().Get("process"), preset, style)))
	})
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
	shotAt := func(file, hash string) {
		t.Helper()
		target := filepath.Join(out, file)
		_ = os.Remove(target)
		profile := t.TempDir()
		args := []string{
			"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir=" + profile, "--window-size=1280,800", "--virtual-time-budget=15000",
			// The BROWSER process's log, not the page's. --headless=new does not
			// put the renderer's console on stderr: a whole run yields ~300 bytes
			// here, all of it chrome_browser_cloud_management_controller.cc. Kept
			// because a renderer crash still shows up, but the page's own errors
			// and the panel's geometry are read out of the DOM instead — see
			// checkPanelFits.
			"--enable-logging=stderr", "--log-level=0",
			"--screenshot=" + target,
			fmt.Sprintf("%s/operator/station/%d%s", srv.URL, stationID, hash),
		}
		cmd := exec.Command(chrome, args...)
		cmd.Dir = out
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("chrome %s: %v\n%s", file, err, raw)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("%s was not written: %v\n%s", target, err, raw)
		}
		if info.Size() < 10_000 {
			t.Fatalf("%s is %d bytes — a blank page, not a picture", target, info.Size())
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if rendererFailure(line) {
				t.Errorf("%s: the renderer failed: %s", file, strings.TrimSpace(line))
			}
		}
		t.Logf("wrote %s (%d bytes)", target, info.Size())
	}
	// dumpDOM renders the same URL again with --dump-dom and returns the
	// serialised page. A screenshot cannot be interrogated; this can.
	dumpDOM := func(hash string) string {
		t.Helper()
		profile := t.TempDir()
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir="+profile, "--window-size=1280,800", "--virtual-time-budget=15000",
			"--dump-dom", fmt.Sprintf("%s/operator/station/%d%s", srv.URL, stationID, hash))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("chrome --dump-dom %s: %v", hash, err)
		}
		return string(out)
	}

	// THE PANEL'S OWN GEOMETRY, READ BACK. SPEC S5 says never off-screen and
	// never over the bar, and the page publishes whether that held into
	// data-composer-fit (composer-render.js reportPanelFit). This is the guard
	// that fails the old layout; the console.error it replaces could not,
	// because the page's console never leaves the renderer.
	fitRe := regexp.MustCompile(`data-composer-fit="([^"]*)"`)
	errRe := regexp.MustCompile(`data-composer-error="([^"]*)"`)
	checkPanelFits := func(label, hash string) {
		t.Helper()
		dom := dumpDOM(hash)
		if m := errRe.FindStringSubmatch(dom); m != nil {
			t.Errorf("%s: the page threw: %s", label, html.UnescapeString(m[1]))
		}
		// THE CHOREOGRAPHY CONTROL HAS TO SAY WHICH CHOREOGRAPHY. SPEC S5 is
		// glyph AND name; an operator cannot read a choreography off a 26 px
		// picture, and the spec never asked them to. This is measured because
		// the way it broke was invisible to every test and plausible in a
		// screenshot: `.os-comp-stage svg { width: 100% }`, meant for the press,
		// also matched the glyphs inside the popover and stretched one to 171 px,
		// which squeezed the label to zero width.
		if sm := regexp.MustCompile(`data-composer-seg="([^"]*)"`).FindStringSubmatch(dom); sm != nil {
			var seg struct {
				BtnW     int    `json:"btnW"`
				SpanW    int    `json:"spanW"`
				SpanText string `json:"spanText"`
				GlyphW   int    `json:"glyphW"`
			}
			if err := json.Unmarshal([]byte(html.UnescapeString(sm[1])), &seg); err == nil {
				if seg.GlyphW != 26 {
					t.Errorf("%s: the swap-mode glyph renders %d px, want 26 (R6, GLYPHS-swap-mode-LOCKED)", label, seg.GlyphW)
				}
				if strings.TrimSpace(seg.SpanText) == "" {
					t.Errorf("%s: the segmented button has no name — SPEC S5 is glyph AND name", label)
				}
				if seg.SpanW < 40 {
					t.Errorf("%s: the segmented button's name is %d px wide (%q) — it is being squeezed out",
						label, seg.SpanW, seg.SpanText)
				}
			}
		}
		m := fitRe.FindStringSubmatch(dom)
		if m == nil {
			t.Errorf("%s: the page published no data-composer-fit — the panel never reported its geometry, so nothing is checking SPEC S5", label)
			return
		}
		var fit struct {
			Panel           []int `json:"panel"`
			OverlapsBar     bool  `json:"overlapsBar"`
			OverlapsPrimary bool  `json:"overlapsPrimary"`
			OutsidePicture  bool  `json:"outsidePicture"`
		}
		raw := html.UnescapeString(m[1])
		if err := json.Unmarshal([]byte(raw), &fit); err != nil {
			t.Errorf("%s: data-composer-fit is not JSON (%q): %v", label, raw, err)
			return
		}
		if fit.OverlapsBar {
			t.Errorf("%s: the panel %v covers the bottom bar — SPEC S5 says never over the bar", label, fit.Panel)
		}
		if fit.OverlapsPrimary {
			t.Errorf("%s: the panel %v covers the primary button — the one control SPEC 0.5 spends the screen's saturated colour on", label, fit.Panel)
		}
		if fit.OutsidePicture {
			t.Errorf("%s: the panel %v leaves the picture — SPEC S5 says never off-screen", label, fit.Panel)
		}
	}

	shot := func(file, styleName string) {
		t.Helper()
		styleID, ok := seeded.Styles[styleName]
		if !ok {
			t.Fatalf("style %q not seeded", styleName)
		}
		if err := db.SetActiveStyle(seeded.ProcessID, &styleID); err != nil {
			t.Fatalf("set active style: %v", err)
		}
		shotAt(file, "#flow")
	}
	// The composer's states, by style id rather than by name: the hash entry
	// takes the id the picker would have passed.
	composerShot := func(file, styleName, state, node string) {
		t.Helper()
		styleID, ok := seeded.Styles[styleName]
		if !ok {
			t.Fatalf("style %q not seeded", styleName)
		}
		h := fmt.Sprintf("#compose=%d;state=%s", styleID, state)
		if node != "" {
			h += ";node=" + node
		}
		shotAt(file, h)
	}
	composerShotHeld := func(file, styleName, state string) {
		t.Helper()
		styleID, ok := seeded.Styles[styleName]
		if !ok {
			t.Fatalf("style %q not seeded", styleName)
		}
		shotAt(file, fmt.Sprintf("#compose=%d;state=%s;hold=1", styleID, state))
	}
	// The schematic first, while no scene is cached — the state a fresh Edge
	// is in — then the scene arrives and the two choreographies are drawn to
	// scale.
	shot("u4-schematic.png", "PART 40421-RVJ56.37")
	eng.SetSceneGeometry("shots-a", points, edges)
	shot("u4-press-index.png", "PART 40421-RVJ56.37")
	shot("u4-two-robot-swap.png", "PART 68644-WSL97.20")

	// THE GATE IS OPEN FOR THE SHOTS, and it has to be: S4-S8 are unreachable
	// with flow_composer_enabled off (brief R7), so a run without this would
	// photograph the picker and the set-up card and then eight blank pages.
	// The routing set is derived and adopted first for the same reason the
	// service test does it — an unadopted backfill leaves every option list
	// empty, and a panel with no chips is not what the reference shows.
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "shots")
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, true); err != nil {
		t.Fatalf("open the gate: %v", err)
	}

	// ── U10: BOTH OFFERED SHAPES ARE NAMED, HERE, BEFORE THE STATION SHOTS ──
	//
	// S4's strip draws a card per preset, so the strip has nothing to
	// photograph until a press has some. Plant A's first press runs ten
	// flows over two shapes and the offer is those two, so naming both is what
	// an engineer does on their first visit — and it leaves the strip with the
	// two cards the brief asks for.
	//
	// NAMED FROM THE CANDIDATES, not from a style, because that is the act
	// that gives a preset MEMBERS: naming one style's flow names a shape and
	// stamps nobody (the style it came from included), which is right — the
	// engineer named a shape, they did not declare which parts run it. So a
	// card named from a style would read `not used yet`, and `used by 7 parts`
	// is the line worth photographing.
	//
	// The second one is ARCHIVED again further down, after the station shots,
	// which is what puts its shape back on D6's offer. See that block.
	var presetIDs []int64
	presetSvc := service.NewProcessService(db)
	{
		offer, err := presetSvc.PresetsFor(seeded.ProcessID)
		if err != nil {
			t.Fatalf("read the migration offer: %v", err)
		}
		if len(offer.Candidates) != 2 {
			t.Fatalf("the plant A seed offers %d shapes; these shots were written against its two", len(offer.Candidates))
		}
		for _, c := range offer.Candidates {
			id, err := presetSvc.CreateFromCandidate(seeded.ProcessID, c.SuggestedName, c.ShapeKey, "s.brown")
			if err != nil {
				t.Fatalf("name %q: %v", c.SuggestedName, err)
			}
			presetIDs = append(presetIDs, id)
			t.Logf("named %q (preset %d) over %d of %d flows", c.SuggestedName, id, len(c.Members), offer.StylesWithFlow)
		}
	}

	// ── S3: the set-up card, and the gate's one button ───────────────────
	//
	// THE GATE-OFF CARD HAS EXACTLY ONE BUTTON (owner ruling, F6). A locked-down
	// press has no alternative flow to offer, so a second control — or a
	// sentence explaining a door that is locked — tells the floor there is a
	// half of the system they are not allowed to understand. With the gate on
	// there are two: the verb, and the quiet `Edit the flow first` under it.
	//
	// Counted off the DOM rather than eyeballed in the PNG, because "one
	// button" is exactly the kind of thing a screenshot shows and no test
	// asserts. The ✕ is not counted: it is the way back, not a choice about
	// the changeover.
	cardRe := regexp.MustCompile(`(?s)<div class="os-comp-card">(.*)</div></div>`)
	cardBtnRe := regexp.MustCompile(`data-act="(runasis|compose)"`)
	checkSetupCard := func(styleID int64) {
		t.Helper()
		hash := fmt.Sprintf("#compose=%d;state=S3", styleID)

		dom := dumpDOM(hash)
		if m := errRe.FindStringSubmatch(dom); m != nil {
			t.Errorf("S3 with the gate on: the page threw: %s", html.UnescapeString(m[1]))
		}
		card := cardRe.FindStringSubmatch(dom)
		if card == nil {
			t.Fatal("S3: no set-up card in the DOM — the shot is of some other screen")
		}
		if got := cardBtnRe.FindAllString(card[1], -1); len(got) != 2 {
			t.Errorf("S3 with the gate ON has %d buttons (%v), want the verb and the quiet edit", len(got), got)
		}
		// THE COUNT AND THE VERB, not the wording. This also required three exact
		// strings to be present and four to be absent — the F6 turnover's before
		// and after. The gone-list has already turned over once, so what it holds
		// now is that nobody re-words a button, which is not what F6 ruled: F6 is
		// about how many choices the card offers, and that is the count above.
		// The verb itself is checked because "one button" and "the button that
		// starts the changeover" are different facts.
		if !strings.Contains(card[1], ">Start changeover<") {
			t.Error("S3 with the gate ON has lost its verb")
		}

		// And again with the gate off.
		if err := db.SetFlowComposerEnabled(seeded.ProcessID, false); err != nil {
			t.Fatalf("close the gate: %v", err)
		}
		defer func() {
			if err := db.SetFlowComposerEnabled(seeded.ProcessID, true); err != nil {
				t.Fatalf("re-open the gate: %v", err)
			}
		}()
		dom = dumpDOM(hash)
		card = cardRe.FindStringSubmatch(dom)
		if card == nil {
			t.Fatal("S3 with the gate off: no set-up card — the gate removes a button, not a screen")
		}
		if got := cardBtnRe.FindAllString(card[1], -1); len(got) != 1 {
			t.Errorf("S3 with the gate OFF has %d buttons (%v), want exactly one: the verb", len(got), got)
		}
		if !strings.Contains(card[1], ">Start changeover<") {
			t.Error("S3 with the gate off has lost its one button")
		}
	}

	const idx, swap = "PART 40421-RVJ56.37", "PART 68644-WSL97.20"
	// A CHANGEOVER IS FROM ONE STYLE TO ANOTHER, and the seam refuses a preview
	// of the style already on the press ("process is already running style N").
	// So each composer shot runs with the OTHER style active: from-something, and
	// never from-itself.
	runFrom := func(styleName string) {
		t.Helper()
		id, ok := seeded.Styles[styleName]
		if !ok {
			t.Fatalf("style %q not seeded", styleName)
		}
		if err := db.SetActiveStyle(seeded.ProcessID, &id); err != nil {
			t.Fatalf("set active style: %v", err)
		}
	}
	runFrom(swap)
	composerShot("02-picker.png", idx, "S2", "")
	composerShot("03-setup-card.png", idx, "S3", "")
	checkSetupCard(seeded.Styles[idx])
	composerShot("04-composer-press-index.png", idx, "S4", "")
	// THE STRIP CARD IS LOCKED AT 200x56 WITH A 32 PX GLYPH (U8 brief §A S4),
	// and neither line of it may ellipsise. Measured off the rendered card
	// (composer-render.js's reportStripFit), because both of those are exactly
	// what a screenshot shows and no test asserts: a glyph is written with
	// inline width/height and the way it goes wrong is a CSS rule reaching it
	// from elsewhere, which already happened once to the popover's 26 px one.
	//
	// The second line was `PLN_01 / PLN_04 · used by 7 parts` and ellipsised to
	// `PLN_01 / PLN_04 · use…` in 118 px — cutting off the half that says why
	// the card is worth tapping. It abbreviates the way a picker row does now.
	{
		dom := dumpDOM(fmt.Sprintf("#compose=%d;state=S4", seeded.Styles[idx]))
		m := regexp.MustCompile(`data-composer-strip="([^"]*)"`).FindStringSubmatch(dom)
		if m == nil {
			t.Error("04: the strip published no geometry — nothing is checking the card's lock")
		} else {
			var fit struct {
				CardW, CardH, GlyphW                  int
				NameText, WhereText, UseText          string
				NameClipped, WhereClipped, UseClipped bool
			}
			raw := html.UnescapeString(m[1])
			if err := json.Unmarshal([]byte(raw), &fit); err != nil {
				t.Errorf("04: data-composer-strip is not JSON (%q): %v", raw, err)
			} else {
				t.Logf("04 strip card: %dx%d, glyph %d px, name %q%s, where %q%s, use %q%s",
					fit.CardW, fit.CardH, fit.GlyphW, fit.NameText, clippedWord(fit.NameClipped),
					fit.WhereText, clippedWord(fit.WhereClipped),
					fit.UseText, clippedWord(fit.UseClipped))
				if fit.CardW != 200 || fit.CardH != 56 {
					t.Errorf("04: the card renders %dx%d, want 200x56 (U8 §A S4)", fit.CardW, fit.CardH)
				}
				if fit.GlyphW != 32 {
					t.Errorf("04: the card's glyph renders %d px, want 32 (U8 §A S4)", fit.GlyphW)
				}
				if fit.WhereText == "" {
					t.Error("04: the card names no positions; two cards of one mode would be one word apart")
				}
				if fit.WhereClipped || fit.UseClipped {
					t.Errorf("04: the card ellipsises (%q%s / %q%s) — the positions tell two cards apart and the count says why either is worth tapping",
						fit.WhereText, clippedWord(fit.WhereClipped), fit.UseText, clippedWord(fit.UseClipped))
				}
			}
		}
	}
	runFrom(idx)
	composerShot("05-composer-two-robot-swap.png", swap, "S4", "")
	runFrom(swap)
	composerShot("06-position-panel.png", idx, "S5", "PLN_01")
	checkPanelFits("06 position panel", fmt.Sprintf("#compose=%d;state=S5;node=PLN_01", seeded.Styles[idx]))
	// V2 (2026-09-12): THE PANEL'S SUB-LINE IS THE PICTURE'S ROW, NOT THE
	// CLAIM'S KIND. In plant A, PLN_01 is Kind `front` and is DRAWN in the
	// BACK row; U9 fixed the desktop's positions table to read the row and the
	// station's panel kept the old read, so 06 captioned PLN_01 `front` under a
	// picture that said BACK and beside a desktop table that said `back`.
	// F2's rule, one source, checked here because the PNG is where it showed.
	{
		dom := dumpDOM(fmt.Sprintf("#compose=%d;state=S5;node=PLN_01", seeded.Styles[idx]))
		h3 := regexp.MustCompile(`(?s)<h3>PLN_01<span>([^<]*)</span>`).FindStringSubmatch(dom)
		if h3 == nil {
			t.Error("06: no PLN_01 panel heading in the DOM")
		} else if !strings.HasPrefix(strings.TrimSpace(h3[1]), "back") {
			t.Errorf("06: the panel says %q for PLN_01; the picture and the desktop table both say back (F2)",
				strings.TrimSpace(h3[1]))
		}
		// F3 (2026-09-12): the panel's rows are the claim's own field names —
		// and from ONE table. These are flowspec.Label's words, which is what
		// a refusal about the same field says; the panel used to spell its own
		// ("Paired position" for paired_core_node), which is the second name
		// F3 exists to remove.
		for _, want := range []string{
			">" + flowspec.Label(flowspec.InboundSource) + "<",
			">" + flowspec.Label(flowspec.OutboundDestination) + "<",
			">" + flowspec.Label(flowspec.KeyRoute) + "<",
			">" + flowspec.Label(flowspec.PairedCoreNode) + "<",
		} {
			if !strings.Contains(dom, want) {
				t.Errorf("06: the position panel has no row %q — F3: a field is called what the claim calls it", want)
			}
		}
		for _, gone := range []string{"New bin comes from", "Old bin goes to", "Paired back position"} {
			if strings.Contains(dom, gone) {
				t.Errorf("06: the panel still says %q", gone)
			}
		}
	}
	composerShot("07-dock-panel.png", idx, "S6", "")
	checkPanelFits("07 dock panel", fmt.Sprintf("#compose=%d;state=S6", seeded.Styles[idx]))
	composerShot("08-new-part-blank.png", idx, "S7", "")
	// S8 is S7 with a position added and no choreography chosen — the finding
	// the server cannot raise before a preview, on the card AND in the bar.
	composerShot("09-finding-on-node-and-bar.png", idx, "S7", "PLN_03")
	checkPanelFits("09 finding", fmt.Sprintf("#compose=%d;state=S7;node=PLN_03", seeded.Styles[idx]))
	composerShot("10-confirm-rows-and-orders.png", idx, "S9", "")
	// ;hold=1 stops the 4 s countdown: without it S10 is back on the board long
	// before the 15 s virtual-time budget fires and the shot is of the board.
	composerShotHeld("11-started-auto-return.png", idx, "S10")

	// ONE NAME GOES BACK TO WAITING, so D3 photographs the state Q5's whole
	// ruling is about: a backfilled name switched off, its sub-line amber with
	// the evidence, and the panel's summary counting it. Adopting every row
	// left the shot showing only the settled case, which is the one that
	// needed no ruling.
	//
	// A REAL derived row, not an invented one, so the count under it is a real
	// count — a fabricated row reads "on 0 styles", which is a number no
	// backfill can produce and a caption that teaches the wrong thing. It is
	// put back AFTER every station shot, because the composer's pickers read
	// the ENABLED members and an un-adopted destination would empty S5's "old
	// bin goes to" list in shot 06.
	{
		// WITH counts: the plain read does not compute them (they are the
		// Routing panel's, and an unindexable correlated COUNT(DISTINCT) has
		// no business on the composer's hot path), and this shot is looking
		// for a name the panel would show evidence under.
		rows, err := db.ListRoutingNodesWithCounts(seeded.ProcessID)
		if err != nil {
			t.Fatalf("list routing set: %v", err)
		}
		var target *domain.RoutingNode
		for i := range rows {
			if rows[i].Role == domain.RoutingRoleDestination && rows[i].StyleCount > 0 {
				target = &rows[i]
				break
			}
		}
		if target == nil {
			t.Fatal("no derived destination with live styles behind it — D3 cannot show a name awaiting a decision")
		}
		if _, err := db.UpsertRoutingNode(domain.RoutingNodeInput{
			ProcessID: seeded.ProcessID, CoreNodeName: target.CoreNodeName, Role: target.Role,
			Label: target.Label, Sequence: target.Sequence,
			Origin: domain.RoutingOriginBackfill, Enabled: false,
		}); err != nil {
			t.Fatalf("put %s back to waiting: %v", target.CoreNodeName, err)
		}
	}

	// ── U9: the desktop Processes page at 1440x900 ───────────────────────
	//
	// THE THEME IS CHOSEN, NOT INHERITED. header.html picks the theme from
	// localStorage and falls back to prefers-color-scheme, and a fresh
	// --user-data-dir has no localStorage — so every desktop shot has been
	// taken at whatever headless Chrome's own default is (dark, on the Chrome
	// this runs against). That is a default, not a decision, and it is how
	// flow-picture.css shipped four literal colours that were --text-strong's
	// DARK value: on a light Processes page the position name was near-white
	// on a white card, and no shot could have shown it.
	//
	// preferredColorScheme is Blink's own setting (0 dark, 1 light), which is
	// what matchMedia answers from — so the page takes the same path a
	// browser set to that scheme takes, rather than a test-only door.
	desktopShotIn := func(file, path string, scheme int) {
		t.Helper()
		target := filepath.Join(out, file)
		_ = os.Remove(target)
		profile := t.TempDir()
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
			"--no-default-browser-check", "--user-data-dir="+profile,
			"--blink-settings=preferredColorScheme="+strconv.Itoa(scheme),
			"--window-size=1440,900", "--virtual-time-budget=15000",
			"--screenshot="+target, srv.URL+path)
		cmd.Dir = out
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("chrome %s: %v\n%s", file, err, raw)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("%s was not written: %v", target, err)
		}
		if info.Size() < 10_000 {
			t.Fatalf("%s is %d bytes — a blank page, not a screen", target, info.Size())
		}
		t.Logf("wrote %s (%d bytes)", target, info.Size())
	}
	// The dark scheme is what the reference screenshots are in, so it stays the
	// default for the set.
	desktopShot := func(file, path string) { t.Helper(); desktopShotIn(file, path, 0) }
	// A SHOT THAT CANNOT FAIL IS NOT A CHECK. The same lesson U8's
	// checkPanelFits was written for: --screenshot writes a PNG of whatever
	// rendered, including the screen behind the one asked for. So each desktop
	// state names a selector that only that state has, and the DOM is asked.
	desktopDOM := func(label, path, want string) {
		t.Helper()
		profile := t.TempDir()
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir="+profile, "--window-size=1440,900",
			"--virtual-time-budget=15000", "--dump-dom", srv.URL+path)
		cmd.Dir = out
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("chrome --dump-dom %s: %v", label, err)
		}
		// THE PAGE'S OWN ERROR FIRST. Without this a throw reports as "the
		// page rendered without <selector>", which names the symptom and
		// hides the cause — three runs of this harness to find one typo.
		if m := regexp.MustCompile(`data-pd-error="([^"]*)"`).FindStringSubmatch(string(raw)); m != nil {
			t.Errorf("%s: the page threw: %s", label, html.UnescapeString(m[1]))
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s: the page rendered without %q — the shot is of some other screen", label, want)
		}
	}

	// desktopDump is desktopDOM without an assertion: the driver pages report
	// through their own attributes, so the caller reads them rather than
	// looking for a substring.
	desktopDump := func(label, path string) string {
		t.Helper()
		profile := t.TempDir()
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir="+profile, "--window-size=1440,900",
			"--virtual-time-budget=15000", "--dump-dom", srv.URL+path)
		cmd.Dir = out
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("chrome --dump-dom %s: %v", label, err)
		}
		return string(raw)
	}

	// The other half of desktopDOM, for a state whose whole point is what is
	// NOT drawn — a row that cannot be saved must not advertise its order
	// count, and an answered row must have no question left on it (F1).
	desktopRefuteDOM := func(label, path, unwanted string) {
		t.Helper()
		profile := t.TempDir()
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir="+profile, "--window-size=1440,900",
			"--virtual-time-budget=15000", "--dump-dom", srv.URL+path)
		cmd.Dir = out
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("chrome --dump-dom %s: %v", label, err)
		}
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("%s: the page still draws %q", label, unwanted)
		}
	}

	// ── D1 FITS THE SCREEN, MEASURED ────────────────────────────────────
	//
	// Both of the things this checks were invisible to every other test and
	// plausible in a screenshot: a table whose last three columns are off the
	// right edge still renders, and a bottom bar below the fold still exists in
	// the DOM. The page publishes its own rendered geometry into
	// data-desktop-fit (reportDesktopFit) and this reads it back — the same
	// shape as U8's data-composer-fit, and for the same reason.
	deskFitRe := regexp.MustCompile(`data-desktop-fit="([^"]*)"`)
	checkDesktopFits := func(label, path string) {
		t.Helper()
		profile := t.TempDir()
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir="+profile, "--window-size=1440,900",
			"--virtual-time-budget=15000", "--dump-dom", srv.URL+path)
		cmd.Dir = out
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("chrome --dump-dom %s: %v", label, err)
		}
		m := deskFitRe.FindStringSubmatch(string(raw))
		if m == nil {
			t.Fatalf("%s: the page published no data-desktop-fit — D1 did not finish drawing", label)
		}
		var fit struct {
			Viewport     []int `json:"viewport"`
			TableScrollW int   `json:"tableScrollW"`
			TableClientW int   `json:"tableClientW"`
			PageScrollW  int   `json:"pageScrollW"`
			BarBottom    int   `json:"barBottom"`
			BarTop       int   `json:"barTop"`
			PicBottom    int   `json:"picBottom"`
			TableTop     int   `json:"tableTop"`
			TableBottom  int   `json:"tableBottom"`
			// P3
			ScrollerBottom  int    `json:"scrollerBottom"`
			HasFooter       bool   `json:"hasFooter"`
			HasPairedLine   bool   `json:"hasPairedLine"`
			FooterTop       int    `json:"footerTop"`
			FooterBottom    int    `json:"footerBottom"`
			PairedOffsetTop int    `json:"pairedOffsetTop"`
			ScrollerScrollH int    `json:"scrollerScrollH"`
			ScrollerClientH int    `json:"scrollerClientH"`
			PicW            int    `json:"picW"`
			PicH            int    `json:"picH"`
			ViewBox         string `json:"viewBox"`
			HeaderH         int    `json:"headerH"`
			RowH            int    `json:"rowH"`
			RowsVisible     int    `json:"rowsVisible"`
		}
		if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &fit); err != nil {
			t.Fatalf("%s: data-desktop-fit is not JSON: %v", label, err)
		}
		if len(fit.Viewport) != 2 {
			t.Fatalf("%s: no viewport in the fit report", label)
		}
		vw, vh := fit.Viewport[0], fit.Viewport[1]

		// T1. Equal, not "close": one pixel of overflow is a column an
		// engineer cannot see, and the fix is a narrower column and not a
		// scrollbar.
		if fit.TableScrollW > fit.TableClientW {
			t.Errorf("%s: the positions table is %d px wide in a %d px box — it scrolls sideways, so its last columns are off the screen at %dx%d",
				label, fit.TableScrollW, fit.TableClientW, vw, vh)
		}
		if fit.PageScrollW > vw {
			t.Errorf("%s: the page itself is %d px wide in a %d px viewport", label, fit.PageScrollW, vw)
		}

		// T2. The bar pins to the foot of the main column, inside the viewport,
		// and the table is the thing between the picture and it.
		if fit.BarBottom > vh {
			t.Errorf("%s: the bottom bar ends at y=%d in a %d px viewport — it is below the fold, and it is where Save lives",
				label, fit.BarBottom, vh)
		}
		if fit.TableTop < fit.PicBottom-1 {
			t.Errorf("%s: the positions table starts at y=%d, above the picture's foot at y=%d", label, fit.TableTop, fit.PicBottom)
		}
		if fit.TableBottom > fit.BarTop+1 {
			t.Errorf("%s: the positions table runs to y=%d, past the bar's top at y=%d", label, fit.TableBottom, fit.BarTop)
		}
		// ── P3. THE ADD ROW IS THE BOX'S FOOTER AND IS ALWAYS VISIBLE ──
		//
		// In the accepted D1 shot neither the add row nor the paired-above line
		// was on screen: both were rows of a tbody that scrolls, and at the
		// picture's resting height the box showed exactly the two position
		// rows. The one control that adds a position was below the fold of a
		// box with no visible scrollbar, so an engineer had no way to learn
		// that PLN_03 and PLN_06 were free.
		if !fit.HasFooter {
			t.Errorf("%s: the positions box has no add-a-position footer", label)
		} else {
			if fit.FooterBottom > fit.BarTop+1 {
				t.Errorf("%s: the add row ends at y=%d, past the bar's top at y=%d — it is under the bottom bar",
					label, fit.FooterBottom, fit.BarTop)
			}
			if fit.FooterBottom > vh {
				t.Errorf("%s: the add row ends at y=%d in a %d px viewport — below the fold",
					label, fit.FooterBottom, vh)
			}
			// It is the FOOTER: under the scroller, not inside it.
			if fit.FooterTop < fit.ScrollerBottom-1 {
				t.Errorf("%s: the add row starts at y=%d, above the scroller's foot at y=%d — it is still a scrolling row",
					label, fit.FooterTop, fit.ScrollerBottom)
			}
		}
		// THE VIEWBOX IS THE FRAME. Not a constant: the frame shrinks to give
		// the positions box its floor, and the drawing has to be laid out at
		// whatever size it ended up with rather than scaled into it.
		if want := fmt.Sprintf("0 0 %d %d", fit.PicW, fit.PicH); fit.ViewBox != want {
			t.Errorf("%s: the picture's viewBox is %q for a %dx%d frame — want %q, or the browser is scaling the whole drawing, cards and type included",
				label, fit.ViewBox, fit.PicW, fit.PicH, want)
		}
		if fit.PicH < 240 {
			t.Errorf("%s: the picture frame is %d px tall, under its 240 px floor", label, fit.PicH)
		}

		// TWO POSITION ROWS ARE ON SCREEN, WHOLE. A box that satisfied every
		// other assertion here while showing 30 px of scroller — half of one
		// row — is the state this round started in, and every one of those
		// assertions passed on it.
		//
		// Counted from the MEASURED row height: `.pd-postbl td` says 32 px and
		// a rendered row is taller, because the chips inside it are 26-28 px
		// and carry their own padding. A count divided by 32 said two rows fit
		// while the second was clipped through the middle of its chips.
		if fit.RowsVisible < 2 {
			t.Errorf("%s: %d whole position rows fit — scroller %d px, header %d, row %d. The picture is supposed to give way first (T2)",
				label, fit.RowsVisible, fit.ScrollerClientH, fit.HeaderH, fit.RowH)
		}
		// The paired-above line is the last SCROLLING row: reachable, not
		// pinned. Visible outright when the box is tall enough, which is the
		// same test at a scrollTop of zero.
		if fit.HasPairedLine {
			if fit.PairedOffsetTop > fit.ScrollerScrollH {
				t.Errorf("%s: the paired-above line sits at %d px inside a %d px scroll height — it cannot be reached",
					label, fit.PairedOffsetTop, fit.ScrollerScrollH)
			}
			reach := fit.ScrollerScrollH - fit.ScrollerClientH
			t.Logf("%s paired-above line at %d px; scroller %d in %d (%d px of scroll)",
				label, fit.PairedOffsetTop, fit.ScrollerScrollH, fit.ScrollerClientH, reach)
		}
		t.Logf("%s rows: %d whole (scroller %d px, header %d, row %d)",
			label, fit.RowsVisible, fit.ScrollerClientH, fit.HeaderH, fit.RowH)
		t.Logf("%s fits: viewport %dx%d · table %d in %d · picture %dx%d (viewBox %q) foot %d · box %d..%d · footer %d..%d · bar %d..%d",
			label, vw, vh, fit.TableScrollW, fit.TableClientW, fit.PicW, fit.PicH, fit.ViewBox, fit.PicBottom,
			fit.TableTop, fit.TableBottom, fit.FooterTop, fit.FooterBottom, fit.BarTop, fit.BarBottom)
	}

	desktopShot("P0-processes.png", "/processes")
	// D1 IS SHOT ON A STYLE THAT IS NOT RUNNING, as the reference draws it
	// (PIA26/27 selected while PIA11/12 is on the press). Opened with no
	// #style the page selects the RUNNING one, and the seam then refuses its
	// preview — "process is already running style N" — so the bottom bar in
	// the shot was a refusal rather than the "Preview OK · 4 orders" line the
	// reference shows. A changeover is from one style to another.
	d1 := fmt.Sprintf("/processes?process=%d#style=%d", seeded.ProcessID, seeded.Styles[idx])
	// The assets these two pages name are checked in the gate now, by
	// www/static_assets_served_test.go — it needs no browser, and behind the
	// shots tag it only ran when someone installed Chrome and remembered.
	desktopShot("D1-flows-selected.png", d1)
	// D1 ON THE RUNNING PART (owner ruling R3, 2026-09-12). Until this round
	// the seam refused a preview of the style on the press — `process is
	// already running style N` — so this was a shot of a refusal, and D1's
	// header sentence (`running — a saved change applies at the next
	// changeover`) sat over a Save button that could only ever fail. The
	// refusal is gone: a save to the running style writes its rows and starts
	// nothing, and its preview validates without planning, so the bar says the
	// two true things and the header sentence is finally one of them.
	// D1 IN THE LIGHT THEME, the same state. Half this branch's colour work is
	// about the light page — flow-picture.css's literals, the --scrim token,
	// the dead --status-alarm fallback — and every one of those defects was
	// invisible in a dark shot.
	desktopShotIn("D1-flows-selected-light.png", d1, 1)
	d1run := fmt.Sprintf("/processes?process=%d#style=%d", seeded.ProcessID, seeded.Styles[swap])
	desktopShot("D1-flows-running.png", d1run)
	for _, want := range []string{
		"running — saved changes apply at the next changeover",
		"orders not previewed while running",
	} {
		desktopDOM("D1 running bar", d1run, want)
	}
	// F4 (2026-09-12): RUNNING IS A STATE, NOT A SENTENCE. The header's sub
	// line carried the whole sentence and was not wide enough for it — the shot
	// read `…applies at the next cha…`, a sentence that stops before the half
	// that matters. The rail's own green tag says the state; the bar above says
	// the sentence, whole, and always did.
	desktopDOM("D1 running tag", d1run, `class="pd-tag run">running<`)
	desktopRefuteDOM("D1 header says it once", d1run, `· running — a saved change applies`)

	// F3: D1's headings are the claim's own field names, READ FROM THE ONE
	// TABLE. Spelled out here they were a second copy of the word, and they
	// drifted from it the moment flowspec's label changed — "Inbound source"
	// against the claim's "Inbound Source", "Paired" against "Paired Core
	// Node". A pin that has its own spelling of the thing it pins is not
	// pinning it.
	for _, f := range []flowspec.Field{
		flowspec.InboundSource, flowspec.OutboundDestination,
		flowspec.KeyRoute, flowspec.PairedCoreNode,
	} {
		desktopDOM("D1 field names", d1, "<th>"+flowspec.Label(f)+"</th>")
	}
	for _, gone := range []string{"New bins from", "Old bins to", "Robot drives via"} {
		desktopRefuteDOM("D1 invents no second name", d1, gone)
	}
	// D2 is D1 with one position's Advanced sheet open. The hash is the page's
	// own deep link (#style=;adv=), not a shot-only door: the harness cannot
	// drive a mouse and a state only a mouse can reach is a state no shot
	// checks.
	d2 := fmt.Sprintf("/processes?process=%d#style=%d;adv=PLN_01", seeded.ProcessID, seeded.Styles[idx])
	desktopShot("D2-advanced.png", d2)
	desktopDOM("P0 processes", "/processes", `class="pd-tbl"`)
	desktopDOM("D1 flows", d1, `class="pd-postbl"`)
	// THE PICTURE IS DRAWN AT THE COLUMN'S OWN SIZE — asserted inside
	// checkDesktopFits now, as the RELATION it is, rather than by looking for
	// ` 430"` in the DOM. 430 was the frame's resting height and the height no
	// longer stays there: P3 gave the positions box a floor and the picture
	// gives way to it (T2's rule), so the old check went red on a page that
	// was doing exactly what it was told. The property never was "the frame is
	// 430 tall"; it is "the viewBox is the frame", and a page still carrying
	// the station's 1280x560 fails that at any frame size.
	// The two mode-dependent columns are separate (owner ruling Q1). One
	// "Partner" column could only ever hold one of them.
	desktopDOM("D1 staging column", d1, `<th>Staging</th>`)
	// P3: the add row is the box's footer, not a row of the table.
	desktopDOM("D1 add-position footer", d1, `class="pd-posfoot"`)
	checkDesktopFits("D1 flows", d1)
	desktopDOM("D2 advanced", d2, `class="pd-modal"`)
	d3 := fmt.Sprintf("/processes?process=%d#tab=routing", seeded.ProcessID)
	desktopShot("D3-routing-set.png", d3)
	desktopDOM("D3 routing set", d3, `class="pd-rsp"`)
	// The map is the screen. A routing panel over an empty frame is the shape
	// of D3 with none of its content, and that is exactly what a missing
	// geometry cache or a bad projection would leave behind.
	desktopDOM("D3 map", d3, `class="mp-pos`)
	desktopDOM("D3 supply path", d3, `class="mp-route r1"`)
	d4 := fmt.Sprintf("/processes?process=%d#tab=screens", seeded.ProcessID)
	d5 := fmt.Sprintf("/processes?process=%d#tab=settings", seeded.ProcessID)
	desktopShot("D4-operator-screens.png", d4)
	desktopShot("D5-settings.png", d5)
	desktopDOM("D4 operator screens", d4, `data-act="screen-add"`)
	desktopDOM("D5 settings", d5, `class="pd-seg"`)

	// ── THE ONE REAL CLICK ──────────────────────────────────────────────
	//
	// D5's Save, driven through /__shots/click-d5 (see the driver above): a
	// real click on the real button, the real PUT and PATCH, and the page
	// re-reading the result. Checked from BOTH ends — the driver says the app
	// bar's pill flipped, and the store says the row did — because a page that
	// redrew from its own optimistic state would satisfy the first alone.
	gateBefore, err := db.GetProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("read the gate before the click: %v", err)
	}
	{
		profile := t.TempDir()
		cmd := exec.Command(chrome,
			"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
			"--user-data-dir="+profile, "--window-size=1440,900",
			"--virtual-time-budget=30000", "--dump-dom",
			fmt.Sprintf("%s/__shots/click-d5?process=%d", srv.URL, seeded.ProcessID))
		cmd.Dir = out
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("chrome --dump-dom click-d5: %v", err)
		}
		attr := func(name string) string {
			m := regexp.MustCompile(`data-` + name + `="([^"]*)"`).FindStringSubmatch(string(raw))
			if m == nil {
				return ""
			}
			return html.UnescapeString(m[1])
		}
		if got := attr("result"); got != "OK" {
			t.Errorf("D5 save click: %q (step %q).\n"+
				"  This is the one check in this file that presses a button. Every other one renders, "+
				"and a page whose write bodies are undefined renders perfectly.", got, attr("step"))
		} else {
			t.Logf("D5 save click: gate %s -> %s, the app bar re-read it", attr("before"), attr("after"))
		}
	}
	gateAfter, err := db.GetProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("read the gate after the click: %v", err)
	}
	if gateAfter.FlowComposerEnabled == gateBefore.FlowComposerEnabled {
		t.Errorf("the gate is still %v after the click — the page redrew but the write never landed",
			gateBefore.FlowComposerEnabled)
	}
	// PUT IT BACK. D6 below is shot over this process and its own fixture
	// opens the gate; leaving it flipped would make every state after this one
	// depend on the order the checks happen to run in.
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, gateBefore.FlowComposerEnabled); err != nil {
		t.Fatalf("restore the gate after the click: %v", err)
	}

	// ── U10: D6 · Presets at 1440x900 ────────────────────────────────────
	//
	// THE TAB NEEDS SOMETHING TO SHOW, and what it shows is computed from
	// truth — so the fixture is made the way an engineer makes it: name one
	// style's flow as a preset (which stamps the styles that already run that
	// shape), then hand-edit ONE of the members so the drift figure is real.
	// Nothing here writes a drift flag; there is no such flag to write.
	//
	// The HK seed runs ten styles with a flow over two shapes, so naming one
	// leaves the other in FOUND IN YOUR FLOWS — which is how one page can show
	// both sections at once.
	// THE DESTINATION D3's BLOCK PARKED STAYS PARKED (owner ruling R4,
	// 2026-09-12), and D6 is shot over it. That block un-adopts one derived
	// destination on purpose so D3 can show a name awaiting a decision — and
	// `Supermarket Area` is the one it picks, which is the destination every
	// HK flow sends old bins to.
	//
	// U10 re-adopted it here, because CreateFlowPreset validated against the
	// ENABLED routing set and refused to name any of these flows while the row
	// waited:
	//
	//   cell 1 (PLN_01): outbound_destination "Supermarket Area" not a
	//   position or a routing-set member of this process
	//
	// A press could RUN a shape it could not NAME — `flow/save` never checked
	// `enabled`. R4 settled it: the switch is the engineer filtering what
	// OPERATORS are offered, and says nothing about what a flow may name. So
	// the fixture leaves the press in the state a backfilled one arrives in,
	// and the naming below is the pin.
	{
		rows, err := db.ListRoutingNodes(seeded.ProcessID)
		if err != nil {
			t.Fatalf("list routing set before D6: %v", err)
		}
		parked := 0
		for _, r := range rows {
			if !r.Enabled {
				parked++
				t.Logf("D6 fixture: %s (%s) stays un-adopted, and D6 is shot over it (R4)", r.CoreNodeName, r.Role)
			}
		}
		if parked == 0 {
			t.Fatal("no un-adopted routing row left by D3's block — R4's pin has nothing to prove")
		}
		// R4 — a routing row nobody has adopted is still nameable — is pinned in
		// the gate by store/flow_presets_test.go's
		// TestFlowPreset_RefusesANodeOutsidePositionsAndRoutingSet, which checks
		// it before AND after adoption. It was also asserted here, where it cost
		// a preset that had to be archived again so it would not show up in the
		// shot. The un-adopted row above is still what D6 is photographed over.
	}

	// THE SECOND SHAPE GOES BACK ON OFFER, by archiving the preset that named
	// it. That is the honest way to get both D6 sections onto one page with a
	// seed that runs exactly two shapes, and it is a real act with a real
	// consequence: archiving hides a version from both screens, so nothing
	// active claims that shape any more and the migration offer picks it up
	// again. The members keep their provenance — a claim points at the row and
	// that reference has to keep meaning what it meant — which is why the
	// offer has to look at the ACTIVE presets and not at the stamps.
	presetID := presetIDs[0]
	presetName := ""
	if err := presetSvc.Archive(presetIDs[1]); err != nil {
		t.Fatalf("archive the second preset: %v", err)
	}

	// A DRIFTED MEMBER, made the only way drift can be made: by editing a
	// member away from the shape. Nothing here writes a drift flag, because
	// there is no such flag — the figure is computed from truth on every read.
	{
		view, err := presetSvc.PresetsFor(seeded.ProcessID)
		if err != nil {
			t.Fatalf("read the presets back: %v", err)
		}
		var row *service.PresetRow
		for i := range view.Presets {
			if view.Presets[i].ID == presetID {
				row = &view.Presets[i]
			}
		}
		if row == nil {
			t.Fatalf("the preset just named is not in the read")
		}
		presetName = row.Name
		if len(row.Members) < 2 {
			t.Fatalf("the preset has %d members; D6's shot needs one to drift and one to stay in step",
				len(row.Members))
		}
		victim := row.Members[0].StyleID
		claims, err := db.ListStyleNodeClaims(victim)
		if err != nil {
			t.Fatalf("read the victim's claims: %v", err)
		}
		if len(claims) == 0 {
			t.Fatal("the victim has no claim to edit")
		}
		in := domain.InputFromClaim(claims[0])
		// ONE FIELD, the one D1 heads `Outbound destination`, so the member
		// reads `drifted · PLN_0x · outbound destination` in the table's own
		// words — which are the CLAIM's words on both now (F3).
		in.OutboundDestination = "Supermarket Empty Totes"
		if _, err := db.UpsertStyleNodeClaim(in); err != nil {
			t.Fatalf("drift the victim: %v", err)
		}
		after, err := presetSvc.PresetsFor(seeded.ProcessID)
		if err != nil {
			t.Fatalf("re-read after the drift: %v", err)
		}
		// The counts these three lines asserted are held in the gate by
		// service/flow_preset_service_test.go (TestPresets_ArchiveHidesItAndKeepsProvenance,
		// TestPresets_DriftIsComputedFromTruth). What is needed HERE is the state:
		// one archived preset so its shape is back on offer, one drifted member so
		// the row reads `drifted`. The t.Logf below says what the shot was taken over.
		d := after.Presets[0].Drifted
		t.Logf("D6 fixture: %q keeps %d members, style %d (%s) drifted on %v; %q back on offer",
			presetName, len(after.Presets[0].Members), victim, d[0].Name, d[0].Fields,
			after.Candidates[0].SuggestedName)
	}

	// WHICH PART THE APPLY MODAL'S SHOT TICKS, chosen rather than named.
	//
	// It has to be a style that (a) is not the one the press is running —
	// flow/preview refuses that one by name — and (b) does not already run the
	// named shape, because re-applying a shape to a part that has it reads
	// `already this shape — nothing would change`, which is the true answer and
	// not the one the shot is for. So: a member of the shape that is STILL
	// UNNAMED, which is exactly the migration this modal performs.
	var tickStyle int64
	{
		view, err := eng.ProcessService().PresetsFor(seeded.ProcessID)
		if err != nil {
			t.Fatalf("read the offer for a tick target: %v", err)
		}
		running := int64(0)
		if p, err := db.GetProcess(seeded.ProcessID); err == nil && p.ActiveStyleID != nil {
			running = *p.ActiveStyleID
		}
		for _, c := range view.Candidates {
			for _, m := range c.Members {
				if m != running {
					tickStyle = m
					break
				}
			}
			if tickStyle != 0 {
				break
			}
		}
		if tickStyle == 0 {
			t.Fatal("no unnamed-shape style that the press is not running — the apply shot has nothing to change")
		}
		t.Logf("D6 fixture: the apply modal ticks style %d (running is %d)", tickStyle, running)
	}

	d6 := fmt.Sprintf("/processes?process=%d#tab=presets", seeded.ProcessID)
	d6open := fmt.Sprintf("/processes?process=%d#tab=presets;preset=%d", seeded.ProcessID, presetID)
	desktopShot("D6-presets.png", d6)
	desktopShot("D6-presets-member-expanded.png", d6open)
	desktopDOM("D6 presets", d6, `class="pd-tbl pd-presettbl"`)
	// THE OFFER IS ON THE SAME PAGE. One shape named, the other still unnamed:
	// a shot with only the first section would not show that the migration
	// offer is a section rather than a separate screen.
	desktopDOM("D6 found shapes", d6, `class="pd-tbl pd-candtbl"`)
	// Drift is amber and never the accent, and it names the field.
	desktopDOM("D6 drift", d6, `class="pd-warn"`)
	desktopDOM("D6 member list", d6open, `class="pd-mem"`)
	// The drifted line names the FIELD, in the claim's own word — the same
	// flowspec.Label the panel row and the column heading use. Spelled
	// lowercase here it was a third spelling of it.
	desktopDOM("D6 named field", d6open, flowspec.Label(flowspec.OutboundDestination))

	// THE APPLY MODAL, with one part ticked and its previewed diff on screen.
	// `;tick=` previews that row, which is the whole rule this modal exists to
	// enforce: never a save without its preview drawn.
	d6apply := fmt.Sprintf("/processes?process=%d#tab=presets;apply=%d;tick=%d",
		seeded.ProcessID, presetID, tickStyle)
	desktopShot("D6-apply-modal.png", d6apply)
	desktopDOM("D6 apply modal", d6apply, `class="pd-modal pd-apply"`)
	desktopDOM("D6 apply diff", d6apply, `class="pd-diff"`)
	// NONE PRE-TICKED except the one the hash ticked: an unticked row must not
	// carry a diff, because a diff on screen is what the save is agreed from.
	desktopDOM("D6 apply tick", d6apply, `class="pd-tick on"`)
	// R8 (2026-09-12): THE DIFF SAYS WHICH PARTS THE APPLY LEAVES LOOSE. A
	// preset carries no part, so a position the new shape does not name
	// releases whatever was on it — and the row that said only `PLN_03 ·
	// removed from the flow` now also says what came off it. The row stays
	// saveable; the part is a finding on the flow, and that is what stops a
	// changeover.
	desktopDOM("D6 apply unplaced", d6apply, `will need a position`)
	// AND THE PREVIEW'S OWN FINDINGS ARE ON THE ROW. Measured on this fixture:
	// applying PLN_01/PLN_04 to a part running PLN_03/PLN_06 lands two cells
	// with no part, which the server answers with `PLN_01 · Select a payload`
	// and `PLN_04 · Select a payload` while still planning two press-index
	// swaps. The modal drew `2 orders after the change` over an enabled
	// `Save to 1 part`, and the engineer met the rest as a 422 afterwards.
	desktopDOM("D6 apply row findings", d6apply, `class="pd-warn"`)
	// R5: THE POSITIONS ARE IN THE TITLE, off the shape — the name is whatever
	// the engineer typed and this modal is about to write that shape onto parts.
	desktopDOM("D6 apply title names the positions", d6apply, `PLN_01 / PLN_04`)
	// R6: Rename is in the row menu now that there is an endpoint that means it.
	desktopDOM("D6 rename in the menu", d6, `data-act="preset-menu"`)

	// ── F1 (2026-09-12): THE MODAL NEVER OFFERS A SAVE SHINGO REFUSES ──────
	//
	// The applied shape brings PLN_01 and PLN_04 into a part that runs
	// PLN_03/PLN_06, so it has nobody to put on them — the one question a
	// preset cannot answer. The modal asks it, inline, and the row is out of
	// the count until it is answered. Two shots, because both halves are the
	// ruling: the question, and the row once it is answered.
	desktopDOM("D6 apply asks", d6apply, `data-act="papply-pick"`)
	desktopDOM("D6 apply asks by position", d6apply, `PLN_01 · which part? ▾`)
	desktopDOM("D6 apply counts none", d6apply, `>Save to 0 parts<`)
	// AND THE ORDER COUNT IS NOT DRAWN over a row that cannot be saved — the
	// bar's own rule, that findings replace the count.
	desktopRefuteDOM("D6 apply hides the order count", d6apply, `orders after the change`)

	// THE SAME ROW, ANSWERED BY CLICKING. This used to be `;pick=auto` on the
	// hash, which ran a sixteen-line answer-every-picker loop that shipped to
	// the plant in processes-desktop.js and was called by nothing but this
	// line. The driver clicks the `which part?` chip and then the first part it
	// offers, until no chip is left — what a person does, through the page's
	// own handlers.
	//
	// The assertions come back through the DRIVER's attributes because
	// --dump-dom serialises the top document and the page is in an iframe.
	d6picked := fmt.Sprintf("/__shots/click-apply?process=%d&preset=%d&style=%d",
		seeded.ProcessID, presetID, tickStyle)
	desktopShot("D6-apply-modal-answered.png", d6picked)
	{
		dom := desktopDump("D6 apply answered", d6picked)
		attr := func(name string) string {
			m := regexp.MustCompile(`data-` + name + `="([^"]*)"`).FindStringSubmatch(dom)
			if m == nil {
				return ""
			}
			return html.UnescapeString(m[1])
		}
		if got := attr("result"); got != "OK" {
			t.Errorf("D6 apply answered: %q (step %q). saw: save=%q pickers=%q warn=%q row=%q",
				got, attr("step"), attr("saw-label"), attr("saw-pickers"), attr("saw-warn"), attr("saw-row"))
		}
		// WHAT A CLICK PROVES, which is what this driver is for: the chip and
		// the part chip are wired to a handler and the question they answered
		// is gone. That is the whole of the bug removing `;pick=auto` found —
		// the apply modal draws into #pd-scrim, which the template puts outside
		// #pd-root, and onClick (which had these cases) is bound to #pd-root,
		// so every control in this modal rendered and did nothing.
		//
		// IT DOES NOT ASSERT `Save to 1 part`. The state the retired door
		// reached is not reachable by clicking on this fixture — see the report
		// of 2026-09-13: the second position's offer does not include the part
		// the row says still needs one, so answering every question the modal
		// asks still leaves it unsaveable. `;pick=auto` got there by calling the
		// reducer with a part the offer never showed. That is a product
		// question, it is written down, and it is not this harness's to paper
		// over with a door.
		before, left := attr("pickers-before"), attr("pickers-left")
		if before == "" || before == "0" {
			t.Errorf("D6 apply answered: the row began with %q questions — nothing to click", before)
		}
		if left >= before {
			t.Errorf("D6 apply answered: %s question(s) before the clicks and %s after — the chip is "+
				"not wired to a handler", before, left)
		}
	}

	// D1'S FOURTH ACTION. Disabled while dirty — the shot is of a clean draft,
	// so it is enabled, and that is the state worth photographing because the
	// disabled one is the default.
	desktopDOM("D1 save as preset", d1, `data-act="save-preset"`)

	time.Sleep(200 * time.Millisecond) // let the last SSE client drop before the server closes
}

// clippedWord makes the strip log say which half ellipsised without a second
// t.Logf per field.
func clippedWord(b bool) string {
	if b {
		return " (ellipsised)"
	}
	return ""
}

// wireScene is the pull as a full node-list response would have carried it.
func wireScene(fx scenefixtures.Plant) ([]protocol.ScenePointInfo, []protocol.SceneEdgeInfo) {
	var pts []protocol.ScenePointInfo
	for _, p := range fx.ScenePoints {
		x, y, d := p.PosX, p.PosY, p.Dir
		pts = append(pts, protocol.ScenePointInfo{InstanceName: p.InstanceName, ClassName: p.ClassName, PosX: &x, PosY: &y, Dir: &d})
	}
	var eds []protocol.SceneEdgeInfo
	for _, e := range fx.SceneEdges {
		fx0, fy0, tx0, ty0 := e.FromX, e.FromY, e.ToX, e.ToY
		info := protocol.SceneEdgeInfo{From: e.FromName, To: e.ToName, FromX: &fx0, FromY: &fy0, ToX: &tx0, ToY: &ty0,
			Ctrl1X: e.Ctrl1X, Ctrl1Y: e.Ctrl1Y, Ctrl2X: e.Ctrl2X, Ctrl2Y: e.Ctrl2Y}
		eds = append(eds, info)
	}
	return pts, eds
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

// clickD5Driver is the harness page for the one real click: D5's Settings save.
//
// IT TOGGLES THE GATE, which is the control whose save calls BOTH bodies in
// desktop-bodies.js — processSettings on the PUT and processGate on the PATCH
// — so a page whose window.DesktopBodies is undefined fails here on the first
// one. A field edit would exercise only the PUT.
//
// EVERY STEP IS POLLED, not slept on: the page draws from a fetch, the save is
// two awaited fetches and a reload, and a fixed wait is a flake waiting for a
// slow box. The driver gives up after its own budget and says which step it
// was on, because "no result" and "the click did nothing" are different
// findings.
func clickD5Driver(processID string) string {
	return `<!doctype html><meta charset="utf-8"><title>click-d5</title>
<body data-step="starting" data-result="">
<iframe id="f" style="width:1440px;height:900px;border:0"
        src="/processes?process=` + template.HTMLEscapeString(processID) + `#tab=settings"></iframe>
<script>
const out = document.body;
const fail = (step, why) => { out.dataset.step = step; out.dataset.result = 'FAILED: ' + why; };
const until = (step, fn, ms = 8000) => new Promise((resolve, reject) => {
    const t0 = Date.now();
    (function poll() {
        let v = null;
        try { v = fn(); } catch (e) { return reject(step + ': threw ' + e.message); }
        if (v) return resolve(v);
        if (Date.now() - t0 > ms) return reject(step + ': never happened within ' + ms + 'ms');
        setTimeout(poll, 50);
    })();
});
(async () => {
  try {
    const d = await until('the settings tab draws', () => {
        const doc = document.getElementById('f').contentDocument;
        return doc && doc.querySelector('.pd-settings') ? doc : null;
    });
    if (d.body.dataset.pdError) return fail('load', 'the page threw: ' + d.body.dataset.pdError);

    const toggle = d.querySelector('[data-act="st-toggle"][data-st="flow_composer_enabled"]');
    if (!toggle) return fail('find the gate toggle', 'no [data-st=flow_composer_enabled] on the settings sheet');
    const before = toggle.classList.contains('on');
    out.dataset.before = before ? 'on' : 'off';

    // A THROW IS CHECKED INSIDE EVERY WAIT, not after it. The dead-buttons
    // failure this whole check exists for shows up as the page doing nothing,
    // and "nothing happened within 8s" names the symptom while the reporter
    // already has the cause sitting in an attribute.
    const threw = () => {
        if (d.body.dataset.pdError) throw new Error('the page threw: ' + d.body.dataset.pdError);
        const refusal = d.querySelector('.pd-refusal');
        if (refusal) throw new Error('the server refused: ' + refusal.textContent);
    };

    toggle.click();
    const save = await until('Save settings becomes enabled',
        () => { threw(); const b = d.querySelector('[data-act="st-save"]'); return b && !b.disabled ? b : null; });

    save.click();

    // THE PAGE RE-READ IT: the app bar's pill is drawn from the process the
    // page holds, and saveSettings reloads that from the server before
    // redrawing. So the pill flipping is the round trip, not the click.
    await until('the app bar re-reads the gate', () => {
        threw();
        const pill = d.querySelector('.pd-gate .pd-pill');
        if (!pill) return false;
        return pill.classList.contains('on') !== before;
    });

    out.dataset.after = before ? 'off' : 'on';
    out.dataset.step = 'done';
    out.dataset.result = 'OK';
  } catch (e) {
    fail('driver', String(e));
  }
})();
</script>`
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

// clickApplyDriver answers every picker the apply modal raises, by clicking.
//
// WHY A DRIVER AND NOT A HASH. This state used to be reached through
// `;pick=auto`, which ran autoAnswerApplyRow — sixteen lines of "answer each
// picker with the first free part, re-previewing between" living in
// processes-desktop.js with one caller, this harness, which never runs in CI.
// That is a second implementation of an engineer's clicks shipped to the
// plant so a screenshot could reach a screen. This file's own rule already
// says a door only the harness can open is forbidden; the rule applies to a
// door only the harness WALKS THROUGH too.
//
// So the driver clicks what a person clicks: the `which part?` chip, then the
// first part it offers, until no chip is left. One at a time and polled
// between, because a pick re-previews over the network and the next chip is
// drawn from what came back.
//
// THE IFRAME IS THE VIEWPORT. The screenshot of this page has to be a
// screenshot of the product, so the body has no margin and the frame is the
// window — the PNG is the Processes page, reached by clicking.
func clickApplyDriver(processID string, presetID, styleID int64) string {
	src := fmt.Sprintf("/processes?process=%s#tab=presets;apply=%d;tick=%d",
		template.HTMLEscapeString(processID), presetID, styleID)
	return `<!doctype html><meta charset="utf-8"><title>click-apply</title>
<style>html,body{margin:0;padding:0;overflow:hidden}</style>
<body data-step="starting" data-result="">
<iframe id="f" style="width:1440px;height:900px;border:0;display:block" src="` + src + `"></iframe>
<script>
const out = document.body;
// THE TICKED ROW'S OWN PICKERS. The modal lists every style on the press and
// draws a picker on any row the shape leaves a position empty on, ticked or
// not — so "the first picker in the modal" is usually a row nobody asked
// about, whose offer is the style's parts rather than the ones this apply
// just unplaced (tickApplyRow captures those, and only for the row it ticks).
// Answering those churned for twelve rounds and never made the ticked row
// saveable.
const STYLE = ` + fmt.Sprint(styleID) + `;
const pickSel = '[data-act="papply-pick"][data-style="' + STYLE + '"]';
const partSel = '[data-act="papply-part"][data-style="' + STYLE + '"]';
const fail = (step, why) => { out.dataset.step = step; out.dataset.result = 'FAILED: ' + why; };
const until = (step, fn, ms = 8000) => new Promise((resolve, reject) => {
    const t0 = Date.now();
    (function poll() {
        let v = null;
        try { v = fn(); } catch (e) { return reject(step + ': threw ' + e.message); }
        if (v) return resolve(v);
        if (Date.now() - t0 > ms) return reject(step + ': never happened within ' + ms + 'ms');
        setTimeout(poll, 50);
    })();
});
(async () => {
  let d = null;
  try {
    d = await until('the apply modal draws', () => {
        const doc = document.getElementById('f').contentDocument;
        return doc && doc.querySelector(pickSel) ? doc : null;
    });
    const threw = () => {
        if (d.body.dataset.pdError) throw new Error('the page threw: ' + d.body.dataset.pdError);
        const refusal = d.querySelector('.pd-refusal');
        if (refusal) throw new Error('the server refused: ' + refusal.textContent);
    };
    const label = () => {
        const b = d.querySelector('[data-act="sheet-ok"]');
        return b ? b.textContent.trim() : '';
    };
    // ANSWER EACH QUESTION WITH A PART NOT ALREADY USED.
    //
    // ONE POSITION PER PART: the reducer's setPart MOVES a part rather than
    // copying it, so answering a second picker with a part already placed
    // empties the position it came from and raises a fresh question in its
    // place. A person does not offer the same part twice; nor does this. The
    // offer list leads with the parts the shape just unplaced (F1's ordering),
    // so "the first one not used yet" is the answer the modal is fishing for.
    const used = new Set();
    const pickersAtStart = d.querySelectorAll(pickSel).length;
    let answered = 0;
    for (let round = 0; round < 12; round++) {
        threw();
        const chip = d.querySelector(pickSel);
        if (!chip) break;
        chip.click();
        const opts = await until('the picker opens', () => {
            threw();
            const o = d.querySelectorAll(partSel);
            return o.length ? o : null;
        });
        // TRY AN OPTION, AND CHECK IT HELPED.
        //
        // The offer includes parts this row ALREADY has somewhere, and setPart
        // MOVES rather than copies — so answering with one of those empties the
        // position it came from and the question count does not fall. The door
        // this replaced filtered the offer by the model's placed set, which the
        // DOM does not publish. What the DOM does publish is the result: if the
        // number of questions on this row did not drop, that option moved a part
        // instead of placing one, so it is burned and the next is tried.
        const before = d.querySelectorAll(pickSel).length;
        let part = null;
        for (const o of opts) {
            if (!used.has(o.dataset.part)) { part = o; break; }
        }
        if (!part) return fail('answer the pickers',
            'every part this picker offers has been tried: ' +
            Array.from(opts).map(o => o.dataset.part).join(', '));
        used.add(part.dataset.part);
        part.click();
        answered++;
        await until('the row redraws after a pick',
            () => { threw(); return !d.querySelector(partSel); });
        if (d.querySelectorAll(pickSel).length >= before) {
            // It moved a part rather than placing one. The used set already
            // holds it, so the next round takes the next option.
            continue;
        }
    }
    if (!answered) return fail('answer the pickers', 'the modal raised no picker to answer');

    // WHAT THE ANSWERED MODAL SAYS, copied out of the real page for --dump-dom
    // to read. Chrome serialises the top document only, so an assertion about
    // the iframe's markup has to come through the driver — the same shape
    // click-d5 uses for the gate pill.
    out.dataset.answered = String(answered);
    out.dataset.saveLabel = label();
    out.dataset.pickersBefore = String(pickersAtStart);
    out.dataset.pickersLeft = String(d.querySelectorAll(pickSel).length);
    out.dataset.orders = /orders after the change/.test(d.body.innerHTML) ? 'yes' : 'no';
    out.dataset.unplaced = /will need a position/.test(d.body.innerHTML) ? 'yes' : 'no';
    out.dataset.step = 'done';
    out.dataset.result = 'OK';
  } catch (e) {
    // WHAT IT SAW, not just that it gave up. A timeout with no state is three
    // runs of this harness to learn which half was wrong.
    try {
        if (d) {
            const b = d.querySelector('[data-act="sheet-ok"]');
            out.dataset.sawLabel = b ? b.textContent.trim() : '(no save button)';
            out.dataset.sawPickers = String(d.querySelectorAll('[data-act="papply-pick"]').length);
            out.dataset.sawWarn = ((d.querySelector('.pd-warn') || {}).textContent || '').slice(0, 200);
            out.dataset.sawRow = (d.querySelector('.pd-alist') || {}).textContent
                ? (d.querySelector('.pd-alist').textContent || '').replace(/\s+/g, ' ').slice(0, 300) : '';
        }
    } catch (_) { /* the iframe is gone; the message below is what there is */ }
    fail('driver', String(e));
  }
})();
</script>`
}
