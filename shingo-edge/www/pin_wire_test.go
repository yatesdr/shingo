package www

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/service"
	"shingoedge/store/processes"
)

// pin_wire_test.go — lane W pins (ui-cleanup, 2026-10-07), Edge side.
//
// P0 pinned each control AS DEAD at 5c0beb74 (built and one connection
// short). The W unit wired them and flipped each pin to the wired state, under
// its label (W1, W2, W3); the before and after of every case is in the
// evidence folder (predictions/p0-wrm.md).

// pinDelegateActionsKeys returns the keys of every `delegateActions(document.body,
// { … })` map in a page script, in source order, one slice per call. A key is the
// shorthand name or the name before `:`; quoted keys are unquoted.
func pinDelegateActionsKeys(t *testing.T, src string) [][]string {
	t.Helper()
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

// W1 (Edge). production.js imports hideModal and now maps it, so the four
// data-action="hideModal:<id>" buttons in production.html (view-bin and
// request-empty modals: × and Close/Cancel) close their modal.
func TestPinWire_W1_EdgeProductionHideModalMapped(t *testing.T) {
	js := pinReadFile(t, "static/js/pages/production.js")
	maps := pinDelegateActionsKeys(t, js)
	if len(maps) != 1 {
		t.Fatalf("production.js: %d delegateActions maps, want 1", len(maps))
	}
	// Scanner self-check: the map parses to known members.
	if len(maps[0]) < 10 || !pinContains(maps[0], "viewBinContents") || !pinContains(maps[0], "autofillNodeDefaults") {
		t.Fatalf("production.js map parsed as %v; want autofillNodeDefaults and viewBinContents among its keys", maps[0])
	}
	if !regexp.MustCompile(`(?m)^import \{[^}]*\bhideModal\b[^}]*\} from '/static/js/shingoedge.js';`).MatchString(js) {
		t.Errorf("production.js no longer imports hideModal from shingoedge.js")
	}
	if !pinContains(maps[0], "hideModal") {
		t.Errorf("W1: hideModal is missing from production.js's delegateActions map; its four buttons do nothing")
	}
	tmpl := pinReadFile(t, "templates/production.html")
	if n := strings.Count(tmpl, `data-action="hideModal:`); n != 4 {
		t.Errorf("production.html has %d data-action=\"hideModal:…\" buttons, want 4", n)
	}
}

// W2. buildChangeoverViewData fills GateBlockers and both handlers' template
// data maps now carry it, so the "Cutover is waiting on…" panel
// (partials/changeover-body.html, {{if .GateBlockers}}) renders on the full
// page and on the SSE partial when blockers are present.
func TestPinWire_W2_ChangeoverGateBlockersRendered(t *testing.T) {
	h, _ := newTestHandlers(t)
	h.tmpl = template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))
	eng := h.engine.(*stubEngine)
	eng.gateCanComplete = false
	eng.gateBlockers = []domain.Blocker{
		{Reason: "task at node PIN-W2-NODE in staging_requested", NodeName: "PIN-W2-NODE", Hard: true},
		{Reason: "order 703 in in_transit", OrderID: 703, Hard: true},
	}

	pid := seedProcess(t, "PinW2Gate")
	fromStyleID := seedStyle(t, "PinW2-From", pid)
	toStyleID := seedStyle(t, "PinW2-To", pid)
	active := fromStyleID
	testutil.MustNoErr(t, testDB.SetActiveStyle(pid, &active), "SetActiveStyle")
	stationID := seedOperatorStation(t, pid, "PIN-W2-ST", "PinW2Station")
	_ = seedProcessNode(t, pid, stationID, "PIN-W2-NODE")
	existing, err := testDB.ListProcessNodesByProcess(pid)
	if err != nil {
		t.Fatalf("ListProcessNodesByProcess: %v", err)
	}
	from := fromStyleID
	if _, err := service.NewChangeoverService(testDB).Create(pid, &from, toStyleID, "test", "pin W2",
		[]int64{stationID},
		[]processes.NodeTaskInput{{ProcessID: pid, CoreNodeName: "PIN-W2-NODE", Situation: "switch", State: "pending"}},
		nil, existing); err != nil {
		t.Fatalf("CreateChangeover: %v", err)
	}

	// Control 1: the view data does carry the blockers.
	d := h.buildChangeoverViewData(&processes.Process{ID: pid, ActiveStyleID: &fromStyleID})
	if len(d.GateBlockers) != 2 {
		t.Fatalf("buildChangeoverViewData GateBlockers = %d, want 2 (the stub's canned blockers)", len(d.GateBlockers))
	}

	// Control 2: the partial renders the panel when the key is present.
	var ctl strings.Builder
	if err := h.tmpl.ExecuteTemplate(&ctl, "changeover-body", map[string]any{
		"ActiveChangeover": d.ActiveChangeover, "GateBlockers": d.GateBlockers,
	}); err != nil {
		t.Fatalf("render changeover-body with GateBlockers: %v", err)
	}
	if !strings.Contains(ctl.String(), `id="changeover-gate-panel"`) {
		t.Fatalf("changeover-body with GateBlockers did not render #changeover-gate-panel; the control is broken")
	}

	q := "?process=" + strconv.FormatInt(pid, 10)
	for _, tc := range []struct {
		name string
		path string
		fn   http.HandlerFunc
	}{
		{"full page handleChangeover", "/changeover" + q, h.handleChangeover},
		{"SSE partial handleChangeoverPartial", "/changeover/partial" + q, h.handleChangeoverPartial},
	} {
		rec := httptest.NewRecorder()
		tc.fn(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %q", tc.name, rec.Code, body)
		}
		if !strings.Contains(body, `data-action="cancelProcessChangeover"`) {
			t.Fatalf("%s: the active-changeover branch did not render; the fixture is wrong", tc.name)
		}
		if !strings.Contains(body, `id="changeover-gate-panel"`) || !strings.Contains(body, "Cutover is waiting on") {
			t.Errorf("W2: %s does not render the gate panel with 2 blockers present", tc.name)
		}
		for _, b := range eng.gateBlockers {
			if !strings.Contains(body, b.Reason) {
				t.Errorf("W2: %s gate panel does not name blocker %q", tc.name, b.Reason)
			}
		}
	}
}

