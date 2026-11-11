package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestComposerBootJS runs the Node tests of the station's deferred loads
// (testdata/static/operator-station/composer-boot.test.js): what
// composer-boot.js requests, in what order, and what a failed request leaves
// for the next tap. Skipped if `node` is not on PATH (matches the other JS
// test wrappers).
func TestComposerBootJS(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("testdata", "static", "operator-station", "composer-boot.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("composer-boot JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("%s", out)
}
