package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// production_refresh_feedspin_test.go — which event refreshes what on the
// Production page, and which page opens /events with which URL.
//
// Production holds two /events streams today: htmx's sse extension
// (sse-connect on #production-content) drives the content refresh through
// hx-trigger, and production.js's createSSE('/events') drives the shift chart
// and the manual-order node lists. A source-shape pin: the page's script runs
// too much DOM at load to drive under node without a browser, so the triggers
// are read from the markup and the script.
//
// after (C2): one stream. No sse-connect or hx-ext="sse" on production.html;
// production.js's createSSE dispatches the events the hx-trigger lists
// (htmx.trigger), so the same refreshes fire from it. The hx-trigger list
// itself is unchanged in meaning.
//
// after (C1): Diagnostics opens /events?debug=1; the other pages keep /events.

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

	// What the content refresh listens for. The base also listed
	// sse:order-completed, which nothing sends: the Edge broadcasts
	// EventOrderCompleted as order-update (www/sse.go). It is gone (flipped in
	// the edge costs commit); a completion refreshes through order-update.
	wantTriggers := []string{
		"refreshProduction from:body",
		"refreshMaterial from:body",
		"sse:order-update throttle:5s",
		"sse:order-failed",
		"sse:counter-update throttle:10s",
	}
	// after: the same refreshes, fed from production.js's one stream (C2). If
	// C2 renames the sse: prefixed triggers to plain events dispatched by
	// htmx.trigger, the list changes in spelling only.
	if strings.Join(triggers, "|") != strings.Join(wantTriggers, "|") {
		t.Errorf("hx-trigger = %q\nwant %q (after: same refreshes from one stream, C2)", triggers, wantTriggers)
	}

	createSSEHandlers := regexp.MustCompile(`\n    (on[A-Z][A-Za-z]*): function`).FindAllStringSubmatch(js, -1)
	var handlers []string
	for _, h := range createSSEHandlers {
		handlers = append(handlers, h[1])
	}

	cases := []struct {
		name  string
		got   any
		want  any
		after any
		label string
	}{
		// C2: the next two were true.
		{"production.html sse-connect=\"/events\"", strings.Contains(attrs, `sse-connect="/events"`), false, false, "C2"},
		{"production.html hx-ext=\"sse\"", strings.Contains(attrs, `hx-ext="sse"`), false, false, "C2"},
		{"production.js createSSE('/events'", strings.Count(js, "createSSE('/events'"), 1, 1, "C2"},
		// C2, prediction corrected: the page's own handlers are unchanged; the
		// order-update / order-failed / counter-update
		// forwarding is added around them by productionStreamHandlers
		// (production-refresh.js, tested in production-refresh.test.js).
		{"production.js createSSE handlers", strings.Join(handlers, ","), "onCounterUpdate,onCoreNodes",
			"onCounterUpdate,onCoreNodes (forwarding wrapped around them by production-refresh.js)", "C2"},
		// C2: was false.
		{"production.js calls htmx.trigger", strings.Contains(js, "htmx.trigger("), true, true, "C2"},
		// C1: was true.
		{"diagnostics.js opens /events (no debug)", strings.Contains(readWWWFile(t, "static", "js", "pages", "diagnostics.js"), "createSSE('/events',"), false, false, "C1 (opens /events?debug=1)"},
		{"manual-order.js opens /events", strings.Contains(readWWWFile(t, "static", "js", "pages", "manual-order.js"), "createSSE('/events',"), true, true, "C1"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v (after: %v, %s)", c.name, c.got, c.want, c.after, c.label)
		}
	}
}
