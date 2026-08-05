package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalLaneHeldJS runs the Node-based unit tests for
// isStationReleasable (static/operator-station/operator-modal.js) — the
// predicate that decides whether a staged order is offered a RELEASE button.
//
// Core writes `staged` for an order parked at a lane's gate point (the robot
// reaches the wait point, RDS reports WAITING, MapState maps it), so the board
// was being invited to offer a release for a wait whose precondition is a lane
// being safe. Nobody at a station can observe or bring that about, and Core now
// refuses such a release outright — so a surviving button would be one whose
// only correct outcome is an error.
//
// What this pins is the CONTROL/STATUS split: the order still renders with its
// status and reason, and only the button goes. Skipped if `node` is not on PATH
// (matches the other JS test wrappers).
func TestOperatorModalLaneHeldJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-lane-held.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal isStationReleasable JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal isStationReleasable: %s", out)
}
