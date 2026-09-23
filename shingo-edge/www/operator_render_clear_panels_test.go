package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorRenderClearPanelsJS runs the Node unit test for the unloader
// window's two panels in static/operator-station/operator-render.js —
// confirmUnloadSwap (CLEAR) and confirmPushEmpty (PUSH EMPTY / PUSH AS) — and
// what each button posts. Skipped if `node` is not on PATH (matches the other
// JS test wrappers).
func TestOperatorRenderClearPanelsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-render-clear-panels.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("operator-render clear panels JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-render clear panels: %s", out)
}
