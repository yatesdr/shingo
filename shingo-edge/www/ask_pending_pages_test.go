package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDesktopPagesSayAnAskIsPending runs the Node harness for the desktop
// pages' ask-backed actions (static/js/pages/ask-pending.test.js): the
// Production page's LOAD and REQUEST EMPTY and the containment page's hold
// clear show the station's notice on a 202 pending answer, hold the act as the
// station does, and never show a success over an act Core has not answered.
func TestDesktopPagesSayAnAskIsPending(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the desktop pages' pending-ask test")
	}
	script := filepath.Join("static", "js", "pages", "ask-pending.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("desktop pages' pending-ask test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("desktop pages' pending ask: %s", out)
}
