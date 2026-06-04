package www

import (
	"bytes"
	"html/template"
	"io/fs"
	"strings"
	"testing"

	"shingocore/store/dashboards"
)

// TestTemplatesParse mirrors NewRouter's template parsing so a malformed page
// template fails in `go test` rather than panicking (template.Must) at server
// startup. The handler-test fixtures use an empty tmpls map, so without this
// nothing exercises the real parse pipeline.
//
// It also smoke-executes the two dashboard templates: the chromeless display
// (run by its own file name via the renderBare path) and the admin page (run
// through the shared "layout"). This catches a bad field reference or template
// function the parse step alone would not.
func TestTemplatesParse(t *testing.T) {
	base := template.Must(template.New("").Funcs(templateFuncs()).
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

	// Admin page: executed through the shared layout chrome.
	adm, ok := tmpls["dashboards.html"]
	if !ok {
		t.Fatal("dashboards.html not parsed")
	}
	buf.Reset()
	if err := adm.ExecuteTemplate(&buf, "layout", map[string]any{"Page": "dashboards", "Authenticated": true}); err != nil {
		t.Fatalf("execute dashboards.html via layout: %v", err)
	}
}
