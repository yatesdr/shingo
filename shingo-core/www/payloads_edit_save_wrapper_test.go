package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPayloadsEditSaveJS runs static/pages/payloads.edit-save.test.js: the
// payload edit form sends its carrier types and its manifest only once it has
// loaded them, so a save before the load or after a failed one leaves both
// standing. Named _wrapper_test.go, not _js_test.go (a trailing _js is read as
// GOOS=js). Skipped when node is not on PATH, like the other wrappers.
func TestPayloadsEditSaveJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	script := filepath.Join("static", "pages", "payloads.edit-save.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("payloads edit save JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("payloads edit save: %s", out)
}
