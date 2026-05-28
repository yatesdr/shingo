package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUtilsJSTableSort runs the Node-based unit tests for the
// installTableSort helper added for Field-notes Note 4. Skipped if
// `node` is not on PATH (matches the other JS test wrappers in this
// package).
func TestUtilsJSTableSort(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join(".", "utils.tablesort.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installTableSort test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("installTableSort: %s", out)
}
