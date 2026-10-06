package www

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io/fs"
	"os/exec"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/dashboards"
	"shingocore/store/orders"
)

// TestTemplatesParse mirrors NewRouter's template parsing so a malformed page
// template fails in `go test` rather than panicking (template.Must) at server
// startup. The handler-test fixtures use an empty tmpls map, so without this
// nothing exercises the real parse pipeline.
//
// It also smoke-executes the two chromeless dashboard templates, each by its
// own file name via the renderBare path. This catches a bad field reference or
// template function the parse step alone would not. Pages that render through
// the shared "layout" are executed by phase6_pages_render_test.go's renderPage,
// which is the closer mirror of what router.go does.
func TestTemplatesParse(t *testing.T) {
	base := template.Must(template.New("").Funcs(templateFuncs(nil)).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))

	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	tmpls := map[string]*template.Template{}
	for _, p := range pages {
		name := strings.TrimPrefix(p, "templates/")
		if name == "layout.html" {
			continue
		}
		clone := template.Must(template.Must(base.Clone()).ParseFS(templateFS, p))
		tmpls[name] = clone
	}

	// Chromeless display: executed by its own file name (the renderBare path),
	// with the dashboard config baked in server-side.
	disp, ok := tmpls["dashboard-display.html"]
	if !ok {
		t.Fatal("dashboard-display.html not parsed")
	}
	var buf bytes.Buffer
	d := &dashboards.Dashboard{ID: 7, Name: "North Cell", Kind: "task-board"}
	if err := disp.ExecuteTemplate(&buf, "dashboard-display.html", map[string]any{"Dashboard": d}); err != nil {
		t.Fatalf("execute dashboard-display.html: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "North Cell") || !strings.Contains(out, `data-dashboard-id="7"`) {
		t.Errorf("display output missing baked-in config; got:\n%s", out)
	}

	// Robot-map display: same chromeless (renderBare) path, different kind.
	mp, ok := tmpls["dashboard-map.html"]
	if !ok {
		t.Fatal("dashboard-map.html not parsed")
	}
	buf.Reset()
	dm := &dashboards.Dashboard{ID: 9, Name: "Plant Map", Kind: "robot-map"}
	if err := mp.ExecuteTemplate(&buf, "dashboard-map.html", map[string]any{"Dashboard": dm}); err != nil {
		t.Fatalf("execute dashboard-map.html: %v", err)
	}
	if !strings.Contains(buf.String(), "Plant Map") || !strings.Contains(buf.String(), `data-dashboard-kind="robot-map"`) {
		t.Errorf("map output missing baked-in config; got:\n%s", buf.String())
	}
}

// kioskTemplatesMissingColorScheme lists the standalone (renderBare) templates
// that hard-code a dark theme on <html> but do not tell the browser so. Without
// color-scheme the browser draws its LIGHT scrollbars and form controls on the
// dark page — the bright white bar down a wall display framed on the Dashboard.
func kioskTemplatesMissingColorScheme(t *testing.T) []string {
	t.Helper()
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	dark := 0
	for _, p := range pages {
		b, err := fs.ReadFile(templateFS, p)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, `<html lang="en" data-theme="dark">`) {
			continue
		}
		dark++
		if !strings.Contains(src, `<meta name="color-scheme" content="dark">`) {
			missing = append(missing, strings.TrimPrefix(p, "templates/"))
		}
	}
	if dark == 0 {
		t.Fatal("no dark standalone template found — the scan has drifted from the markup")
	}
	return missing
}

func TestKioskTemplatesDeclareTheirColorScheme(t *testing.T) {
	// Was all four: dashboard-display, dashboard-map, dashboard-node-report and
	// heartbeat drew light scrollbars on a dark page.
	if got := kioskTemplatesMissingColorScheme(t); len(got) != 0 {
		t.Errorf(`dark kiosk templates without <meta name="color-scheme" content="dark">: %v`, got)
	}
}

// TestOrderRouteRuleMatchesThePopup: the route is drawn twice — the board's
// order-route partial (templates/partials/orders-rows.html, server side) and
// the order pop-up's routeHTML (static/pages/orders.js). They differ on
// purpose in dress (the pop-up links each node and says "not assigned yet";
// the board prints a dash), and must not differ in which nodes appear: From,
// then the line node unless it repeats an end, then To. Both are new on the
// core-ui-cleanup branch, and nothing held them together. This feeds both the
// same cases and compares the node sequences.
func TestOrderRouteRuleMatchesThePopup(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	cases := [][3]string{
		{"UTN_014", "ALN_003", "UTN_013"}, // all three
		{"UTN_014", "UTN_014", "UTN_013"}, // line repeats From
		{"UTN_014", "UTN_013", "UTN_013"}, // line repeats To
		{"UTN_014", "", "UTN_013"},        // no line
		{"", "ALN_003", "UTN_013"},        // From not chosen
		{"UTN_014", "ALN_003", ""},        // To not chosen
		{"", "", ""},                      // nothing chosen
		{"UTN_014", "ALN_003", "UTN_014"}, // round trip
	}

	base := template.Must(template.New("").Funcs(templateFuncs(nil)).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	board := make([]string, len(cases))
	for i, c := range cases {
		var buf bytes.Buffer
		o := &orders.Order{SourceNode: c[0], ProcessNode: c[1], DeliveryNode: c[2]}
		if err := base.ExecuteTemplate(&buf, "order-route", o); err != nil {
			t.Fatalf("execute order-route: %v", err)
		}
		board[i] = routeSequence(buf.String(), "→", "—")
	}

	src, err := fs.ReadFile(staticFS, "static/pages/orders.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := jsFunction(string(src), "routeHTML")
	if fn == "" {
		t.Fatal("orders.js no longer defines routeHTML — move this guard with it")
	}
	caseJSON, err := json.Marshal(cases)
	testutil.MustNoErr(t, err, "marshal cases")
	script := "var nodeLink = function (n) { return '<a>' + n + '</a>'; };\n" +
		"var NOT_ASSIGNED = '<span>not assigned yet</span>';\n" + fn + "\n" +
		"console.log(JSON.stringify(" + string(caseJSON) + ".map(function (c) {" +
		" return routeHTML({ source_node: c[0], process_node: c[1], delivery_node: c[2] }); })));"
	cmd := exec.Command(nodePath, "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var popupHTML []string
	if err := json.Unmarshal(out, &popupHTML); err != nil {
		t.Fatalf("decode node output %q: %v", out, err)
	}
	for i, c := range cases {
		popup := routeSequence(popupHTML[i], "&rarr;", "not assigned yet")
		if popup != board[i] {
			t.Errorf("route %q: board %s, pop-up %s", c, board[i], popup)
		}
	}
}

// routeSequence reduces a drawn route to "A > B > C", an unchosen end as "?".
func routeSequence(html, arrow, none string) string {
	var parts []string
	for _, p := range strings.Split(html, arrow) {
		p = visibleRouteText(p)
		if p == none {
			p = "?"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " > ")
}

func visibleRouteText(html string) string {
	var b strings.Builder
	in := false
	for _, r := range html {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// jsFunction is the source of `function name(...) {...}`, braces matched.
func jsFunction(src, name string) string {
	at := strings.Index(src, "function "+name+"(")
	if at < 0 {
		return ""
	}
	depth := 0
	for i := strings.Index(src[at:], "{") + at; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[at : i+1]
			}
		}
	}
	return ""
}
