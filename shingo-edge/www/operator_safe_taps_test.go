package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorSafeTapsJS runs the Node tests of the station's safe taps
// (static/operator-station/operator-safe-taps.test.js): a modal closed or
// refused leaves nothing dead or frozen, a finished request closes only the
// modal it came from, and one tap sends one request. Skipped if `node` is
// not on PATH (matches the other JS test wrappers).
func TestOperatorSafeTapsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-safe-taps.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator safe taps JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("%s", out)
}
