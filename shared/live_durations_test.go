package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUtilsJSLiveDurations runs the Node-based unit tests for
// installLiveDurations / renderLiveDurations in utils.js. Skipped if `node` is
// not on PATH (matches the existing JS test wrappers).
func TestUtilsJSLiveDurations(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join(".", "utils.livedurations.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live durations test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("live durations: %s", out)
}
