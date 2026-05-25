package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProcessesJSClaimEditorCharacterization runs the Node-based
// characterization test for processes.js's claim editor. The test
// exercises every (role, swap_mode) cell through the actual JS code,
// asserting visibility of every fieldset/group and the JSON body that
// saveClaim() would POST.
//
// These tests pin behavior at the moment the UI consistency refactor
// began. The processes.js rewrite (state-driven editor) must continue
// to satisfy every assertion in characterization.test.js. A silent
// change to which fields show/require/POST is caught here at CI time.
//
// The Node script is self-contained: it builds a minimal DOM stub,
// loads processes.js via vm.runInContext, and asserts. No npm install
// required. If `node` is missing the test is skipped; the source-level
// JS file is then exercised by the developer's local harness.
func TestProcessesJSClaimEditorCharacterization(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping characterization test (set up a Node runtime to run it)")
	}

	scriptPath := filepath.Join("static", "js", "pages", "processes.characterization.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("characterization test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("characterization: %s", out)
}
