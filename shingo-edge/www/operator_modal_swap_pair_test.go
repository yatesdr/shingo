package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalSwapPairJS runs the Node-based unit tests for swapPair
// (static/operator-station/operator-modal.js) — which two orders at a node the
// two-robot waiting arm treats as the coordinated pair.
//
// It used to treat the node's whole active-order list as the pair, which is
// wrong in a way that only shows up when something is left over in it: the
// blocker label picked the OLDEST non-parked order, and the >=2 guard that
// releases the recovery surface counted strangers. `delivered` is not terminal
// and a Core-side confirm had no path to the Edge, so a finished order could
// linger in that list for hours. Springfield ALN_001 2026-08-03 spent 13 minutes
// with no RELEASE button and a label naming the style being changed away from,
// quoted off an order finished at 20:33.
//
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorModalSwapPairJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-swap-pair.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal swapPair JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal swapPair: %s", out)
}
