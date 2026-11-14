package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProcessesDesktopRouteJS runs the desktop process map's route pin under
// node: the page's own travelGraph, route and graphNodeFor over the synthetic
// route scene, recorded. The desktop graph is directed and weighs arcs, the
// rule the shared route model adopts, so these are the vectors that model must
// reproduce; the pin goes when the desktop map does. Skipped when node is
// absent, like the other JS wrappers.
func TestProcessesDesktopRouteJS(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the desktop route pin")
	}
	script := filepath.Join("testdata", "static", "js", "pages", "processes-desktop.route.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("desktop route pin failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("desktop route: %s", out)
}
