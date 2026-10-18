package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestComposerModalJS runs the Node unit test for the station's pick-a-part
// modal in static/operator-station/composer-render.js — which screen each tap
// leads to (two off only with a two-produce recipe, a live style used
// silently, one recipe used, several asked by name, none → an engineer,
// Positions only with the switch on) and the part rows' words (Core's verdict
// pill, the style a part runs as, a CATID). Skipped if `node` is not on PATH
// (matches the other JS test wrappers).
func TestComposerModalJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "composer-modal.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("composer modal JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("composer modal: %s", out)
}
