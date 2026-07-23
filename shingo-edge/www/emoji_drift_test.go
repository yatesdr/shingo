package www

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"shingo/shared"
)

// TestNoEmojiInTemplatesAndPageJS enforces the "no emoji, ever" icon policy
// (D17, docs/ui-style-guide.md § Icon policy) on the Edge admin surface. Mirror
// of the Core test; the detection lives in shared.IsEmoji so both surfaces stay
// in lockstep. Emoji render inconsistently, can't take currentColor, and drift
// from the vendored Lucide sprite — use an icon or plain text. Monochrome
// geometric glyphs (arrows, chevrons, bullets, check/cross, the bare warning
// sign) are allowed; see shared.IsEmoji for the exact boundary.
func TestNoEmojiInTemplatesAndPageJS(t *testing.T) {
	scanForEmoji(t, templatesFS, "templates")
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
				t.Errorf("%s:%d emoji U+%04X forbidden by the no-emoji icon policy (D17) — use <svg class=\"icon\"><use href=\"#icon-name\"> or plain text\n  %s",
					p, i+1, r, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
