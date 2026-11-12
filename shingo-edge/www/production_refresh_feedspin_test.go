package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// production_refresh_feedspin_test.go — which event refreshes what on the
// Production page, and which page opens /events with which URL. A source-shape
// pin: the page's script runs too much DOM at load to drive under node without
// a browser, so the triggers are read from the markup and the script.
//
// Production holds one /events stream, the page's bus (shared/bus.js):
// production.js subscribes the shift chart and the manual-order node lists,
// and the hub's refreshOnEvents fires the hx-trigger events on
// #production-content (htmx.trigger). production.html opens no stream of its
// own (no sse-connect, no hx-ext="sse"). Diagnostics asks the bus for
// debug-log (?debug=1); Manual Order subscribes core-nodes.
//
// Before "perf(edge): sourcing writes only what changed, status shows the Core
// link, one stream per page" htmx's sse extension opened a second stream for
// the content refresh and Diagnostics opened plain /events. Every Edge admin
// page's one stream is TestEdgeAdmin_OneBus's.

func readWWWFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(b)
}

func TestProductionRefreshTriggers_FeedsPin(t *testing.T) {
	t.Parallel()
	html := readWWWFile(t, "templates", "production.html")
	js := readWWWFile(t, "static", "js", "pages", "production.js")

	m := regexp.MustCompile(`(?s)<div id="production-content"(.*?)>`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("#production-content not found in production.html")
	}
	attrs := m[1]
	trig := regexp.MustCompile(`hx-trigger="([^"]*)"`).FindStringSubmatch(attrs)
	if trig == nil {
		t.Fatal("#production-content has no hx-trigger")
	}
	var triggers []string
	for _, p := range strings.Split(trig[1], ",") {
		triggers = append(triggers, strings.TrimSpace(p))
	}

	// What the content refresh listens for. The list once also held
	// sse:order-completed, which nothing sends: the Edge broadcasts
	// EventOrderCompleted as order-update (www/sse.go). It went in the change
	// the header names; a completion refreshes through order-update.
	wantTriggers := []string{
		"refreshProduction from:body",
		"refreshMaterial from:body",
		"sse:order-update throttle:5s",
		"sse:order-failed",
		"sse:counter-update throttle:10s",
	}
	if strings.Join(triggers, "|") != strings.Join(wantTriggers, "|") {
		t.Errorf("hx-trigger = %q\nwant %q", triggers, wantTriggers)
	}

	busHandlers := regexp.MustCompile(`(?m)^onSSE\('([a-z-]+)'`).FindAllStringSubmatch(js, -1)
	var handlers []string
	for _, h := range busHandlers {
		handlers = append(handlers, h[1])
	}
	hub := readWWWFile(t, "static", "js", "shingoedge.js")

	cases := []struct {
		name string
		got  any
		want any
	}{
		// The next two were true.
		{"production.html sse-connect=\"/events\"", strings.Contains(attrs, `sse-connect="/events"`), false},
		{"production.html hx-ext=\"sse\"", strings.Contains(attrs, `hx-ext="sse"`), false},
		// createSSE('/events', …), once, is gone: the page's one stream is the bus.
		{"production.js createSSE", strings.Count(js, "createSSE("), 0},
		{"production.js refreshOnEvents('production-content', onSSE)", strings.Count(js, "refreshOnEvents('production-content', onSSE)"), 1},
		// The page's own handlers; the order-update / order-failed /
		// counter-update refreshes are refreshOnEvents', subscribed first
		// (TestProductionRefresh_OneStream, shingoedge.refresh.test.js).
		{"production.js bus handlers", strings.Join(handlers, ","), "counter-update,core-nodes"},
		// production.js called htmx.trigger itself; the hub's verb does now.
		{"shingoedge.js refreshOnEvents calls htmx.trigger", strings.Contains(hub, "window.htmx.trigger(target, 'sse:' + type, data)"), true},
		// Was true: Diagnostics opens /events?debug=1, on the bus now.
		{"diagnostics.js asks the bus for debug-log", strings.Contains(readWWWFile(t, "static", "js", "pages", "diagnostics.js"), "setSSEQuery({ debug: 1 });"), true},
		{"manual-order.js subscribes core-nodes", strings.Contains(readWWWFile(t, "static", "js", "pages", "manual-order.js"), "onSSE('core-nodes', function() {"), true},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
