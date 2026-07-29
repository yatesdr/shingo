package www

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"shingo/shared"
)

// TestNoRawZIndexInEdgeAssets holds Edge's stylesheets, page scripts and
// templates to the layer scale in shared/tokens.css (D19).
func TestNoRawZIndexInEdgeAssets(t *testing.T) {
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
					"  If the value carries !important, that is the finding: an !important on a z-index is the escape hatch that makes the scale unenforceable, and the next person copies it.",
					p, raw)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
	}

	scan(staticFS, "static", "static")
	scan(templatesFS, "templates", "templates")

	if scanned == 0 {
		t.Fatal("scanned no Edge assets at all — the walk has drifted and this test is a no-op")
	}
}

// TestOperatorStationZLayerScaleMatchesShared holds the operator station's
// copy of the layer scale to the shared one.
//
// The copy exists because operator-display.html links ONLY
// static/operator-station/operator.css — it does not load shared/tokens.css
// the way header.html does for Edge admin. A var(--z-modal) in operator.css
// with no definition in scope is not an error: it makes the whole z-index
// declaration invalid, the element falls back to `auto`, and the browser says
// nothing. On this screen that lands on #keypad-modal, whose entire job is to
// be above the LoadBin modal that opened it — the exact bug the raise was
// added to fix in the first place.
//
// So the values are duplicated, and this is what makes the duplication safe.
// It compares NAME SETS as well as values: a rung added to shared/tokens.css
// and not to the station would leave the station silently one rung short, and
// a rung the station has invented would be a private ordering.
func TestOperatorStationZLayerScaleMatchesShared(t *testing.T) {
	sharedCSS, err := fs.ReadFile(shared.Files, "tokens.css")
	if err != nil {
		t.Fatalf("read shared/tokens.css: %v", err)
	}
	stationCSS, err := fs.ReadFile(staticFS, "static/operator-station/operator.css")
	if err != nil {
		t.Fatalf("read operator.css: %v", err)
	}

	want := shared.ZLayerScale(string(sharedCSS))
	got := shared.ZLayerScale(string(stationCSS))
	if len(want) == 0 {
		t.Fatal("shared/tokens.css declares no --z-* tokens — nothing to compare against, so this test would pass vacuously")
	}
	if len(got) == 0 {
		t.Fatal("operator.css declares no --z-* tokens in :root.\n" +
			"  It cannot inherit them: operator-display.html does not load shared/tokens.css. Without the local copy every var(--z-*) on this screen resolves to nothing and z-index silently becomes auto.")
	}

	for name, wantVal := range want {
		gotVal, ok := got[name]
		if !ok {
			t.Errorf("operator.css is missing layer token %s (shared/tokens.css has it at %s).\n"+
				"  Copy the whole block; a partial copy is worse than none, because the missing rung fails silently.", name, wantVal)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("layer token %s is %s in operator.css but %s in shared/tokens.css.\n"+
				"  The station's ordering has diverged from the app's. Whichever is right, both copies move together.",
				name, gotVal, wantVal)
		}
	}
	for name, gotVal := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("operator.css declares layer token %s = %s, which shared/tokens.css does not.\n"+
				"  A rung that exists on one surface only is a private ordering — add it to the shared scale or drop it.", name, gotVal)
		}
	}
}
