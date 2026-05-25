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
// Mirrors the equivalent test in shingo-edge/www; see that file's
// comment for the rationale.
func TestNoInlineEventHandlersInTemplates(t *testing.T) {
	eventRe := regexp.MustCompile(`(?i)\bon(click|change|input|blur|keydown|submit|focus|keyup|mousedown|mouseup)\s*=`)
	allowlist := []string{
		// (empty — single landing pad for justified exceptions)
	}

	root := "templates"
	err := fs.WalkDir(templateFS, root, func(p string, d fs.DirEntry, err error) error {
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
		body, err := fs.ReadFile(templateFS, p)
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
