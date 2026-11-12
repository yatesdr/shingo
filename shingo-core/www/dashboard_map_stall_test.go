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
// dashboard's static route lane and the motion clock under it. Skipped if
// `node` is not on PATH, matching the other JS test wrappers.
//
// The regressions it guards: the lane was once drawn for any robot with an
// order that held still for five seconds (MOVE_LINGER_MS, the chevron-flicker
// threshold), then for one still past 45 s, which at HOP (2026-11-12) read as a
// highlighted route to act on. A robot standing still now draws no lane at
// all; the lane is drawn only for a blocked/faulted order or under
// prefers-reduced-motion, and a freshly loaded fleet draws none.
func TestDashboardMapStallJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("testdata", "static", "pages", "dashboard-map.stall.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard-map stall tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("dashboard-map stall: %s", out)
}
