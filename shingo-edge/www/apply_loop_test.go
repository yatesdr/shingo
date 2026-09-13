package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPresetApplyLoop runs the apply loop's decisions under node.
//
// runPresetApply is the Processes page's only unguarded state machine — it
// walks every ticked style, saves each one and reacts to what comes back — and
// nothing tested it. Its two decisions (the order it saves in, and what a
// response means for a row) are in desktop-bodies.js for the reason the eight
// body builders are: so a test can exercise the page's logic without a DOM,
// and so the page calls the tested thing rather than repeating it.
//
// Both were wrong. The running style was saved in rail order, which moves the
// fingerprint's from-side under every row after it and 409-cascaded through
// them; and a 409 re-previewed and then stopped, leaving the row unwritten.
func TestPresetApplyLoop(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the apply-loop test")
	}
	script := filepath.Join("static", "js", "pages", "apply-loop.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("apply loop test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("apply loop: %s", out)
}
