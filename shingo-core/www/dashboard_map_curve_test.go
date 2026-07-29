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
// dashboard_map_stall_test.go, loaders_flow_gating_test.go and
// sourcing_reload_triggers_test.go.
//
// TestDashboardMapCurveJS runs the Node-based unit tests for the robot map's
// curved-segment rendering. Skipped if `node` is not on PATH, matching the
// other JS test wrappers.
//
// The regression it guards: shingo read a lane's className, learned it was a
// BezierPath or a DegenerateBezier, and drew the straight chord anyway,
// because SEER's two cubic control handles were discarded at JSON decode
// three layers upstream. At Springfield the widest of those lanes, LM10-LM113,
// bows 1.30 m off its 7.17 m chord, so robots visibly drove off the network as
// painted, and every route length derived from the same chord ran up to 24%
// short of the distance actually driven.
func TestDashboardMapCurveJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "dashboard-map.curve.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard-map curve tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("dashboard-map curve: %s", out)
}
