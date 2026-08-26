package www

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A modal taller than the viewport used to bleed off both edges with nothing
// able to scroll it, taking the footer — and therefore Save — with it.
// .modal-overlay is position:fixed and vertically centred, and .modal declared
// neither max-height nor overflow, so the overflow was symmetrical: header
// above the top of the screen, footer below the bottom. Zooming the browser
// out was the only way to reach the button.
//
// MEASURED, not reasoned about, in headless Chrome against the real CSS and
// the real claim-modal markup:
//
//	viewport    before (modal 939px)              after
//	1280x800    top -69, footer +69  UNREACHABLE  clamped 768, reachable, scrolls
//	1366x768    UNREACHABLE                       clamped 736, reachable, scrolls
//	1280x600    UNREACHABLE                       clamped 568, reachable, scrolls
//	1920x1080   reachable (939 < 1080)            966, reachable, no scroll
//
// The last row is why this went unnoticed: on a 1080p desktop the claim editor
// fits and nothing looks wrong. It is the 768- and 800-high laptop and HMI
// screens the floor actually uses where Save is off the bottom.
//
// ── WHAT THIS TEST CAN AND CANNOT ANSWER ──────────────────────────────────
//
// It reads the stylesheet, so it answers "are the declarations still there".
// It does NOT answer "is Save on screen" — that is geometry, and only a render
// can settle it. The declarations are the mechanism; keeping them is necessary
// and not sufficient. If this ever needs to become a real geometry check, the
// instrument is a headless Chrome measurement of the rendered modal, not a
// cleverer regex.
func TestModalDeclaresScrollContainment(t *testing.T) {
	body, err := os.ReadFile("static/css/shingoedge.css")
	if err != nil {
		t.Fatalf("read shingoedge.css: %v", err)
	}
	css := string(body)

	block := func(selector string) string {
		re := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(selector) + `\s*\{(.*?)\}`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			t.Fatalf("selector %q not found in shingoedge.css — if it was renamed, move this guard with it", selector)
		}
		return m[1]
	}

	modal := block(".modal")
	for _, decl := range []string{"max-height", "display: flex", "flex-direction: column"} {
		if !strings.Contains(modal, decl) {
			t.Errorf(".modal is missing %q. Without it a tall modal overflows the viewport in both "+
				"directions and the footer — the Save button — goes off screen. Restore it in "+
				"static/css/shingoedge.css.", decl)
		}
	}

	// The scrolling region. Both spellings are covered because the style guide
	// prescribes .modal-body and every modal in processes.html predates it.
	scroll := block(".modal > .card-body,\n.modal > .modal-body")
	for _, decl := range []string{"overflow-y: auto", "min-height: 0"} {
		if !strings.Contains(scroll, decl) {
			t.Errorf("the modal body rule is missing %q. min-height:0 is load-bearing: a flex item "+
				"defaults to min-height:auto, refuses to shrink below its content, and reinstates "+
				"the overflow this rule exists to remove.", decl)
		}
	}

	for _, sel := range []string{".modal-header", ".modal-footer"} {
		if !strings.Contains(block(sel), "flex: 0 0 auto") {
			t.Errorf("%s must not shrink (flex: 0 0 auto) — it is pinned around the scrolling body.", sel)
		}
	}
}
