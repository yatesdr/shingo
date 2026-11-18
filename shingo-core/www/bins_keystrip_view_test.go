package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: not *_js_test.go, which Go reads as GOOS=js.
//
// TestBinsKeyStripJS runs the Node-based unit tests for bins-keystrip.js, a
// family tab's key strip on the Bins page: at rest (each key as saved, nothing
// to save), a field open (one at a time; Escape or a press away changes
// nothing), a key picked and not saved (Save keys and Cancel show), and Cancel
// (both fields back). Skipped if `node` is not on PATH, as the other JS test
// wrappers are.
func TestBinsKeyStripJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "static", "pages", "bins.keystrip.test.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bins key strip failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("bins key strip: %s", out)
}
