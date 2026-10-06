package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestComposerPickerPillJS runs the Node unit test for the S2 picker rows in
// static/operator-station/composer-render.js — that each sourcing verdict Core
// can send renders its own pill words with Core's note on the row, and that
// the flow summary names positions in full. Skipped if `node` is not on PATH
// (matches the other JS test wrappers).
func TestComposerPickerPillJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "composer-picker-pill.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("composer picker pill JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("composer picker pill: %s", out)
}
