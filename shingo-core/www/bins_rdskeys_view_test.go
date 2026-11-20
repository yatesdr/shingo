package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: not *_js_test.go, which Go reads as GOOS=js.
//
// TestBinsRDSKeysViewsJS runs the Node-based unit tests for bins-rdskeys.js,
// the RDS keys adapter the Bin Types table's Lift and Wait fields draw the
// shared picker through: each field opens on its own view, which keeps a key
// by RDS's operation and never by its name; the default and the saved key are
// in every view; a key with no operation is under All only. Skipped if `node`
// is not on PATH, as the other JS test wrappers are.
func TestBinsRDSKeysViewsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "static", "pages", "bins.rdskeys.test.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bins RDS keys views failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("bins RDS keys views: %s", out)
}
