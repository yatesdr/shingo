package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorRefreshJS runs the Node tests of the station's refresh
// (static/operator-station/operator-refresh.test.js): which events rebuild a
// station's view, how often, and what a build costs the screen. Skipped if
// `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorRefreshJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-refresh.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator refresh JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("%s", out)
}
