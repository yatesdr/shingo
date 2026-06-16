package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorRenderDrainJS runs the Node unit test for the drain-slot decision logic
// in static/operator-station/operator-render.js — isDrainSlot (which manual_swap slots
// render/route as consume "drain" slots) and drainColorClass (the loader-board palette
// mapping). These drive the unloader's slot-only tap + colors. Skipped if `node` is not
// on PATH (matches the other JS test wrappers).
func TestOperatorRenderDrainJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-render-drain.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-render drain JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-render drain: %s", out)
}
