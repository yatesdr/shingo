package www

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io/fs"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/store/orders"
)

// TestOrderRouteRuleMatchesThePopup: the route is drawn on three surfaces —
// the boards' order-route partial (templates/partials/orders-rows.html, also
// the Dashboard's Active Orders card), the task board's rowHTML
// (static/pages/dashboard.js) and the order pop-up (static/pages/orders.js),
// whose hero draws the same strip and whose timeline lists every stop. The two
// scripts draw through static/components/route-strip.js. The rule is
// domain.RouteStops and domain.RouteViewOf, server side: the scripts and the
// partial lay out what they are sent and derive nothing. This feeds all of them
// the same orders and compares the strip's cells, the strip's title and the
// timeline's stops with the rule's.
func TestOrderRouteRuleMatchesThePopup(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	const r1 = `[{"action":"wait","node":"PLN_012","wait_kind":"station"},{"action":"pickup","node":"PLN_012"},` +
		`{"action":"dropoff","node":"ULN_002"},{"action":"pickup","node":"SMN_BUF_100","empty":true},{"action":"dropoff","node":"PLN_008"}]`
	const single = `[{"action":"pickup","node":"SMN_01"},{"action":"dropoff","node":"IN_STG"},` +
		`{"action":"wait","node":"ALN_001"},{"action":"pickup","node":"ALN_001"},{"action":"dropoff","node":"OUT_STG"},` +
		`{"action":"pickup","node":"IN_STG"},{"action":"dropoff","node":"ALN_001"},{"action":"pickup","node":"OUT_STG"},` +
		`{"action":"dropoff","node":"ULN_001"}]`
	cases := []struct {
		name  string
		order orders.Order
		strip string
	}{
		{"press-index R1 parked for RELEASE", orders.Order{StepsJSON: r1, Status: protocol.StatusStaged, StepsDone: 1,
			SourceNode: "PLN_012", ProcessNode: "PLN_012", DeliveryNode: "PLN_008"},
			"here:PLN_012 next:ULN_002 remaining:+1 final:PLN_008"},
		{"press-index R1 released, at the outbound", orders.Order{StepsJSON: r1, Status: protocol.StatusInTransit, WaitIndex: 1, StepsDone: 3},
			"passed:PLN_012 ➜ next:SMN_BUF_100 final:PLN_008"},
		{"single_robot, queued", orders.Order{StepsJSON: single, Status: protocol.StatusQueued},
			"next:SMN_01 remaining:+6 final:ULN_001"},
		{"single_robot parked at the line", orders.Order{StepsJSON: single, Status: protocol.StatusStaged, StepsDone: 3},
			"passed:SMN_01 here:ALN_001 next:OUT_STG remaining:+3 final:ULN_001"},
		{"single_robot released, the old bin lifted again at outbound staging",
			orders.Order{StepsJSON: single, Status: protocol.StatusInTransit, WaitIndex: 1, StepsDone: 8},
			"passed:SMN_01 ➜ next:ULN_001"},
		{"single_robot delivered", orders.Order{StepsJSON: single, Status: protocol.StatusDelivered},
			"passed:SMN_01 here:ULN_001"},
		{"plain, lifted", orders.Order{SourceNode: "UTN_014", DeliveryNode: "UTN_013", Status: protocol.StatusInTransit, StepsDone: 1},
			"left:UTN_014 ➜ next:UTN_013"},
		{"unreadable steps", orders.Order{StepsJSON: `[{"action":`, SourceNode: "UTN_014", DeliveryNode: "UTN_013"},
			"next:UTN_014 final:UTN_013"},
		{"all three", orders.Order{SourceNode: "UTN_014", ProcessNode: "ALN_003", DeliveryNode: "UTN_013"},
			"next:UTN_014 remaining:+1 final:UTN_013"},
		{"line repeats From", orders.Order{SourceNode: "UTN_014", ProcessNode: "UTN_014", DeliveryNode: "UTN_013"},
			"next:UTN_014 final:UTN_013"},
		{"From not chosen", orders.Order{ProcessNode: "ALN_003", DeliveryNode: "UTN_013"},
			"next:— remaining:+1 final:UTN_013"},
		{"To not chosen", orders.Order{SourceNode: "UTN_014", ProcessNode: "ALN_003"},
			"next:UTN_014 remaining:+1 final:—"},
		{"nothing chosen", orders.Order{}, "next:— final:—"},
		{"round trip", orders.Order{SourceNode: "UTN_014", ProcessNode: "ALN_003", DeliveryNode: "UTN_014"},
			"next:UTN_014 remaining:+1 final:UTN_014"},
	}

	// The rule, as the server sends it to the scripts: the stops and the view.
	type sent struct {
		Route     []domain.RouteStop `json:"route"`
		RouteView domain.RouteView   `json:"route_view"`
	}
	rule := make([]sent, len(cases))
	for i := range cases {
		stops := domain.RouteStops(&cases[i].order)
		rule[i] = sent{stops, domain.RouteViewOf(stops, cases[i].order.Status)}
		if got := viewSequence(rule[i].RouteView); got != cases[i].strip {
			t.Errorf("%s: the rule's strip reads %s, want %s", cases[i].name, got, cases[i].strip)
		}
	}
	sentJSON, err := json.Marshal(rule)
	testutil.MustNoErr(t, err, "marshal the rule")

	// The boards' partial.
	base := template.Must(template.New("").Funcs(templateFuncs(nil)).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	board := make([]string, len(cases))
	for i := range cases {
		var buf bytes.Buffer
		if err := base.ExecuteTemplate(&buf, "order-route", &cases[i].order); err != nil {
			t.Fatalf("execute order-route: %v", err)
		}
		board[i] = buf.String()
	}

	component := staticJSFunction(t, "static/components/route-strip.js", "routeStateLabel") + "\n" +
		staticJSFunction(t, "static/components/route-strip.js", "routeStripHTML") + "\n" +
		staticJSFunction(t, "static/components/route-strip.js", "routeTimelineHTML") + "\n" +
		"var escapeHtml = function (s) { return String(s); };\n"

	// The task board's row: its route cell.
	task := runRouteJS(t, nodePath, component+
		"var statusClass = function () { return ''; }, statusLabel = function (s) { return s; }, formatETA = function () { return ''; };\n"+
		staticJSFunction(t, "static/pages/dashboard.js", "rowHTML")+"\n"+
		"console.log(JSON.stringify("+string(sentJSON)+".map(function (o) {"+
		" var m = rowHTML(o).match(/<td class=\"r-route\">(.*?)<\\/td>/); return m ? m[1] : ''; })));")

	// The pop-up: its hero's strip and its timeline.
	hero := runRouteJS(t, nodePath, component+
		staticJSFunction(t, "static/pages/orders.js", "routeHeroHTML")+"\n"+
		"console.log(JSON.stringify("+string(sentJSON)+".map(function (o) { return routeHeroHTML(o); })));")
	timeline := runRouteJS(t, nodePath, component+
		"var nodeLink = function (n) { return '<a>' + n + '</a>'; };\n"+
		"var NOT_ASSIGNED = '<span>not assigned yet</span>';\n"+
		"console.log(JSON.stringify("+string(sentJSON)+".map(function (o) { return routeTimelineHTML(o.route, nodeLink, NOT_ASSIGNED); })));")

	for i, c := range cases {
		wantTitle := titleOf(rule[i].Route)
		for _, s := range []struct{ where, html string }{{"board", board[i]}, {"task board", task[i]}, {"pop-up hero", hero[i]}} {
			if got := stripCells(s.html); got != c.strip {
				t.Errorf("%s: the %s strip reads %s, the rule %s", c.name, s.where, got, c.strip)
			}
			if got := stripTitle(t, s.html); got != wantTitle {
				t.Errorf("%s: the %s strip's title reads %q, want %q", c.name, s.where, got, wantTitle)
			}
		}
		if got, want := timelineCells(timeline[i]), stopsSequence(rule[i].Route); got != want {
			t.Errorf("%s: the pop-up timeline reads %s, the rule %s", c.name, got, want)
		}
		if strings.Contains(stopsSequence(rule[i].Route), "left:") && !strings.Contains(strings.Join(labelsOf(timeline[i]), ","), "left") {
			t.Errorf("%s: the pop-up timeline does not say which stop the robot left: %s", c.name, timeline[i])
		}
	}
}

// runRouteJS runs a script under node and decodes the JSON array of strings it
// prints.
func runRouteJS(t *testing.T, nodePath, script string) []string {
	t.Helper()
	cmd := exec.Command(nodePath, "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var html []string
	if err := json.Unmarshal(out, &html); err != nil {
		t.Fatalf("decode node output %q: %v", out, err)
	}
	return html
}

// staticJSFunction is one function's source from a static script.
func staticJSFunction(t *testing.T, file, name string) string {
	t.Helper()
	src, err := fs.ReadFile(staticFS, file)
	if err != nil {
		t.Fatal(err)
	}
	fn := jsFunction(string(src), name)
	if fn == "" {
		t.Fatalf("%s no longer defines %s — move this guard with it", file, name)
	}
	return fn
}

// viewSequence is the rule's strip: its beats left to right, a stop as
// state:node (an unchosen end as —), the stops between the next and the final
// as remaining:+n, and the robot's arrow as ➜ before the stop it heads to.
func viewSequence(v domain.RouteView) string {
	var parts []string
	beat := func(b domain.RouteBeat, s string) {
		if v.Heading == b {
			parts = append(parts, "➜")
		}
		parts = append(parts, s)
	}
	beat(domain.BeatOrigin, cellText(v.Origin))
	if v.Here != nil {
		parts = append(parts, cellText(*v.Here))
	}
	if v.Next != nil {
		beat(domain.BeatNext, cellText(*v.Next))
	}
	if v.Remaining > 0 {
		parts = append(parts, "remaining:+"+strconv.Itoa(v.Remaining))
	}
	if v.Final != nil {
		beat(domain.BeatFinal, cellText(*v.Final))
	}
	return strings.Join(parts, " ")
}

func cellText(s domain.RouteStop) string {
	if s.Unassigned {
		return string(s.State) + ":—"
	}
	return string(s.State) + ":" + s.Node
}

// stopsSequence is every stop of the rule's route as state:node.
func stopsSequence(stops []domain.RouteStop) string {
	parts := make([]string, len(stops))
	for i, s := range stops {
		parts[i] = cellText(s)
	}
	return strings.Join(parts, " ")
}

// titleOf is the strip's title as every surface must write it: each stop by
// name ("not assigned yet" for an end not chosen), its state in words after
// it unless it is still to come, "→" between.
func titleOf(stops []domain.RouteStop) string {
	words := map[domain.RouteState]string{domain.RoutePassed: "passed", domain.RouteLeft: "left",
		domain.RouteHere: "robot here", domain.RouteNext: "next", domain.RouteFinal: "final"}
	parts := make([]string, len(stops))
	for i, s := range stops {
		parts[i] = s.Node
		if s.Unassigned {
			parts[i] = "not assigned yet"
		}
		if w := words[s.State]; w != "" {
			parts[i] += " (" + w + ")"
		}
	}
	return strings.Join(parts, " → ")
}

var (
	stripCellRe    = regexp.MustCompile(`<span class="route-(?:cell (?:route-chip )?route-([a-z]+)[^"]*|sep (route-heading))"[^>]*>([^<]*)</span>`)
	stripTitleRe   = regexp.MustCompile(`<span class="route-strip" title="([^"]*)"`)
	timelineCellRe = regexp.MustCompile(`(?s)<li class="route-cell route-([a-z]+)[^"]*"[^>]*>(.*?)</li>`)
	timelineNodeRe = regexp.MustCompile(`(?s)<span class="route-node">(.*?)</span>`)
	timelineLabel  = regexp.MustCompile(`<span class="route-label">([^<]*)</span>`)
	entityText     = strings.NewReplacer("&#43;", "+", "&mdash;", "—", "&rarr;", "→", "&#8594;", "→",
		"&#39;", "'", "&#34;", `"`, "&amp;", "&")
)

// stripCells reduces a drawn strip to its cells and its arrow as viewSequence
// spells them.
func stripCells(html string) string {
	var parts []string
	for _, m := range stripCellRe.FindAllStringSubmatch(html, -1) {
		if m[2] != "" {
			parts = append(parts, "➜")
			continue
		}
		parts = append(parts, m[1]+":"+strings.TrimSpace(entityText.Replace(m[3])))
	}
	return strings.Join(parts, " ")
}

// stripTitle is the strip's title, the whole route, unescaped.
func stripTitle(t *testing.T, html string) string {
	t.Helper()
	m := stripTitleRe.FindStringSubmatch(html)
	if m == nil {
		t.Errorf("no route-strip title in %q", html)
		return ""
	}
	return entityText.Replace(m[1])
}

// timelineCells reduces the pop-up's timeline to its stops as stopsSequence
// spells them.
func timelineCells(html string) string {
	var parts []string
	for _, m := range timelineCellRe.FindAllStringSubmatch(html, -1) {
		node := m[2]
		if n := timelineNodeRe.FindStringSubmatch(m[2]); n != nil {
			node = n[1]
		}
		node = visibleRouteText(node)
		if node == "not assigned yet" {
			node = "—"
		}
		parts = append(parts, m[1]+":"+node)
	}
	return strings.Join(parts, " ")
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

func labelsOf(html string) []string {
	var out []string
	for _, m := range timelineLabel.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestDashboardActiveOrders_DrawsTheBoardsCompactTable: the Dashboard's Active
// Orders card draws the orders board's rows (the same order-route partial and
// columns), so it draws them at the board's density. At the default table
// density its route strip and timestamps, each kept to one line, ran past the
// card at 1440 px; table-compact is the board's own answer to the same
// columns. The robot id is one token there as on the board.
func TestDashboardActiveOrders_DrawsTheBoardsCompactTable(t *testing.T) {
	src, err := fs.ReadFile(templateFS, "templates/partials/dashboard-active-orders.html")
	testutil.MustNoErr(t, err, "read the Active Orders partial")
	part := string(src)
	if !strings.Contains(part, `<table class="table-compact">`) {
		t.Error("the Active Orders table is not drawn table-compact, the density the orders board draws the same columns at")
	}
	if !strings.Contains(part, `<td class="orders-robot">{{.RobotID}}</td>`) {
		t.Error("the Active Orders robot cell is not orders-robot: the id wraps mid-token as the board's does not")
	}
}
