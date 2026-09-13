package www

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strings"
	"testing"

	"shingoedge/domain"
	"shingoedge/service"
)

// processes_loader_board_panel_test.go — what the Processes page HANDS the
// desktop, and the fact that it hands it at all.
//
// The three-tab page rendered the loader-window panel and the routing fieldset
// in Go templates, and these tests read the rendered HTML for them. U9 moved
// both to the client — D4 draws the loader windows, D3 draws the routing set —
// so the server-side contract is no longer markup: it is the JSON on
// #page-data, and the page script reads it there rather than asking again.
//
// THE FAILURE MODE IS THE SAME AND SO IS THE TEST'S REASON. #page-data is
// filled by a template action per attribute, so a bad field reference does not
// fail a build or a parse — it fails at EXECUTE, which 500s the whole page.
// That is worse than the advisory it carries, so the render is exercised
// directly and the payload is parsed, not string-matched: a template that
// emitted `[object Object]` would satisfy a Contains check and starve the
// screen.
func renderProcessesPage(t *testing.T, data map[string]any) string {
	t.Helper()
	tmpl, err := template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "processes.html", data); err != nil {
		t.Fatalf("execute processes.html: %v", err)
	}
	return buf.String()
}

// pageData pulls one attribute off #page-data and unmarshals it, which is what
// the page script does with it.
func pageData(t *testing.T, out, attr string, into any) {
	t.Helper()
	i := strings.Index(out, "id=\"page-data\"")
	if i < 0 {
		t.Fatal("the Processes page has no #page-data; the desktop boots from it and would draw nothing")
	}
	end := strings.Index(out[i:], "></div>")
	if end < 0 {
		t.Fatal("#page-data is unterminated")
	}
	block := out[i : i+end]
	j := strings.Index(block, attr+"='")
	if j < 0 {
		t.Fatalf("#page-data carries no %s", attr)
	}
	rest := block[j+len(attr)+2:]
	k := strings.Index(rest, "'")
	if k < 0 {
		t.Fatalf("%s is unterminated", attr)
	}
	raw := strings.NewReplacer("&#34;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'").
		Replace(rest[:k])
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		t.Fatalf("%s is not JSON the page can read (%v): %s", attr, err, raw)
	}
}

// The loader windows D4 lists: Core loaders with no operator screen on this
// edge. They travel on the page rather than through a request because the
// handler already read them for the old panel and a second round trip would
// buy nothing.
func TestProcessesPage_CarriesLoaderBoardGaps(t *testing.T) {
	out := renderProcessesPage(t, map[string]any{
		"Page":            "processes",
		"ActiveProcess":   &domain.Process{ID: 15, Name: "Press 4", ProductionState: "active_production"},
		"ActiveProcessID": int64(15),
		"LoaderBoardGaps": []service.LoaderBoardGap{{
			LoaderKey: "loader:9", Name: "Unloader",
			Role: "consume", Layout: "shared_window",
			Windows:     []string{"ULN_002", "ULN_003"},
			ProcessID:   15,
			ProcessName: "Press 4",
		}},
	})
	var gaps []service.LoaderBoardGap
	pageData(t, out, "data-loader-gaps", &gaps)
	if len(gaps) != 1 {
		t.Fatalf("page carries %d loader gaps, want 1", len(gaps))
	}
	if gaps[0].Name != "Unloader" || gaps[0].LoaderKey != "loader:9" {
		t.Errorf("the gap lost its identity on the way to the page: %+v", gaps[0])
	}
	if len(gaps[0].Windows) != 2 || gaps[0].Windows[0] != "ULN_002" {
		t.Errorf("the gap lost its windows: %v", gaps[0].Windows)
	}
	if gaps[0].ProcessName != "Press 4" {
		t.Errorf("the inferred owner is shown, not assumed: %q", gaps[0].ProcessName)
	}
}

// Nothing to say, nothing carried. D4 then prints the one sentence that says
// so, rather than an empty box that trains people to stop reading.
func TestProcessesPage_NoGapsCarriesAnEmptyList(t *testing.T) {
	out := renderProcessesPage(t, map[string]any{
		"Page":            "processes",
		"ActiveProcess":   &domain.Process{ID: 15, Name: "Press 4", ProductionState: "active_production"},
		"ActiveProcessID": int64(15),
		"LoaderBoardGaps": []service.LoaderBoardGap{},
	})
	var gaps []service.LoaderBoardGap
	pageData(t, out, "data-loader-gaps", &gaps)
	if len(gaps) != 0 {
		t.Errorf("page carries %d gaps with none to report", len(gaps))
	}
}

// TestProcessesPage_CarriesTheGateInBothStates pins the flow-composer gate on
// the page in BOTH states — a single-state assertion would pass on a page that
// never reads the field.
//
// The gate is why this test exists at all: D5 draws the only switch for it and
// the app bar draws a read-only pill from the same value (ruling R3), so a page
// that shipped it wrong would show an engineer a press whose operators can edit
// flows as one whose operators cannot.
func TestProcessesPage_CarriesTheGateInBothStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"gate off", false},
		{"gate on", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := domain.Process{ID: 15, Name: "Press 4", ProductionState: "active_production", FlowComposerEnabled: tc.enabled}
			out := renderProcessesPage(t, map[string]any{
				"Page":            "processes",
				"ActiveProcess":   &p,
				"ActiveProcessID": int64(15),
				"Processes":       []domain.Process{p},
			})
			var got []domain.Process
			pageData(t, out, "data-processes", &got)
			if len(got) != 1 {
				t.Fatalf("page carries %d processes, want 1", len(got))
			}
			if got[0].FlowComposerEnabled != tc.enabled {
				t.Errorf("flow_composer_enabled = %v, want %v", got[0].FlowComposerEnabled, tc.enabled)
			}
		})
	}
}

// The page boots the desktop and nothing else: one root, one sheet host, and
// the four files the composer is. A page that lost the module tag would render
// the noscript list and look, from the server, exactly as healthy.
func TestProcessesPage_BootsTheDesktop(t *testing.T) {
	out := renderProcessesPage(t, map[string]any{
		"Page":            "processes",
		"ActiveProcess":   &domain.Process{ID: 15, Name: "Press 4"},
		"ActiveProcessID": int64(15),
	})
	for _, want := range []string{
		`id="pd-root"`,
		`id="pd-scrim"`,
		`/static/js/pages/processes-desktop.js`,
		`/static/operator-station/composer-model.js`,
		`/static/operator-station/composer-glyphs.js`,
		`/static/operator-station/flowspec-data.js`,
		`/static/operator-station/flow-picture.css`,
		`/static/css/processes-desktop.css`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the Processes page does not carry %s", want)
		}
	}
	// AND THE CLAIM EDITOR IS GONE. Every one of these was a control of the
	// three-tab page; each now has exactly one home on the desktop, and a
	// second copy reappearing is the failure this names.
	for _, gone := range []string{
		`id="claims-add-role"`,
		`id="claims-add-swap"`,
		`id="claim-modal"`,
		`id="compare-all`,
		`id="pd-legacy"`,
		`/static/js/pages/processes.js`,
	} {
		if strings.Contains(out, gone) {
			t.Errorf("the retired claim editor is back on the page: %s", gone)
		}
	}
}
