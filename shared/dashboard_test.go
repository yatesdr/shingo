package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUtilsJSDashboardPrimitives runs the Node-based unit tests for the
// dashboard primitives (reconcileList, createStore, onSSE) in utils.js.
// Skipped if `node` is not on PATH (matches the existing JS test wrappers).
func TestUtilsJSDashboardPrimitives(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join(".", "utils.dashboard.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard primitives test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("dashboard primitives: %s", out)
}
