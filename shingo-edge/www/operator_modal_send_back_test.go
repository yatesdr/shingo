package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalSendBackJS runs the Node-based unit tests for sendBackCard
// (static/operator-station/operator-modal.js): the tote press's SEND BACK line
// rendered from the server's send_back object, deciding nothing. Skipped if
// `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorModalSendBackJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-send-back.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal sendBackCard JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal sendBackCard: %s", out)
}
