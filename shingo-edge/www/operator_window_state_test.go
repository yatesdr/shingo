package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorWindowStateJS runs the Node-based unit tests for the loader/unloader
// window state model (static/operator-station/operator-window-state.js): cardModel +
// headerModel for both roles, plus the encoded incident cases. The state machine
// drives the operator board's per-payload cards and header badge; this pins them so a
// refactor can't silently change what the board shows or which tap fires. Skipped if
// `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorWindowStateJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-window-state.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-window-state JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-window-state: %s", out)
}
