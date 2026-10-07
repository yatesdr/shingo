package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestLiveScheduleJS runs the Node-based tests for static/pages/live-schedule.js
// and the Overview hero's live wiring (LC9, LC11): one read per burst of
// events, nothing in a hidden tab and one catch-up when shown, robot-update
// making no request and reading the alerts at most once per 30 s. Skipped if
// `node` is not on PATH, matching the other JS test wrappers. (Not named
// *_js_test.go: Go would read the _js suffix as GOOS=js and never build it.)
func TestLiveScheduleJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "live-schedule.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("live-schedule tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("live schedule: %s", out)
}
