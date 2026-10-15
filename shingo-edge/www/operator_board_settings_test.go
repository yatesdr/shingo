package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorBoardSettingsJS runs the Node-based unit tests for the loader
// board's station settings, hiding idle cards and listing every part
// (static/operator-station/operator-board-settings.test.js). Skipped if `node`
// is not on PATH (matches the other JS test wrappers).
func TestOperatorBoardSettingsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-board-settings.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator board settings JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator board settings: %s", out)
}
