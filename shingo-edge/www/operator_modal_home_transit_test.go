package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalHomeTransitJS runs the Node-based unit tests for the
// manual_swap demand queue in renderModal
// (static/operator-station/operator-modal.js): which per-payload row a consume
// node's own empty-out (U2) lights while it leaves. The modal keeps its own
// per-payload order filter beside cardModel's, so the two are pinned separately.
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorModalHomeTransitJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-home-transit.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal home transit JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal home transit: %s", out)
}
