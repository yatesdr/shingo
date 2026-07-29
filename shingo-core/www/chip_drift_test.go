package www

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"shingo/shared"
)

// TestEveryChipModifierHasAStylesheetRule asserts that every `.chip-<x>`
// applied alongside the base `.chip` class in a template or page JS file has a
// rule in some stylesheet this surface loads.
//
// A chip is two classes: `.chip` is the pill, `.chip-<x>` is the colour. A
// modifier with no rule anywhere still renders — as a bare pill the same
// colour as the page behind it. It looks like nothing was there.
//
// This has now happened TWICE. `.chip-err` shipped at 1.2:1 contrast (fixed in
// d01156b1) and `.chip-warn` was referenced by inventory.js for two releases
// with no rule in any stylesheet at all. Both were caught by somebody
// squinting at a page. This test closes the class.
//
// KNOWN HOLE, and it is deliberate: a modifier assembled at runtime
// (`'chip chip-' + tone`) has no literal to match, so it is invisible to this
// scan. There are none today. If one appears, prefer a small map of literal
// class names over a concatenation — this test is the reason.
func TestEveryChipModifierHasAStylesheetRule(t *testing.T) {
	css := loadAllCSS(t)

	used := map[string][]string{} // modifier -> files that use it
	scanChipUse(t, templateFS, "templates", used)
	scanChipUse(t, staticFS, "static", used)

	if len(used) == 0 {
		t.Fatal("no chip usage found at all — the scanner has drifted from the markup, which makes this test a no-op")
	}

	for mod, files := range used {
		if !shared.CSSDeclaresClass(css, mod) {
			t.Errorf("%s is applied in %s but no stylesheet declares it — it renders as a bare .chip, invisible against the page.\n"+
				"  Add a rule to shared/components.css beside .chip-ok / .chip-err.",
				mod, strings.Join(files, ", "))
		}
	}
}

// scanChipUse records every chip modifier used in fsys under root.
func scanChipUse(t *testing.T, fsys fs.FS, root string, into map[string][]string) {
	t.Helper()
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(p); ext != ".html" && ext != ".js" {
			return nil
		}
		if strings.Contains(p, "/vendor/") || strings.HasSuffix(p, ".min.js") {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		for _, mod := range shared.ChipModifiers(string(body)) {
			into[mod] = append(into[mod], p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// loadAllCSS concatenates every stylesheet this surface can load: the shared
// component/token sheets and Core's own static CSS. A rule anywhere in that
// set makes the class reachable, which is what the test asks.
func loadAllCSS(t *testing.T) string {
	t.Helper()
	var b strings.Builder

	err := fs.WalkDir(shared.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".css" {
			return err
		}
		body, err := fs.ReadFile(shared.Files, p)
		if err != nil {
			return err
		}
		b.Write(body)
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk shared CSS: %v", err)
	}

	err = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".css" {
			return err
		}
		body, err := fs.ReadFile(staticFS, p)
		if err != nil {
			return err
		}
		b.Write(body)
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk static CSS: %v", err)
	}

	if b.Len() == 0 {
		t.Fatal("no CSS loaded — the test would pass vacuously")
	}
	return b.String()
}
