package www

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"shingo/shared"
)

// TestNoRawZIndexInCoreAssets holds every stylesheet, page script and template
// this surface ships to the layer scale in shared/tokens.css (D19).
//
// Core is where the ratchet was worst: style.css alone carried eleven raw
// values across seven distinct numbers, including a 2000 field tip and a 9999
// toast that had to keep outbidding each other, and app.js reached for 10000
// to get above both. Nothing connected them, so each was correct in isolation
// and the set was incoherent.
//
// Templates are scanned as well as CSS and JS. A <style> block inside a
// template is a stylesheet that no CSS-only scan can see — demand.html carried
// one — and it is exactly where a value gets added by somebody who is not
// thinking about the app's layering at all.
func TestNoRawZIndexInCoreAssets(t *testing.T) {
	var scanned int

	scan := func(fsys fs.FS, root, label string) {
		err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch filepath.Ext(p) {
			case ".css", ".js", ".html":
			default:
				return nil
			}
			if strings.Contains(p, "/vendor/") || strings.HasSuffix(p, ".min.js") {
				return nil
			}
			body, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			scanned++
			for _, raw := range shared.RawZIndexUses(string(body)) {
				t.Errorf("%s sets z-index to %q instead of a layer token.\n"+
					"  Pick the rung that says what the element IS — raised / sticky / tooltip / dropdown / chrome / modal / modal-over / popover / toast — from shared/tokens.css.\n"+
					"  A raw number cannot be reasoned about against the rest of the app, which is how this surface ended up with 2000, 9999 and 10000 all claiming to be on top.",
					p, raw)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
	}

	scan(staticFS, "static", "static")
	scan(templateFS, "templates", "templates")

	if scanned == 0 {
		t.Fatal("scanned no Core assets at all — the walk has drifted and this test is a no-op")
	}
}
