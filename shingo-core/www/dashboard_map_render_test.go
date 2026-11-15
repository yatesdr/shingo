package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: this must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file would be silently excluded from every normal build.
// Same trap documented on dashboard_map_curve_test.go.
//
// TestDashboardMapRenderJS runs the Node pins for the robot map as drawn: the
// comet set and its signature, the 12-robot calm rule, the comet's look as a
// draw list at fixed times, click-to-focus, zoom and rotate, the minimap, and
// the intent line and frozen lane. Skipped if `node` is not on PATH, matching
// the other JS test wrappers.
//
// What it guards: the map's machinery is moving into shared/ with the same
// look. These were recorded at the tree before the move, so a move that keeps
// the look keeps them green, and every change of look (the minimap built once,
// the route model honouring stored direction) flips a named pin on purpose in
// the commit that makes it.
func TestDashboardMapRenderJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("testdata", "static", "pages", "dashboard-map.render.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard-map render pins failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("dashboard-map render: %s", out)
}
