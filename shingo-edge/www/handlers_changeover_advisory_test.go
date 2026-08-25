package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestChangeoverAdvisoryRenders runs the Node harness for changeover.js's
// unresolved-participants banner.
//
// It RENDERS the thing rather than grepping for it. "Does the advisory reach
// the operator" is a question about what appears on the page, and a source-text
// assertion answers a narrower question that reads identically when the element
// is hidden, the list is empty, or the node names never make it in.
func TestChangeoverAdvisoryRenders(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping changeover advisory render test")
	}
	script := filepath.Join("static", "js", "pages", "changeover.advisory.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("changeover advisory render test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("changeover advisory: %s", out)
}
