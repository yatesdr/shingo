package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestContainmentJS_FeedsPin runs the containment page's refresh pins under
// node (static/js/pages/containment.feedspin.test.js). Flipped by F2: no poll,
// a reload on the SSE `containment` event, and a reload that arrives while a
// dialog is open held and applied when it closes. Each check carries its base
// value (the 5 s poll, the baseline first poll, the modal that swallowed the
// change) beside it.
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
