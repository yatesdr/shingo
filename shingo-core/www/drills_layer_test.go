package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDrillsOnTheLayerJS runs the two drill pop-ups, components/CellDrill.js
// (missions, the heartbeat kiosk) and components/DrillModal.js (overview),
// unmodified in the helper pins' realm (shared/testdata/helper-pins-harness/
// world.js) and holds that each is an entry of the one overlay stack: the page
// behind locked while it is open, Escape over a confirm on it closing the
// confirm first, no close on a drag out of the card, focus in and back, and its
// 60 s idle close and its own close closing its entry and cancelling its read
// (testdata/static/components/drills.layer.test.js says each case). Skipped if
// `node` is not on PATH, as the other JS wrappers are.
func TestDrillsOnTheLayerJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "static", "components", "drills.layer.test.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("drill cases failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("drills: %s", out)
}
