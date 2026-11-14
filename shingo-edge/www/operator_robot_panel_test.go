package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorRobotPanelJS runs the Node pins of the robot panel on a cell
// operator screen (testdata/static/operator-station/operator-robot-panel.test.js):
// when it shows and goes (LEAD-NOTES D13), what it draws on the shared scene
// modules (D6, D10), its crop with one robot, a swap and eight, position lost,
// the tap that never reaches the control under it, the toggle remembered per
// screen, {available: false}, and that hidden is zero work for every cause.
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
//
// The panel waits 3 x CORE_POLL_MS (operator-robot-map.js) for an order's
// first position before any frame has said interval_ms. That is a copy of
// Core's robot poll cadence, robotPollInterval (2 s), which the protocol does
// not carry as a constant: the copy is held to the symbol's existence here,
// not to its value (a value pin would read another module's source). It
// forbids changing Core's cadence without changing CORE_POLL_MS.
// EXPIRY: when robotPollInterval is gone.
func TestOperatorRobotPanelJS(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("testdata", "static", "operator-station", "operator-robot-panel.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator robot panel JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("%s", out)
}
