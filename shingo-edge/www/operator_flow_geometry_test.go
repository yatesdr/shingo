package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorFlowGeometryPins runs the disjointness pin: three synthetic
// cells (the WELD-2 shape with no geometry, the design sheet's hard-cases
// cell, and nine positions in one row) drawn at every width the surfaces
// reach — 640 to 1600 in steps of 40, plus the station's own 1280x560 —
// pinning that every drawn card is inside the frame, no two cards overlap,
// and every text fits its card by the character budget. No database: the
// cells are built in the script, so the pin can ask for shapes no seeded
// plant carries.
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
