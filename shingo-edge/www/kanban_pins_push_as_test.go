package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestKanbanPinPushAsJS runs testdata/static/operator-station/kanban_pins_push_as.test.js,
// the characterization pins of the unloader's PUSH AS panel. Skipped if `node` is
// not on PATH (matches the other JS test wrappers).
func TestKanbanPinPushAsJS(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("testdata", "static", "operator-station", "kanban_pins_push_as.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("PUSH AS pins failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("PUSH AS pins: %s", out)
}
