package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFleetNoticesJS runs the Node unit tests for relevantNotices
// (static/pages/fleet-notices.js). Skipped if `node` is not on PATH, matching
// the other JS test wrappers.
//
// What it guards: an assigned order showing other robots' reasons for not
// taking it (SPR 6903, 2026-09-24), and an unassigned one losing the robot that
// somebody may need to re-enable.
func TestFleetNoticesJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	cmd := exec.Command(nodePath, filepath.Join("static", "pages", "fleet-notices.test.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fleet-notice tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("fleet notices: %s", out)
}
