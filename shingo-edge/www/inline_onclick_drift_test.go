package www

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoInlineEventHandlersInTemplates pins the "no inline event
// handlers" convention from the style guide (§ Event handling).
// Templates must bind events via data-action / data-action-<event>
// attributes that the page script wires to delegated listeners
// (see shared/utils.js delegateActions).
//
// Covers click, change, input, blur, keydown, submit, focus,
// keyup, mousedown, mouseup. If a new event type sneaks in, add
// it to the regex.
//
// Why: inline `on<event>="foo()"` requires `foo` to be a window
// global, which blocks ES module surface cleanup and CSP-friendly
// loading. The shared helper module is already a module; page
// scripts follow when their templates stop emitting inline
// handlers.
//
// The test reads every templates/*.html file and templates/partials/
// recursively, regexes for inline event handler attributes, and
// fails if any match. The allowlist below covers any genuinely-
// dynamic case that can't be removed (none today; the slice
// exists so a future justified exception has a single place to
// land with a comment).
func TestNoInlineEventHandlersInTemplates(t *testing.T) {
	eventRe := regexp.MustCompile(`(?i)\bon(click|change|input|blur|keydown|submit|focus|keyup|mousedown|mouseup)\s*=`)
	allowlist := []string{
		// (empty — see comment above)
	}

	root := "templates"
	err := fs.WalkDir(templatesFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".html" {
			return nil
		}
		for _, allowed := range allowlist {
			if strings.HasSuffix(p, allowed) {
				return nil
			}
		}
		body, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if eventRe.MatchString(line) {
				t.Errorf("%s:%d inline event handler — use data-action[-event] + delegateActions instead\n  %s",
					p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}
