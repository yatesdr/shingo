package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestLegPickersOfferNoLane runs the leg-picker characterization under node.
//
// The claim and routing saves refuse a lane on every leg. The desktop's three
// routing pickers and its Quality Hold destination picker draw from Core's
// node list, and each one leaves a lane out through one predicate in
// desktop-bodies.js. The script pins the predicate and that every picker asks
// it.
func TestLegPickersOfferNoLane(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the leg-picker test")
	}
	script := filepath.Join("static", "js", "pages", "leg-pickers.characterization.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("leg-picker test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("leg pickers: %s", out)
}
