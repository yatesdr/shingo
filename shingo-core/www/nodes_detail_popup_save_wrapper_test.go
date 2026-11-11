package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestNodesDetailPopupSaveJS runs static/pages/nodes-detail.popup-save.test.js:
// the node pop-up writes nothing until the detail for the node it shows has
// loaded, then writes only the group settings the person changed, awaited, and
// a failed load or write stops the save with the reason shown. Named
// _wrapper_test.go, not _js_test.go (a trailing _js is read as GOOS=js).
// Skipped when node is not on PATH, like the other wrappers.
func TestNodesDetailPopupSaveJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	script := filepath.Join("testdata", "static", "pages", "nodes-detail.popup-save.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node pop-up save JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("node pop-up save: %s", out)
}
