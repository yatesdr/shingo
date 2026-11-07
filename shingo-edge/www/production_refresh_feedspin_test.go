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
// Production holds one /events stream: production.js's createSSE('/events')
// drives the shift chart and the manual-order node lists, and fires the
// hx-trigger events on #production-content (htmx.trigger, through
// productionStreamHandlers in production-refresh.js). production.html opens no
// stream of its own (no sse-connect, no hx-ext="sse"). Diagnostics opens
// /events?debug=1; Manual Order keeps /events.
//
// Before "perf(edge): sourcing writes only what changed, status shows the Core
// link, one stream per page" htmx's sse extension opened a second stream for
// the content refresh and Diagnostics opened plain /events. That the
// hx-trigger's sse: names match the forwarded events is
// TestProductionRefresh_OneStream's.

func readWWWFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(b)
}

func TestProductionRefreshTriggers_FeedsPin(t *testing.T) {
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

	createSSEHandlers := regexp.MustCompile(`\n    (on[A-Z][A-Za-z]*): function`).FindAllStringSubmatch(js, -1)
	var handlers []string
	for _, h := range createSSEHandlers {
		handlers = append(handlers, h[1])
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		// The next two were true.
		{"production.html sse-connect=\"/events\"", strings.Contains(attrs, `sse-connect="/events"`), false},
		{"production.html hx-ext=\"sse\"", strings.Contains(attrs, `hx-ext="sse"`), false},
		{"production.js createSSE('/events'", strings.Count(js, "createSSE('/events'"), 1},
		// The page's own handlers; the order-update / order-failed /
		// counter-update forwarding is wrapped around them by
		// productionStreamHandlers (production-refresh.js, tested in
		// production-refresh.test.js).
		{"production.js createSSE handlers", strings.Join(handlers, ","), "onCounterUpdate,onCoreNodes"},
		// Was false.
		{"production.js calls htmx.trigger", strings.Contains(js, "htmx.trigger("), true},
		// Was true: Diagnostics opens /events?debug=1.
		{"diagnostics.js opens /events (no debug)", strings.Contains(readWWWFile(t, "static", "js", "pages", "diagnostics.js"), "createSSE('/events',"), false},
		{"manual-order.js opens /events", strings.Contains(readWWWFile(t, "static", "js", "pages", "manual-order.js"), "createSSE('/events',"), true},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
