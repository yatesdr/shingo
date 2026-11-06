package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestStationCard_PreTicksOnlyCountableUnboundPositionsWhenItIsTheOnlyStation
// runs the station card's model test under node: the boxes, what is
// pre-ticked, and the bindings the save sends.
func TestStationCard_PreTicksOnlyCountableUnboundPositionsWhenItIsTheOnlyStation(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the station card test")
	}
	out, err := exec.Command(nodePath, filepath.Join("static", "js", "pages", "station-card.test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("station card test failed:\n%s\nerror: %v", out, err)
	}
}
