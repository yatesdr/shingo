package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestContainmentJS_FeedsPin runs the containment page's refresh pins under
// node (static/js/pages/containment.feedspin.test.js): the 5 s poll, the
// baseline first poll, no reload on unchanged text, and an open modal that
// swallows the change rather than deferring it. Each check carries its
// predicted value once the page reloads on the SSE `containment` event (F2).
func TestContainmentJS_FeedsPin(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the containment JS pins")
	}
	script := filepath.Join("static", "js", "pages", "containment.feedspin.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("containment JS pins failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("containment JS pins: %s", out)
}
