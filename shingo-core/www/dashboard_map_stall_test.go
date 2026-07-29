package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: this must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file would be silently excluded from every normal build —
// it compiles, it is tracked, and it never runs. Same trap documented on
// loaders_flow_gating_test.go and sourcing_reload_triggers_test.go.
//
// TestDashboardMapStallJS runs the Node-based unit tests for the robot-map
// dashboard's stall threshold. Skipped if `node` is not on PATH, matching the
// other JS test wrappers.
//
// The regression it guards: the frozen route lane keyed off isMoving, i.e.
// MOVE_LINGER_MS, i.e. five seconds. Five seconds is the threshold that stops a
// robot pivoting at a corner from blinking between chevron and disc — it is not
// a stall threshold, and the module's own comment on MOVE_LINGER_MS says as
// much ("bridges brief stops"). On a live floor, holding still for five seconds
// is what a robot does at a traffic point, at a lift, during a jack cycle, and
// while holding an order it has been assigned but not started. Every one of
// those painted a full route line across the plant, so the drawing that should
// have meant "this robot is stuck, go look" carried no information. The lane
// now keys off STALL_MS, and a robot with no observed history is unknown rather
// than stalled.
func TestDashboardMapStallJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "dashboard-map.stall.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard-map stall tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("dashboard-map stall: %s", out)
}
