package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAppEnterSubmitsJS runs static/app.entersubmits.test.js (W5): Enter in a
// data-action-keydown="enterSubmits:<verb>" input calls <verb> from the page's
// delegateActions map. Named _wrapper_test.go, not _js_test.go: a trailing _js
// is read as GOOS=js and the file would never build (see
// localization_board_js_wrapper_test.go). Skipped when node is not on PATH,
// like the other JS wrappers.
func TestAppEnterSubmitsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	script := filepath.Join("static", "app.entersubmits.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("enterSubmits JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("enterSubmits: %s", out)
}
