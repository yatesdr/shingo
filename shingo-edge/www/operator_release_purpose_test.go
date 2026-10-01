package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorReleasePurposeJS runs the Node-based unit tests for the board's
// per-purpose RELEASE (static/operator-station/operator-release-purpose.test.js):
// the labelled button a changeover tile shows, and the glow, including an
// unchanged node keeping its glow during another node's changeover (L10).
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorReleasePurposeJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-release-purpose.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator release purpose JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator release purpose: %s", out)
}
