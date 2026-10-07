package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPinOverview_OrderUpdateMaxWaitJS runs static/pages/overview.maxwait.test.js
// (LC9): the Overview's order-update reads fire at least every 10 s under a
// steady stream and are the plain 1.5 s trailing debounce when events are
// sparse. Named _wrapper_test.go, not _js_test.go (a trailing _js is read as
// GOOS=js). Skipped when node is not on PATH, like the other wrappers.
func TestPinOverview_OrderUpdateMaxWaitJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	script := filepath.Join("static", "pages", "overview.maxwait.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("overview max-wait JS tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("overview max-wait: %s", out)
}
