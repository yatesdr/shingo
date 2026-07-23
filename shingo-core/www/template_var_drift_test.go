package www

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"shingo/shared"
)

// TestNoUndefinedCSSVarsInTemplates guards the blind spot that let a
// `var(--card-bg)` reference sit in a template long after the token was renamed
// away — it rendered as unset with no error. Every `var(--foo)` a template uses
// must resolve to a `--foo` defined somewhere the page actually loads: the
// shared token/component/status CSS, Core's own style.css / dashboard.css, or an
// inline custom property in the template itself (e.g. style="--progress: 60%").
//
// The token-vs-CSS drift test only covered CSS files; template-inline var()
// references escaped it. This closes that gap.
func TestNoUndefinedCSSVarsInTemplates(t *testing.T) {
	defRe := regexp.MustCompile(`(--[a-zA-Z0-9_-]+)\s*:`)
	useRe := regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9_-]+)`)

	defined := map[string]bool{}
	addDefs := func(src string) {
		for _, m := range defRe.FindAllStringSubmatch(src, -1) {
			defined[m[1]] = true
		}
	}

	// Shared CSS (tokens, components, status classes) — loaded by every page.
	for _, name := range []string{"tokens.css", "components.css", "status-classes.css"} {
		b, err := fs.ReadFile(shared.Files, name)
		if err != nil {
			t.Fatalf("read shared/%s: %v", name, err)
		}
		addDefs(string(b))
	}
	// Core's own stylesheets.
	for _, name := range []string{"static/style.css", "static/dashboard.css"} {
		b, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		addDefs(string(b))
	}

	// Allowlist for tokens defined outside the scanned files (none today; a
	// justified exception lands here with a comment).
	allow := map[string]bool{}

	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".html" {
			return nil
		}
		body, err := fs.ReadFile(templateFS, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		src := string(body)
		// A template may define its own custom properties inline; those count.
		local := map[string]bool{}
		for _, m := range defRe.FindAllStringSubmatch(src, -1) {
			local[m[1]] = true
		}
		for i, line := range strings.Split(src, "\n") {
			for _, m := range useRe.FindAllStringSubmatch(line, -1) {
				name := m[1]
				if defined[name] || local[name] || allow[name] {
					continue
				}
				t.Errorf("%s:%d references undefined CSS var %s — define it in tokens.css (or the page CSS), or fix the name\n  %s",
					p, i+1, name, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}
