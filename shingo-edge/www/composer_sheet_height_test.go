package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestComposerSheetNeverExceedsTheViewport pins .os-comp-sheet's height.
//
// The sheet is `position: absolute; bottom: 0; height: 700px` inside
// .os-comp-root, which is the station viewport's own box. On a short screen —
// the RDS host's 1366x768 laptop, a browser window not maximised — 700 px is
// taller than the frame, so the sheet's rounded top and, with it, the close
// affordance sit above the top edge of the screen with no way to reach them.
// Every other overlay on this edge respects the viewport with a
// max-height: calc(100vh - …) rule; the sheet predates that rule and kept a
// bare height.
//
// THE FIX IS min(), NOT max-height. `height: min(700px, calc(100vh - 24px))`
// keeps the sheet at its designed 700 px on every screen that has the room and
// gives way only when the frame is shorter — one declaration, no second
// property to keep in step with the height.
//
// NO LAYOUT ENGINE IN THIS TEST, so the pin is over the declaration itself:
// the sheet's block must carry height: min(700px, calc(100vh - 24px)) and must
// not carry a bare height that would override it. The rendered result — what
// the sheet actually measures on a 768-tall screen — is the shots harness's,
// which is the only place with a browser.
func TestComposerSheetNeverExceedsTheViewport(t *testing.T) {
	t.Parallel()

	css, err := os.ReadFile(filepath.Join("static", "operator-station", "composer.css"))
	if err != nil {
		t.Fatalf("read composer.css: %v", err)
	}

	// The .os-comp-sheet rule: the selector's block, up to the next `}`.
	re := regexp.MustCompile(`(?s)\.os-comp-sheet\s*\{([^}]*)\}`)
	m := re.FindSubmatch(css)
	if m == nil {
		t.Fatal("composer.css has no .os-comp-sheet rule — the selector moved, and this pin with it")
	}
	block := string(m[1])

	const want = "height: min(700px, calc(100vh - 24px))"
	if !strings.Contains(block, want) {
		t.Errorf(".os-comp-sheet's height is not %q.\n  The sheet is bottom-anchored at a fixed 700px, so on a screen shorter "+
			"than that its top — with the close affordance — is above the viewport edge. "+
			"Block as read:\n%s", want, block)
	}
	// A bare height later in the same block would override the min() — guard
	// the pin against the "fix" that adds a second height and keeps the first.
	// Comments are stripped first: they explain the rule and may name it.
	withoutComments := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(block, "")
	if got := len(regexp.MustCompile(`height\s*:`).FindAllString(withoutComments, -1)); got != 1 {
		t.Errorf(".os-comp-sheet declares height %d times; the min() must be the only one.\n%s", got, block)
	}
}
