package www

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"shingo/shared"
)

// TestNoEmojiInTemplatesAndPageJS enforces the "no emoji, ever" icon policy
// (see docs/ui-style-guide.md § Icons). Emoji render inconsistently
// across platforms, can't take currentColor, and drift from the vendored Lucide
// sprite (shared/icons.svg). Use an icon —
// `<svg class="icon"><use href="#icon-name"></use></svg>` — or plain text
// instead. Monochrome geometric glyphs (arrows, chevrons, bullets, check/cross,
// the bare warning sign) are allowed; see shared.IsEmoji for the exact boundary.
//
// Historical first catch: a 🔒 lock emoji in bins.js. The detection lives in
// shared.IsEmoji so this test and the Edge mirror stay in lockstep.
func TestNoEmojiInTemplatesAndPageJS(t *testing.T) {
	scanForEmoji(t, templateFS, "templates")
	scanForEmoji(t, staticFS, "static")
}

func scanForEmoji(t *testing.T, fsys fs.FS, root string) {
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
		// Skip third-party / minified / test files — only first-party UI source
		// is held to the policy.
		if strings.Contains(p, "/vendor/") || strings.HasSuffix(p, ".min.js") || strings.HasSuffix(p, ".test.js") {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			if r, ok := shared.FirstEmoji(line); ok {
				t.Errorf("%s:%d emoji U+%04X forbidden by the no-emoji icon policy — use <svg class=\"icon\"><use href=\"#icon-name\"> or plain text\n  %s",
					p, i+1, r, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
