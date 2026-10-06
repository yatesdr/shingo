package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: this must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file is silently excluded from every normal build.
//
// TestMissionDetailStagesJS runs the Node tests for the mission-detail page
// (static/pages/mission-detail.stages.test.js). Skipped if `node` is not on
// PATH, matching the other JS test wrappers; gate.sh refuses to run without it.
func TestMissionDetailStagesJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	cmd := exec.Command(nodePath, filepath.Join("static", "pages", "mission-detail.stages.test.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mission-detail stage tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("mission detail stages: %s", out)
}
