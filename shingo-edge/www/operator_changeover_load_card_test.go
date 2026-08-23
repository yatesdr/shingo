package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorChangeoverLoadCard renders the loader board's changeover
// directive banner.
//
// Tested by rendering because the deliverable is a sentence an operator reads
// off a board while standing at a loader. Round 1's changeover banner and
// round 2's modal geometry both passed review and failed the moment they were
// put on a screen.
func TestOperatorChangeoverLoadCard(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping changeover load-card render test")
	}
	script := filepath.Join("static", "operator-station", "operator-changeover-load-card.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("changeover load-card render test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("changeover load card: %s", out)
}
