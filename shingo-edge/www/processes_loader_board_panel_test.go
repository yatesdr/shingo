package www

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"shingoedge/domain"
	"shingoedge/service"
)

// The loader-board panel is server-rendered, so a bad field reference does not
// fail a build or a parse — it fails at EXECUTE, which 500s the whole Processes
// page. That is a worse outcome than the advisory it carries, so the render is
// exercised directly.
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

// The whole right-hand column — and with it the Screens tab that carries the
// panel — is behind `{{if .ActiveProcess}}`. So the panel is only ever seen with
// a process selected, which is correct (a screen belongs to one process) and is
// why the fixture supplies one.
func TestProcessesPage_RendersLoaderBoardGaps(t *testing.T) {
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
	for _, want := range []string{
		"Unloader",
		"ULN_002, ULN_003",              // join over Windows
		"Press 4",                       // the inferred owner, shown not assumed
		"createLoaderBoard:loader:9:15", // the delegated action, key and process
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel missing %q", want)
		}
	}
}

// The panel is absent, not empty, when there is nothing to say. A permanent
// empty box on a config page trains people to stop reading it.
func TestProcessesPage_NoPanelWhenNoGaps(t *testing.T) {
	out := renderProcessesPage(t, map[string]any{
		"Page":            "processes",
		"ActiveProcess":   &domain.Process{ID: 15, Name: "Press 4", ProductionState: "active_production"},
		"ActiveProcessID": int64(15),
		"LoaderBoardGaps": []service.LoaderBoardGap{},
	})
	if strings.Contains(out, "no operator screen on this edge") {
		t.Error("panel rendered with nothing to report")
	}
}
