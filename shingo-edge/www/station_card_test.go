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
// runs the card's whole model test: the checklist (the only station counter
// pre-ticks its countable unbound positions while it counts none; once it
// counts one the boxes are the bindings; a second counter pre-ticks nothing;
// the bindings the save sends), the tags, the status, the title and the rows.
func TestStationCard_PreTicksOnlyCountableUnboundPositionsWhenItIsTheOnlyStation(t *testing.T) {
	runStationCardJS(t, "")
}

// TestStationCard_ListsEachCounterWithItsStatus runs the station list's
// checks: one row per counter with its tags, its positions as words and a
// one-line status (amber only when stopped), the checklist only when opened,
// + Add after the last row.
func TestStationCard_ListsEachCounterWithItsStatus(t *testing.T) {
	runStationCardJS(t, "the station list")
}

// TestCountingRows_TheTitleAndTheMESSlots runs the live words and the MES
// slots row: what the process counts on in the section title, the counted
// slot first, in-use slots on their own lines with the re-point, unused and
// unread slots folded.
func TestCountingRows_TheTitleAndTheMESSlots(t *testing.T) {
	runStationCardJS(t, "the title")
	runStationCardJS(t, "the counting rows")
}
