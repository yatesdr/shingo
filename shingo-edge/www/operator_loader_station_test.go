package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorLoaderStationJS runs the Node characterisation of the loader and
// unloader station's glass in
// static/operator-station/operator-loader-station.test.js: what each layout
// draws, every act it offers a loader or unloader tile, and what each tap
// posts. Skipped if `node` is not on PATH (matches the other JS test
// wrappers).
func TestOperatorLoaderStationJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-loader-station.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator loader station JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("%s", out)
}
