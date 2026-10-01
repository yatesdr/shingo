package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorPulledDirectlyJS runs the Node-based unit tests for the loader
// card's sentence at a pulled-directly stage-2 window
// (static/operator-station/operator-pulled-directly.test.js). Skipped if
// `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorPulledDirectlyJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-pulled-directly.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator pulled-directly JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator pulled directly: %s", out)
}
