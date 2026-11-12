package www

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestProductionRefreshJS runs testdata/static/js/shingoedge.refresh.test.js
// under node: the Edge hub's refreshOnEvents turns each sse: name in a
// partial's hx-trigger (Production's, Changeover's, Orders') into one
// subscription on the page's one bus stream, and each frame into one
// htmx.trigger on the element.
func TestProductionRefreshJS(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the refresh-on-events JS tests")
	}
	script := filepath.Join("testdata", "static", "js", "shingoedge.refresh.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("refresh-on-events JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("refresh-on-events JS tests: %s", out)
}

// TestProductionRefresh_OneStream holds the Production page to one stream:
// production.js subscribes on shared/bus.js, #production-content's sse:
// triggers through refreshOnEvents before the page's own handlers (so the
// content refresh fires first on a frame both take, as it did), and the
// element opens no stream of its own.
func TestProductionRefresh_OneStream(t *testing.T) {
	t.Parallel()
	html := readWWWFile(t, "templates", "production.html")
	js := readWWWFile(t, "static", "js", "pages", "production.js")

	if strings.Contains(html, "sse-connect") || strings.Contains(html, `hx-ext="sse"`) {
		t.Error("production.html opens its own /events stream again (sse-connect / hx-ext=\"sse\")")
	}
	if !strings.Contains(js, "import { onSSE } from '/static/shared/bus.js';") {
		t.Error("production.js does not take onSSE from shared/bus.js")
	}
	refresh := strings.Index(js, "refreshOnEvents('production-content', onSSE);")
	if refresh < 0 {
		t.Fatal("production.js no longer refreshes #production-content from the bus (refreshOnEvents)")
	}
	if first := strings.Index(js, "onSSE('"); first >= 0 && first < refresh {
		t.Error("production.js subscribes a handler of its own before refreshOnEvents: the content refresh no longer fires first")
	}
}

// edgeAdminScripts maps each Edge admin template to the page module it loads
// (a template that loads none is left out). The station's templates are not
// admin pages: the station is on the bus as a kiosk (operator-refresh.test.js).
func edgeAdminScripts(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`<script type="module">import "/static/(js/pages/[a-z-]+\.js)";</script>`)
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") || strings.HasPrefix(e.Name(), "operator-") {
			continue
		}
		if m := re.FindStringSubmatch(readWWWFile(t, "templates", e.Name())); m != nil {
			out[e.Name()] = m[1]
		}
	}
	if len(out) < 8 {
		t.Fatalf("found page modules for only %d Edge admin templates; the scan is broken, not the pages", len(out))
	}
	return out
}

// TestEdgeAdmin_OneBus holds Edge admin to the one live client, shared/bus.js
// (ruling 4: both createSSE copies go, the bus is the one client), and to one
// stream per page:
//   - no template connects htmx's sse extension, and htmx-sse.js is gone;
//   - no Edge admin script opens an EventSource or defines createSSE;
//   - every element whose hx-trigger names an sse: event is refreshed by its
//     page's script through refreshOnEvents, on the page's bus;
//   - the hub subscribes no server topic (it runs before the page, so one
//     would open the stream ahead of the page's topics), and subscribes
//     build-changed to the new-build banner once (ruling 4: admin pages show
//     the banner, kiosks reload); a bus frame opens no stream (shared/bus.js),
//     so a page that holds none still holds none;
//   - no page subscribes build-changed itself: the hub's is the one.
func TestEdgeAdmin_OneBus(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(filepath.Join("static", "js", "htmx-sse.js")); err == nil {
		t.Error("static/js/htmx-sse.js is back: the bus is the one SSE client")
	}
	scripts := edgeAdminScripts(t)
	elemRe := regexp.MustCompile(`(?s)<div\b[^>]*>`)
	idRe := regexp.MustCompile(`\bid="([^"]+)"`)
	trigRe := regexp.MustCompile(`\bhx-trigger="([^"]*\bsse:[^"]*)"`)
	for tmpl, script := range scripts {
		html := readWWWFile(t, "templates", tmpl)
		for _, bad := range []string{"sse-connect", `hx-ext="sse"`, "htmx-sse.js"} {
			if strings.Contains(html, bad) {
				t.Errorf("%s carries %s: htmx's sse extension opened a second stream", tmpl, bad)
			}
		}
		js := readWWWFile(t, "static", filepath.FromSlash(script))
		for _, tag := range elemRe.FindAllString(html, -1) {
			if trigRe.MatchString(tag) {
				id := idRe.FindStringSubmatch(tag)
				if id == nil {
					t.Errorf("%s: an element with sse: triggers has no id for refreshOnEvents", tmpl)
					continue
				}
				if !strings.Contains(js, "refreshOnEvents('"+id[1]+"', onSSE)") {
					t.Errorf("%s: #%s refreshes on sse: triggers and %s does not call refreshOnEvents('%s', onSSE)", tmpl, id[1], script, id[1])
				}
			}
		}
	}

	subRe := regexp.MustCompile(`\bonSSE\('([a-z-]+)'`)
	var pages []string
	err := filepath.WalkDir(filepath.Join("static", "js"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".min.js") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		js := string(b)
		rel := filepath.ToSlash(p)
		if strings.Contains(js, "new EventSource(") || regexp.MustCompile(`\bfunction createSSE\b|\bcreateSSE\(`).MatchString(js) {
			t.Errorf("%s opens an EventSource of its own (createSSE / new EventSource): shared/bus.js is the one client", rel)
		}
		server := 0
		for _, m := range subRe.FindAllStringSubmatch(js, -1) {
			switch m[1] {
			case "connected", "disconnected", "heartbeat", "stream-gap", "build-changed":
			default:
				server++
			}
		}
		if rel == "static/js/shingoedge.js" {
			// refreshOnEvents subscribes through the onSSE its page passes;
			// at import the hub subscribes the banner and nothing else.
			if server > 0 {
				t.Errorf("shingoedge.js subscribes a server topic; a page subscribes its own, so the stream opens once")
			}
			if n := len(regexp.MustCompile(`(?m)^onSSE\('build-changed', showNewBuildBanner\);$`).FindAllString(js, -1)); n != 1 {
				t.Errorf("shingoedge.js subscribes build-changed to the new-build banner %d times at import, want 1", n)
			}
			return nil
		}
		if strings.Contains(js, "'build-changed'") {
			t.Errorf("%s subscribes build-changed itself; the Edge hub's subscription shows the banner", rel)
		}
		if server > 0 || strings.Contains(js, "refreshOnEvents(") {
			pages = append(pages, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(pages)
	want := []string{
		"static/js/pages/changeover.js", "static/js/pages/containment.js", "static/js/pages/diagnostics.js",
		"static/js/pages/manual-order.js", "static/js/pages/orders.js", "static/js/pages/production.js",
	}
	if strings.Join(pages, ",") != strings.Join(want, ",") {
		t.Errorf("Edge admin pages on the bus = %v\nwant %v (a page added to or left off the bus says so here)", pages, want)
	}
}
