package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalDepartedJS runs the Node-based unit tests for cellCardAction
// (static/operator-station/operator-modal.js) — the function that decides which
// single button a swap cell's card offers.
//
// The bug behind the departed-leg work was a HIDDEN BUTTON, not a wrong backend
// answer. Springfield press trial 2026-09-02: under the IndexRobotSupplies flip,
// R1 clears the press and drives the full tote to the supermarket while R2 puts
// the fresh carrier on. Every position the cell needed was filled and the
// backend would have accepted the next swap — but the card computed `inFlight`
// over every non-terminal order at the node, found the departed R1, and rendered
// a disabled ROBOT IN TRANSIT for the length of a supermarket round trip.
//
// What this pins is the same CONTROL/STATUS split the lane-held test pins for
// RELEASE: a departed leg is still listed, labelled TO MARKET, and drives none
// of the card's four decisions. Skipped if `node` is not on PATH (matches the
// other JS test wrappers).
func TestOperatorModalDepartedJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-departed.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal cellCardAction JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal cellCardAction: %s", out)
}
