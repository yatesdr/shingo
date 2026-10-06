package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestComposerStripUnplacedChipJS runs the Node unit test for the composer
// strip's unplaced-part chip in static/operator-station/composer-render.js —
// that it renders as a button carrying the removePart verb, that a placed part
// stays a plain label, and that a tap takes the part off and redraws the
// picture once. Skipped if `node` is not on PATH (matches the other JS test
// wrappers).
func TestComposerStripUnplacedChipJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "composer-strip-unplaced-chip.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("composer strip unplaced-chip JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("composer strip unplaced chip: %s", out)
}
