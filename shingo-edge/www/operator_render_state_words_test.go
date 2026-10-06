package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorRenderStateWordsJS runs the Node test for the station's state
// words in static/operator-station/operator-render.js: the header line, the
// style chip and the footer badge say "Running <style>", "Changing over" or
// "No part running" from the same fields, and an empty grid says why in plain
// words. Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorRenderStateWordsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-render-state-words.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-render state words JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-render state words: %s", out)
}
