package www

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io/fs"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/store/orders"
)

// TestOrderRouteRuleMatchesThePopup: the route is drawn on three surfaces —
// the board's order-route partial (templates/partials/orders-rows.html, also
// the Dashboard card), the order pop-up's routeHTML (static/pages/orders.js)
// and the task board's rowHTML (static/pages/dashboard.js). They differ on
// purpose in dress (the pop-up links each node and says "not assigned yet";
// the boards print a dash), and must not differ in which stops appear or
// which is marked current. The rule is domain.RouteStops, server side; the two
// scripts draw what the server sends them and have no rule of their own. This
// feeds all three the same orders and compares their stop sequences with the
// rule's.
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
	robots := stubRobotStations{"AMR-01": "PLN_012", "AMR-02": "ULN_002", "AMR-03": "OUT_STG"}
	cases := []struct {
		name  string
		order orders.Order
		want  string
	}{
		{"press-index R1 parked for RELEASE", orders.Order{StepsJSON: r1, Status: protocol.StatusStaged, RobotID: "AMR-01",
			SourceNode: "PLN_012", ProcessNode: "PLN_012", DeliveryNode: "PLN_008"}, "[PLN_012] > ULN_002 > SMN_BUF_100 > PLN_008"},
		{"press-index R1 released, at the outbound", orders.Order{StepsJSON: r1, Status: protocol.StatusInTransit, WaitIndex: 1, RobotID: "AMR-02"},
			"PLN_012 > [ULN_002] > SMN_BUF_100 > PLN_008"},
		{"single_robot, queued", orders.Order{StepsJSON: single, Status: protocol.StatusQueued},
			"SMN_01 > IN_STG > ALN_001 > OUT_STG > IN_STG > ALN_001 > OUT_STG > ULN_001"},
		{"single_robot parked at the line", orders.Order{StepsJSON: single, Status: protocol.StatusStaged, RobotID: "AMR-03"},
			"SMN_01 > IN_STG > [ALN_001] > OUT_STG > IN_STG > ALN_001 > OUT_STG > ULN_001"},
		{"single_robot released, at outbound staging", orders.Order{StepsJSON: single, Status: protocol.StatusInTransit, WaitIndex: 1, RobotID: "AMR-03"},
			"SMN_01 > IN_STG > ALN_001 > [OUT_STG] > IN_STG > ALN_001 > OUT_STG > ULN_001"},
		{"unreadable steps", orders.Order{StepsJSON: `[{"action":`, SourceNode: "UTN_014", DeliveryNode: "UTN_013"}, "UTN_014 > UTN_013"},
		{"all three", orders.Order{SourceNode: "UTN_014", ProcessNode: "ALN_003", DeliveryNode: "UTN_013"}, "UTN_014 > ALN_003 > UTN_013"},
		{"line repeats From", orders.Order{SourceNode: "UTN_014", ProcessNode: "UTN_014", DeliveryNode: "UTN_013"}, "UTN_014 > UTN_013"},
		{"line repeats To", orders.Order{SourceNode: "UTN_014", ProcessNode: "UTN_013", DeliveryNode: "UTN_013"}, "UTN_014 > UTN_013"},
		{"no line", orders.Order{SourceNode: "UTN_014", DeliveryNode: "UTN_013"}, "UTN_014 > UTN_013"},
		{"From not chosen", orders.Order{ProcessNode: "ALN_003", DeliveryNode: "UTN_013"}, "? > ALN_003 > UTN_013"},
		{"To not chosen", orders.Order{SourceNode: "UTN_014", ProcessNode: "ALN_003"}, "UTN_014 > ALN_003 > ?"},
		{"nothing chosen", orders.Order{}, "? > ?"},
		{"round trip", orders.Order{SourceNode: "UTN_014", ProcessNode: "ALN_003", DeliveryNode: "UTN_014"}, "UTN_014 > ALN_003 > UTN_014"},
	}

	// The rule, as the server sends it to the two scripts.
	stops := make([][]domain.RouteStop, len(cases))
	for i := range cases {
		stops[i] = domain.RouteStops(&cases[i].order, robotStationOf(robots, &cases[i].order))
		if got := stopSequence(stops[i]); got != cases[i].want {
			t.Errorf("%s: the rule reads %s, want %s", cases[i].name, got, cases[i].want)
		}
	}

	// The board's partial.
	base := template.Must(template.New("").Funcs(templateFuncs(nil, robots)).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	board := make([]string, len(cases))
	for i := range cases {
		var buf bytes.Buffer
		if err := base.ExecuteTemplate(&buf, "order-route", &cases[i].order); err != nil {
			t.Fatalf("execute order-route: %v", err)
		}
		board[i] = routeSequence(buf.String(), "→", "—")
	}

	stopsJSON, err := json.Marshal(stops)
	testutil.MustNoErr(t, err, "marshal stops")

	// The pop-up's hero.
	popupHTML := runRouteJS(t, nodePath, "var nodeLink = function (n) { return '<a>' + n + '</a>'; };\n"+
		"var NOT_ASSIGNED = '<span>not assigned yet</span>';\n"+
		staticJSFunction(t, "static/pages/orders.js", "routeHTML")+"\n"+
		"console.log(JSON.stringify("+string(stopsJSON)+".map(function (s) { return routeHTML(s); })));")

	// The task board's row: its route cell.
	taskHTML := runRouteJS(t, nodePath, "var escapeHtml = function (s) { return String(s); };\n"+
		"var statusClass = function () { return ''; }, statusLabel = function (s) { return s; }, formatETA = function () { return ''; };\n"+
		staticJSFunction(t, "static/pages/dashboard.js", "routeCell")+"\n"+
		staticJSFunction(t, "static/pages/dashboard.js", "rowHTML")+"\n"+
		"console.log(JSON.stringify("+string(stopsJSON)+".map(function (s) {"+
		" var m = rowHTML({ route: s }).match(/<td class=\"r-route\">(.*?)<\\/td>/); return m ? m[1] : ''; })));")

	for i, c := range cases {
		popup := routeSequence(popupHTML[i], "&rarr;", "not assigned yet")
		task := routeSequence(taskHTML[i], "&rarr;", "-")
		if board[i] != c.want || popup != c.want || task != c.want {
			t.Errorf("%s: rule %s; board %s, pop-up %s, task board %s", c.name, c.want, board[i], popup, task)
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

// stubRobotStations is the fleet cache as the route reads it.
type stubRobotStations map[string]string

func (s stubRobotStations) GetCachedRobotStatus(id string) (fleet.RobotStatus, bool) {
	st, ok := s[id]
	return fleet.RobotStatus{VehicleID: id, CurrentStation: st}, ok
}

// stopSequence is the rule's answer in routeSequence's terms.
func stopSequence(stops []domain.RouteStop) string {
	parts := make([]string, len(stops))
	for i, s := range stops {
		switch {
		case s.Unassigned:
			parts[i] = "?"
		case s.Current:
			parts[i] = "[" + s.Node + "]"
		default:
			parts[i] = s.Node
		}
	}
	return strings.Join(parts, " > ")
}

var routeTitleAttr = regexp.MustCompile(` title="[^"]*"`)

// routeSequence reduces a drawn route to "A > [B] > C": an unchosen end as
// "?", the stop drawn in <strong> (the current one) in brackets. Titles are
// dropped first: the partial's carries the whole route, arrows and all.
func routeSequence(html, arrow, none string) string {
	html = routeTitleAttr.ReplaceAllString(html, "")
	var parts []string
	for _, p := range strings.Split(html, arrow) {
		current := strings.Contains(p, "<strong")
		p = visibleRouteText(p)
		if p == none {
			p = "?"
		}
		if current {
			p = "[" + p + "]"
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
