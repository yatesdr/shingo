package www

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestProductionRefreshJS runs static/js/pages/production-refresh.test.js
// under node: the Production page's one /events stream fires each of
// #production-content's sse: triggers and still reaches the page's own
// handlers (C2).
func TestProductionRefreshJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the production refresh JS tests")
	}
	script := filepath.Join("static", "js", "pages", "production-refresh.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("production refresh JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("production refresh JS tests: %s", out)
}

// TestProductionRefresh_OneStream holds the three files together: the
// hx-trigger's sse: names are exactly the events production-refresh.js
// forwards, the page opens one /events stream and builds its handlers through
// productionStreamHandlers, and the element opens no stream of its own. If the
// lists drift, a refresh silently stops.
func TestProductionRefresh_OneStream(t *testing.T) {
	html := readWWWFile(t, "templates", "production.html")
	js := readWWWFile(t, "static", "js", "pages", "production.js")
	mod := readWWWFile(t, "static", "js", "pages", "production-refresh.js")

	trig := regexp.MustCompile(`(?s)id="production-content".*?hx-trigger="([^"]*)"`).FindStringSubmatch(html)
	if trig == nil {
		t.Fatal("#production-content hx-trigger not found")
	}
	var sseNames []string
	for _, p := range strings.Split(trig[1], ",") {
		if name, ok := strings.CutPrefix(strings.Fields(p)[0], "sse:"); ok {
			sseNames = append(sseNames, name)
		}
	}
	list := regexp.MustCompile(`PRODUCTION_REFRESH_EVENTS = \[([^\]]*)\]`).FindStringSubmatch(mod)
	if list == nil {
		t.Fatal("PRODUCTION_REFRESH_EVENTS not found in production-refresh.js")
	}
	var forwarded []string
	for _, q := range strings.Split(list[1], ",") {
		forwarded = append(forwarded, strings.Trim(strings.TrimSpace(q), `'`))
	}
	if strings.Join(sseNames, ",") != strings.Join(forwarded, ",") {
		t.Errorf("hx-trigger sse: names %v != forwarded events %v", sseNames, forwarded)
	}

	if strings.Contains(html, "sse-connect") || strings.Contains(html, `hx-ext="sse"`) {
		t.Error("production.html opens its own /events stream again (sse-connect / hx-ext=\"sse\")")
	}
	if n := strings.Count(js, "createSSE("); n != 1 {
		t.Errorf("production.js calls createSSE %d times, want 1", n)
	}
	if !strings.Contains(js, "createSSE('/events', productionStreamHandlers(refreshProductionContent, {") {
		t.Error("production.js's stream no longer builds its handlers through productionStreamHandlers")
	}
	if !strings.Contains(js, "import { productionStreamHandlers } from '/static/js/pages/production-refresh.js';") {
		t.Error("production.js does not import productionStreamHandlers")
	}
}
