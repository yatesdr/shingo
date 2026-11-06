package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// runStationCardJS runs the station card's model test under node: the checks
// whose names hold only ("" runs all).
func runStationCardJS(t *testing.T, only string) {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the station card test")
	}
	out, err := exec.Command(nodePath, filepath.Join("static", "js", "pages", "station-card.test.js"), only).CombinedOutput()
	if err != nil {
		t.Fatalf("station card test failed:\n%s\nerror: %v", out, err)
	}
}

// TestStationCard_PreTicksOnlyCountableUnboundPositionsWhenItIsTheOnlyStation
// runs the card's whole model test: the boxes, what is pre-ticked, the
// bindings the save sends, and what the card measures.
func TestStationCard_PreTicksOnlyCountableUnboundPositionsWhenItIsTheOnlyStation(t *testing.T) {
	runStationCardJS(t, "")
}

// TestStationCard_ShowsLeadBesideRatio: the card says the parts in the line
// since the program change beside the clicks per part, "0 ahead" as a counter
// that counts like the production counter, and the binding confirm names the
// number.
func TestStationCard_ShowsLeadBesideRatio(t *testing.T) {
	runStationCardJS(t, "the lead")
}
