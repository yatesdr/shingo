package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorRobotPanelJS runs the Node pins of the robot panel on a cell
// operator screen (testdata/static/operator-station/operator-robot-panel.test.js):
// when it shows and goes (LEAD-NOTES D13), what it draws on the shared scene
// modules (D6, D10), each robot's destination accented (D16), driving read
// from the positions after a Core-side release (owner's F4), its crop with
// one robot, a swap and eight, position lost,
// the tap that never reaches the control under it, the toggle remembered per
// screen, {available: false}, and that hidden is zero work for every cause.
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
//
// The panel holds no copy of Core's robot poll cadence: it counts a missing
// first position against the interval_ms the stream's {available} frame
// carries (protocol.RobotPollInterval, D17; the frame is pinned by
// TestRobotStream_AvailableOnConnectAndOnEveryChange).
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
