package www

import (
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingoedge/domain"
)

// A template's field references are invisible to every gate the repo runs.
// {{.Runtime.X}} is resolved by reflection at EXECUTE time, so renaming X on
// the Go side leaves the template compiling, vetting, linting and unit-testing
// green while the page it renders dies at the first row that reaches the
// reference — and text/template writes as it goes, so the response is a
// half-rendered page with a 200 already on the wire.
//
// Not hypothetical. RemainingUOP became RemainingUOPCached in the UOP
// bin-as-truth refactor (0fe4a9ca, 2026-05-02); partials/production-body.html
// kept the old name at three sites, and Edge's Production page has died mid-<td>
// on any node carrying a runtime state ever since — losing the UOP column, the
// actions column, all three modals and the footer. It was found by opening the
// page, not by anything here.
//
// The handler tests in this package cover buildStationViews and the view
// enrichment; not one of them EXECUTES a template, which is the whole gap.
// This closes it for the type that has already been renamed once: every
// .Runtime.<Name> in every template must name a real field or method on
// domain.RuntimeState.
func TestTemplateRuntimeFieldsExistOnDomainType(t *testing.T) {
	ref := regexp.MustCompile(`\.Runtime\.([A-Za-z_][A-Za-z0-9_]*)`)

	rt := reflect.TypeOf(domain.RuntimeState{})
	ptr := reflect.PointerTo(rt)
	known := func(name string) bool {
		if _, ok := rt.FieldByName(name); ok {
			return true
		}
		if _, ok := ptr.MethodByName(name); ok {
			return true
		}
		_, ok := rt.MethodByName(name)
		return ok
	}

	seen := map[string]map[string]bool{} // field name -> set of templates referencing it

	err := fs.WalkDir(templatesFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".html" {
			return err
		}
		body, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			return err
		}
		for _, m := range ref.FindAllStringSubmatch(string(body), -1) {
			if seen[m[1]] == nil {
				seen[m[1]] = map[string]bool{}
			}
			seen[m[1]][p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}

	// A check must know whether it had the input to check: if the regex stops
	// matching (the templates move, or the accessor changes shape), this test
	// would go green while checking nothing.
	if len(seen) == 0 {
		t.Fatal("no .Runtime.<Field> references found in any template — " +
			"either the templates moved out of templatesFS or this guard has " +
			"stopped recognising the reference it exists to check")
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if known(name) {
			continue
		}
		files := make([]string, 0, len(seen[name]))
		for f := range seen[name] {
			files = append(files, f)
		}
		sort.Strings(files)
		t.Errorf("%s references .Runtime.%s, which is not a field or method on domain.RuntimeState.\n"+
			"  The page renders 200 and then dies mid-element the first time a row reaches it.\n"+
			"  Rename the reference in the template to the current field name (see shingoedge/domain.RuntimeState).",
			strings.Join(files, ", "), name)
	}
}
