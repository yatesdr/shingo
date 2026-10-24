package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorFlowGeometryPins runs the disjointness pins: the module
// picture's synthetic cells (the WELD-2 shape with no geometry, the design
// sheet's hard-cases cell, nine positions in one row, a press staging at its
// back positions) and the relation picture's (the Press-6 and Hopkinsville
// press shapes, a drifting weld line, a north-south line, a diagonal, rows of
// six and seven) drawn at every width the surfaces reach — 640 to 1600 in
// steps of 40, plus the station's own 1280x560 — pinning that every drawn
// card is inside the picture, no two cards overlap, and every text, chevron
// and tag fits its card. The relation layout's (c, r) tables, the scope gate,
// the fading, the chevrons and the fallback line are pinned there too. No
// database: the cells are built in the script, on invented coordinates, so
// the pin can ask for shapes no seeded plant carries.
func TestOperatorFlowGeometryPins(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping geometry pins")
	}
	script := filepath.Join("static", "operator-station", "operator-flow.geometry.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("geometry pins failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("geometry: %s", out)
}
