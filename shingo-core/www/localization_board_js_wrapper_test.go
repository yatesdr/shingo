package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: it must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file would be silently excluded from every normal build — it
// compiles, it is tracked, and it never runs. Same trap documented on
// dashboard_map_curve_test.go and its siblings, which is why this one is named
// _js_wrapper_test.go instead.
//
// TestLocalizationBoardJS runs the node checks for the robots page's floor
// plan. Skipped when node is not on PATH, matching the other JS wrappers —
// which does mean a gate run on a host without node asserts none of this.
func TestLocalizationBoardJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	script := filepath.Join("static", "pages", "localization-board.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("localization board JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("localization board: %s", out)
}
