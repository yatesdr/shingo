package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorFlowSurfacePins runs what the cell picture does on the surfaces
// that host it: what a selection outlines, what a tap on the composer's
// picture reaches, which robots the legend lists, the card words, the
// tooltip on a cut name, and how the station hosts a picture taller than its
// panel. No database: the cells are synthetic.
func TestOperatorFlowSurfacePins(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping surface pins")
	}
	script := filepath.Join("static", "operator-station", "operator-flow.surface.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("surface pins failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("surfaces: %s", out)
}
