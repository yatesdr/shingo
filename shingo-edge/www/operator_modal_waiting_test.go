package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalWaitingJS runs the Node-based unit tests for waitingLabel
// (static/operator-station/operator-modal.js) — the label on the disabled
// two-robot "waiting" button.
//
// That arm sits above the inFlight arm in renderModal's if/else chain, so it is
// the ONLY thing a two-robot swap operator sees while they cannot act. It used
// to be a bare "WAITING FOR OTHER ROBOT" with no reason attached; Springfield
// ALN_003 2026-07-31 spent 32 minutes on it while the supply leg faulted three
// times, and the abandon sweep then cancelled both legs. This pins that the
// reason keeps reaching the operator. Skipped if `node` is not on PATH (matches
// the other JS test wrappers).
func TestOperatorModalWaitingJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-waiting.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal waitingLabel JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal waitingLabel: %s", out)
}
