package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUtilsJSSettingsPage runs the Node-based unit tests for the settings-page
// helpers in utils.js (durations both ways, list collect after remove-first,
// apiResult, settingsPage's dirty tracking and save). Skipped if `node` is not
// on PATH (matches the existing JS test wrappers).
func TestUtilsJSSettingsPage(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join(".", "utils.settings.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("settings page test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("settings page: %s", out)
}
