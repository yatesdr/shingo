package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pin_wire_test.go — lane W pins (ui-cleanup, 2026-10-07), Core side.
//
// P0 pinned each control AS DEAD at 5c0beb74 (built and one connection
// short). The W unit wired them and flipped each pin to the wired state, under
// its label (W1, W4, W5); the before and after of every case is in the
// evidence folder (predictions/p0-wrm.md).

// pinDelegateActionsKeys returns the keys of every `delegateActions(document.body,
// { … })` map in a page script, in source order, one slice per call. A key is the
// shorthand name or the name before `:`; quoted keys are unquoted.
func pinDelegateActionsKeys(src string) [][]string {
	var maps [][]string
	open := regexp.MustCompile(`delegateActions\(\s*document\.body\s*,\s*\{`)
	keyRe := regexp.MustCompile(`^['"]?([\w-]+)['"]?`)
	for _, loc := range open.FindAllStringIndex(src, -1) {
		depth, i := 1, loc[1]
		for ; i < len(src) && depth > 0; i++ {
			switch src[i] {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				depth--
			}
		}
		body := src[loc[1] : i-1]
		var keys []string
		depth = 0
		start := 0
		for j := 0; j <= len(body); j++ {
			if j < len(body) {
				switch body[j] {
				case '{', '(', '[':
					depth++
				case '}', ')', ']':
					depth--
				}
				if body[j] != ',' || depth != 0 {
					continue
				}
			}
			part := strings.TrimSpace(body[start:j])
			start = j + 1
			if part == "" {
				continue
			}
			if m := keyRe.FindStringSubmatch(part); m != nil {
				keys = append(keys, m[1])
			}
		}
		maps = append(maps, keys)
	}
	return maps
}

func pinReadFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("read %s: %v (a pin that cannot find its subject passes on nothing; repoint it)", rel, err)
	}
	return string(b)
}

