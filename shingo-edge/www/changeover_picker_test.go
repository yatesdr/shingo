package www

import (
	"html/template"
	"strings"
	"testing"

	"shingoedge/domain"
)

// changeover_picker_test.go — the Target Style picker does not offer the style
// the process is already running.
//
// THE REFUSAL ALREADY EXISTED; THE OFFER IS THE BUG. planChangeover refuses a
// target equal to the active style (engine: ErrStyleAlreadyRunning, surfaced as
// a 400), so picking it was never dangerous — it was just silent until the
// operator had selected it, clicked Preview, and read an error. The page prints
// "Current style: X" directly above a list that then offered X. Reported off
// the floor at Hopkinsville, 2026-09-15.
//
// RENDERED, NOT GREPPED. Asserting the template file contains an `eq .Name`
// would pass on a guard wired to the wrong variable, which is the only way this
// can realistically break: the handler passes a map whose key is "CurrentStyle"
// while the struct field behind it is CurrentStyleName (handlers_changeover.go
// :223 and :252 — the full page and the SSE partial each build their own map).
// A guard reading .CurrentStyleName would render nothing and disable nobody.
// Executing the real template against that same map shape is what catches it.
func TestChangeoverPicker_RunningStyleIsOfferedDisabled(t *testing.T) {
	t.Parallel()

	tmpl := template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	// ActiveChangeover is absent on purpose: the picker lives in the {{else}}
	// of that branch, so a changeover in flight shows no list at all.
	data := map[string]any{
		"ActiveProcessID": int64(3),
		"CurrentStyle":    "Running-Style",
		"Styles": []domain.Style{
			{ID: 1, Name: "Running-Style"},
			{ID: 2, Name: "Other-Style"},
			{ID: 3, Name: "Blocked-Style"},
		},
		"SourcingByStyle": map[string]styleSourcingView{
			"Running-Style": {Status: "Ready"},
			"Other-Style":   {Status: "Ready"},
			"Blocked-Style": {Status: "Short", Blocked: true, Note: "no bins"},
		},
	}

	var out strings.Builder
	if err := tmpl.ExecuteTemplate(&out, "changeover-body", data); err != nil {
		t.Fatalf("render changeover-body: %v", err)
	}
	got := out.String()

	for _, tc := range []struct {
		label    string
		option   string
		disabled bool
	}{
		{"the running style", `<option value="1" disabled>Running-Style · running</option>`, true},
		{"a selectable style", `<option value="2">Other-Style · Ready</option>`, false},
		// The sourcing block still decides the others: "running" replaces a
		// status, it does not become a second reason a row can be greyed out.
		{"a sourcing-blocked style", `<option value="3" disabled>Blocked-Style · Short (no bins)</option>`, true},
	} {
		if !strings.Contains(got, tc.option) {
			t.Errorf("%s: expected option not rendered.\nwant: %s\ngot:\n%s", tc.label, tc.option, got)
		}
	}

	// The running style must not ALSO appear in selectable form — a guard that
	// rendered both branches would pass the contains-check above while still
	// offering the style twice.
	if strings.Contains(got, `<option value="1">`) {
		t.Errorf("the running style is also rendered selectable:\n%s", got)
	}
}

// TestChangeoverPicker_NoActiveStyleOffersEverything pins the empty case: with
// no style running, `eq .Name ""` is false for every real name and nothing is
// greyed out on those grounds. Styles have a UNIQUE(process_id, name) index and
// no live row is blank, so there is no name this can collide with — but the
// picker is the wrong place to learn that, so it is pinned.
func TestChangeoverPicker_NoActiveStyleOffersEverything(t *testing.T) {
	t.Parallel()

	tmpl := template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	data := map[string]any{
		"ActiveProcessID": int64(3),
		"CurrentStyle":    "",
		"Styles":          []domain.Style{{ID: 1, Name: "Only-Style"}},
		"SourcingByStyle": map[string]styleSourcingView{"Only-Style": {Status: "Ready"}},
	}

	var out strings.Builder
	if err := tmpl.ExecuteTemplate(&out, "changeover-body", data); err != nil {
		t.Fatalf("render changeover-body: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, `<option value="1">Only-Style · Ready</option>`) {
		t.Errorf("with no style running the only style should be selectable:\n%s", got)
	}
	if strings.Contains(got, "· running") {
		t.Errorf("nothing is running, so no option should be marked running:\n%s", got)
	}
}
