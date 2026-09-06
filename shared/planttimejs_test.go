package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUtilsPlantTimeJS runs the Node-based unit tests for the plant-local
// time formatters (formatTime/formatClock) and the convertTimestamps
// rollover shim. Skipped if `node` is not on PATH, matching the
// delegateActions wrapper.
//
// The FILE NAME avoids an _js_ segment on purpose: a file named
// foo_js_test.go parses to Go as GOOS=js-constrained and lands in
// IgnoredFiles on windows — the test silently never runs.
func TestUtilsPlantTimeJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join(".", "utils.planttime.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("planttime JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("planttime JS: %s", out)
}
