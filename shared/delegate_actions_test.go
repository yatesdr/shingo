package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUtilsJSDelegateActions runs the Node-based unit tests for the
// delegateActions helper. Skipped if `node` is not
// on PATH (matches the existing JS characterization test wrapper).
func TestUtilsJSDelegateActions(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join(".", "utils.delegateactions.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("delegateActions test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("delegateActions: %s", out)
}
