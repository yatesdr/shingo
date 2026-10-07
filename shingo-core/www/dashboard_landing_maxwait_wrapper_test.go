package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDashboardLandingMaxWaitJS runs static/pages/dashboard-landing.maxwait.test.js
// (W4): the Active Orders card's refresh debounce fires at least every 10 s under
// a steady order-update stream and is the plain 1.5 s trailing debounce when
// events are sparse. Named _wrapper_test.go, not _js_test.go (a trailing _js is
// read as GOOS=js). Skipped when node is not on PATH, like the other wrappers.
func TestDashboardLandingMaxWaitJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	script := filepath.Join("static", "pages", "dashboard-landing.maxwait.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard max-wait JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("dashboard max-wait: %s", out)
}