// W3. Twenty-one handler replies name the htmx event refreshMaterial
// (writeJSONWithTrigger / writeActionOK), and the production page now listens
// for it: #production-content's hx-trigger (templates/production.html :14)
// carries "refreshMaterial from:body". That template and nothing else names it.
func TestPinWire_W3_RefreshMaterialHeardOnProduction(t *testing.T) {
	tmpl := pinReadFile(t, "templates/production.html")
	m := regexp.MustCompile(`(?s)id="production-content"\s+hx-get="[^"]*"\s+hx-trigger="([^"]*)"`).FindStringSubmatch(tmpl)
	if m == nil {
		t.Fatalf("production.html: #production-content hx-trigger not found")
	}
	if !strings.Contains(m[1], "refreshProduction from:body") {
		t.Fatalf("#production-content hx-trigger = %q; scanner self-check expects refreshProduction from:body", m[1])
	}
	if !strings.Contains(m[1], "refreshMaterial from:body") {
		t.Errorf("W3: #production-content does not listen for refreshMaterial (%q)", m[1])
	}

	// The one listener in the page layer is production.html.
	var listeners []string
	for _, root := range []string{"templates", "static"} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			if strings.HasSuffix(p, ".test.js") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err == nil && strings.Contains(string(b), "refreshMaterial") {
				listeners = append(listeners, filepath.ToSlash(p))
			}
			return nil
		})
	}
	if len(listeners) != 1 || listeners[0] != "templates/production.html" {
		t.Errorf("W3: refreshMaterial is named in %v, want [templates/production.html] only", listeners)
	}

	// The emitters exist (21 at 5c0beb74; the handler files are not edited by W).
	n := 0
	gos, err := filepath.Glob("*.go")
	testutil.MustNoErr(t, err, "glob handler files")
	for _, p := range gos {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		n += strings.Count(pinReadFile(t, p), `"refreshMaterial"`)
	}
	if n == 0 {
		t.Fatalf("no handler names refreshMaterial; the premise of W3 is gone")
	}
	t.Logf("refreshMaterial emit sites in www/*.go: %d", n)
}
