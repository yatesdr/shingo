package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pin_wire_test.go — P0 pins for lane W (ui-cleanup, 2026-10-07), Core side.
//
// Each test pins a control that is built and one connection short, AS DEAD, at
// 5c0beb74. The W unit wires each one and flips the matching assertion; the
// predicted after-value of every case is in the evidence folder
// (predictions/p0-wrm.md). Nothing here is a guard to keep: when the W unit
// lands, each of these is rewritten to the wired state or deleted with it.

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

// W1 (Core). test-orders.js imports hideModal and leaves it out of its one
// delegateActions map, so the four data-action="hideModal:<id>" buttons in
// test-orders.html (history and receipt modals: × and Close/Cancel) do nothing.
func TestPinWire_W1_CoreTestOrdersHideModalUnmapped(t *testing.T) {
	js := pinReadFile(t, "static/pages/test-orders.js")
	maps := pinDelegateActionsKeys(js)
	if len(maps) != 1 {
		t.Fatalf("test-orders.js: %d delegateActions maps, want 1", len(maps))
	}
	// Scanner self-check: the map parses to its known size and members.
	if len(maps[0]) < 28 || !pinContains(maps[0], "cancelCommand") || !pinContains(maps[0], "viewHistory") {
		t.Fatalf("test-orders.js map parsed as %v; want at least 28 keys incl. cancelCommand and viewHistory", maps[0])
	}
	if !regexp.MustCompile(`(?m)^import \{[^}]*\bhideModal\b[^}]*\} from '/static/app.js';`).MatchString(js) {
		t.Errorf("test-orders.js no longer imports hideModal from app.js")
	}
	if pinContains(maps[0], "hideModal") {
		t.Errorf("W1 NOW: hideModal is in test-orders.js's delegateActions map — the W unit flips this pin")
	}
	tmpl := pinReadFile(t, "templates/test-orders.html")
	if n := strings.Count(tmpl, `data-action="hideModal:`); n != 4 {
		t.Errorf("test-orders.html has %d data-action=\"hideModal:…\" buttons, want 4", n)
	}
}

// W4. The Dashboard's Active Orders card is server-rendered once and never
// updates: dashboard-landing.js (the page's only module) subscribes to no SSE
// topic, and app.js (on every page) subscribes to system-status only.
func TestPinWire_W4_DashboardActiveOrdersHasNoSSE(t *testing.T) {
	page := pinReadFile(t, "templates/dashboard.html")
	if !strings.Contains(page, `src="/static/pages/dashboard-landing.js`) {
		t.Fatalf("dashboard.html no longer loads dashboard-landing.js; repoint this pin")
	}
	// The card's heading may move into a partial (W4 cuts the card out); look
	// in the page and the partials.
	card := page
	if parts, _ := filepath.Glob(filepath.Join("templates", "partials", "*.html")); len(parts) > 0 {
		for _, p := range parts {
			card += pinReadFile(t, filepath.ToSlash(p))
		}
	}
	if !strings.Contains(card, "<h3>Active Orders</h3>") {
		t.Fatalf("no Active Orders card in dashboard.html or templates/partials; repoint this pin")
	}
	js := pinReadFile(t, "static/pages/dashboard-landing.js")
	for _, s := range []string{"onSSE", "order-update", "EventSource", "createSSE"} {
		if strings.Contains(js, s) {
			t.Errorf("W4 NOW: dashboard-landing.js names %q — the W unit flips this pin", s)
		}
	}
	app := pinReadFile(t, "static/app.js")
	subs := regexp.MustCompile(`onSSE\(\s*'([\w-]+)'`).FindAllStringSubmatch(app, -1)
	if len(subs) != 1 || subs[0][1] != "system-status" {
		t.Errorf("app.js onSSE subscriptions = %v, want exactly [system-status]", subs)
	}
}

// W5. Enter in "New Node Group" (nodes.html ngrp-name) and "Add Lane"
// (lane-name) does nothing: enterSubmits is in no delegateActions map, and its
// body resolves the target on window, where no module function lives.
func TestPinWire_W5_EnterSubmitsUnmappedAndWindowResolved(t *testing.T) {
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
	// Scanner self-check: Core registers a few hundred map entries.
	if total < 200 {
		t.Fatalf("parsed %d delegateActions keys under static/, want hundreds; the scanner is broken", total)
	}
	if len(mapped) != 0 {
		t.Errorf("W5 NOW: enterSubmits is in a delegateActions map in %v — the W unit flips this pin", mapped)
	}

	app := pinReadFile(t, "static/app.js")
	body := regexp.MustCompile(`(?s)export function enterSubmits\(targetFnName, el, evt\) \{(.*?)\n\}`).FindStringSubmatch(app)
	if body == nil {
		t.Fatalf("app.js: enterSubmits not found; repoint this pin")
	}
	if !strings.Contains(body[1], "window[targetFnName]") {
		t.Errorf("W5 NOW: enterSubmits no longer resolves its target on window — the W unit flips this pin:\n%s", body[1])
	}
	for _, fn := range []string{"createNodeGroup", "submitAddLane"} {
		if strings.Contains(pinReadFile(t, "static/pages/nodes-supermarket.js"), "window."+fn) {
			t.Errorf("nodes-supermarket.js assigns window.%s; the premise of W5 is gone", fn)
		}
	}
}
