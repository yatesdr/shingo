package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestNodesEscapeLayerJS runs the Nodes page, unmodified, in the helper pins'
// realm (shared/testdata/helper-pins-harness/world.js) and presses Escape on
// its New node group modal: the innermost entry closes first, and typed work
// is asked about before it is discarded (testdata/static/pages/
// nodes.escape-layer.test.js says each case). Skipped if `node` is not on
// PATH, as the other JS wrappers are. TestCorePagesLeaveEscapeToTheLayer holds
// the same rule over every Core page's source.
func TestNodesEscapeLayerJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "static", "pages", "nodes.escape-layer.test.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Nodes page Escape cases failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("nodes escape: %s", out)
}
