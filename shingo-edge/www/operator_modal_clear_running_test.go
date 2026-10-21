package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalClearRunningJS runs the Node-based unit tests for the node
// modal's header status line in renderModal
// (static/operator-station/operator-modal.js): a loader home whose clear Core
// is running reads as an order in progress, not "No active orders", though
// the home's runtime carries no order pointer.
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorModalClearRunningJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-clear-running.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal clear running JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal clear running: %s", out)
}