func pinContains(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// W1 (Core). test-orders.js imports hideModal and now maps it, so the four
// data-action="hideModal:<id>" buttons in test-orders.html (history and
// receipt modals: × and Close/Cancel) close their modal.
func TestPinWire_W1_CoreTestOrdersHideModalMapped(t *testing.T) {
	js := pinReadFile(t, "static/pages/test-orders.js")
	maps := pinDelegateActionsKeys(js)
	if len(maps) != 1 {
		t.Fatalf("test-orders.js: %d delegateActions maps, want 1", len(maps))
	}
	// Scanner self-check: the map parses to known members.
	if len(maps[0]) < 10 || !pinContains(maps[0], "cancelCommand") || !pinContains(maps[0], "viewHistory") {
		t.Fatalf("test-orders.js map parsed as %v; want cancelCommand and viewHistory among its keys", maps[0])
	}
	if !regexp.MustCompile(`(?m)^import \{[^}]*\bhideModal\b[^}]*\} from '/static/app.js';`).MatchString(js) {
		t.Errorf("test-orders.js no longer imports hideModal from app.js")
	}
	if !pinContains(maps[0], "hideModal") {
		t.Errorf("W1: hideModal is missing from test-orders.js's delegateActions map; its four buttons do nothing")
	}
	tmpl := pinReadFile(t, "templates/test-orders.html")
	if n := strings.Count(tmpl, `data-action="hideModal:`); n != 4 {
		t.Errorf("test-orders.html has %d data-action=\"hideModal:…\" buttons, want 4", n)
	}
}

// W4. The Dashboard's Active Orders card is live: the card is one partial,
// rendered by the page and by GET /dashboard-active-orders (public, beside
// "/"), and dashboard-landing.js swaps it in on order-update on a 1500 ms
// debounce, never in a hidden tab, with one catch-up when the tab is shown,
// re-arming the wait clocks after each swap and setting the strip's count
// from the same answer. app.js (on every page) still subscribes to
// system-status only: RM4 empties that handler's body and keeps the
// subscription.
func TestPinWire_W4_DashboardActiveOrdersLive(t *testing.T) {
	page := pinReadFile(t, "templates/dashboard.html")
	if !strings.Contains(page, `src="/static/pages/dashboard-landing.js`) {
		t.Fatalf("dashboard.html no longer loads dashboard-landing.js; repoint this pin")
	}
	if !strings.Contains(page, `{{template "dashboard-active-orders" .}}`) {
		t.Errorf("W4: dashboard.html does not render the dashboard-active-orders partial")
	}
	if !strings.Contains(page, `id="cs-orders-val">{{.TotalOrders}}</span><span class="cs-lbl">Active orders</span>`) {
		t.Errorf("W4: the strip's Active orders figure lost its id; the swap cannot keep it in step with the card")
	}
	if strings.Contains(page, "<h3>Active Orders</h3>") {
		t.Errorf("W4: the card is still inline in dashboard.html as well as in the partial")
	}
	part := pinReadFile(t, "templates/partials/dashboard-active-orders.html")
	if !strings.Contains(part, `{{define "dashboard-active-orders"}}`) || !strings.Contains(part, "<h3>Active Orders</h3>") ||
		!strings.Contains(part, "No active orders.") || strings.Count(part, `id="dash-active-orders"`) != 2 {
		t.Errorf("W4: partials/dashboard-active-orders.html is not the card (both branches carrying id=dash-active-orders)")
	}

	router := pinReadFile(t, "router.go")
	if !regexp.MustCompile(`r\.Get\("/", h\.handleDashboard\)\n(?:\s*//[^\n]*\n)*\s*r\.Get\("/dashboard-active-orders", h\.handleDashboardActiveOrders\)`).MatchString(router) {
		t.Errorf("W4: GET /dashboard-active-orders is not registered beside \"/\" in the public group")
	}

	js := pinReadFile(t, "static/pages/dashboard-landing.js")
	for _, s := range []string{
		`onSSE('order-update', debounceMaxWait(refreshActiveOrders, 1500, 10000))`,
		`fetch('/dashboard-active-orders'`,
		`r.headers.get('X-Active-Orders-Count')`,
		`if (document.hidden) { ordersMissedWhileHidden = true; return; }`,
		`'visibilitychange'`,
	} {
		if !strings.Contains(js, s) {
			t.Errorf("W4: dashboard-landing.js lacks %q", s)
		}
	}
	// installLiveDurations runs at load and again after each swap.
	if n := strings.Count(js, "installLiveDurations();"); n < 2 {
		t.Errorf("W4: installLiveDurations() called %d times in dashboard-landing.js, want the load call and the post-swap call", n)
	}

	app := pinReadFile(t, "static/app.js")
	subs := regexp.MustCompile(`onSSE\(\s*'([\w-]+)'`).FindAllStringSubmatch(app, -1)
	if len(subs) != 1 || subs[0][1] != "system-status" {
		t.Errorf("app.js onSSE subscriptions = %v, want exactly [system-status]", subs)
	}
}

// W5. Enter in "New Node Group" (nodes.html ngrp-name) and "Add Lane"
// (lane-name) submits: nodes-supermarket.js maps enterSubmits, and enterSubmits
// finds its target in the page's delegateActions map instead of on window,
// where no module function lives. The behaviour is run by
// static/app.entersubmits.test.js (TestAppEnterSubmitsJS).
func TestPinWire_W5_EnterSubmitsMappedAndMapResolved(t *testing.T) {
	tmpl := pinReadFile(t, "templates/nodes.html")
	for _, want := range []string{
		`id="ngrp-name" placeholder="e.g. GRP-ZONE-A" data-action-keydown="enterSubmits:createNodeGroup"`,
		`id="lane-name" placeholder="e.g. LAN-01" data-action-keydown="enterSubmits:submitAddLane"`,
	} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("nodes.html lost %q; repoint this pin", want)
		}
	}

	var mapped []string
	total := 0
	err := filepath.Walk("static", func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".test.js") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, keys := range pinDelegateActionsKeys(string(b)) {
			total += len(keys)
			if pinContains(keys, "enterSubmits") {
				mapped = append(mapped, filepath.ToSlash(p))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Scanner self-check: Core registers well over a hundred map entries.
	if total < 100 {
		t.Fatalf("parsed %d delegateActions keys under static/, want well over a hundred; the scanner is broken", total)
	}
	if len(mapped) != 1 || mapped[0] != "static/pages/nodes-supermarket.js" {
		t.Errorf("W5: enterSubmits is mapped in %v, want [static/pages/nodes-supermarket.js]", mapped)
	}
	smkt := pinReadFile(t, "static/pages/nodes-supermarket.js")
	for _, fn := range []string{"createNodeGroup", "submitAddLane"} {
		if !regexp.MustCompile(`(?m)^    ` + fn + `,?$`).MatchString(smkt) {
			t.Errorf("W5: %s is not in nodes-supermarket.js's map; enterSubmits has nothing to call", fn)
		}
		if strings.Contains(smkt, "window."+fn) {
			t.Errorf("nodes-supermarket.js assigns window.%s; W5 resolves through the map, not window", fn)
		}
	}

	app := pinReadFile(t, "static/app.js")
	body := regexp.MustCompile(`(?s)export function enterSubmits\(targetFnName, el, evt\) \{(.*?)\n\}`).FindStringSubmatch(app)
	if body == nil {
		t.Fatalf("app.js: enterSubmits not found; repoint this pin")
	}
	if strings.Contains(body[1], "window[targetFnName]") {
		t.Errorf("W5: enterSubmits still resolves its target on window:\n%s", body[1])
	}
	if !strings.Contains(body[1], "__delegateActionsMap_delegated") {
		t.Errorf("W5: enterSubmits does not resolve its target through the page's delegateActions map:\n%s", body[1])
	}
}
