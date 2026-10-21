package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorBoardModelJS runs the Node unit tests for the loader and
// unloader board's model (static/operator-station/operator-board-model.js):
// the layout from the tiles, the parts a loader tile draws, coverage, the
// changeover instruction, the CLEAR panel's highlight and a window's words.
// Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorBoardModelJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-board-model.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator board model JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("%s", out)
}
