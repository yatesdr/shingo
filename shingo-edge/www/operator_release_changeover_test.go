package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorReleaseChangeover runs the Node harness for the changeover
// release screen: a changeover's release offers no PULL PARTS (the cart goes
// back with its count, or is declared empty), and the steady-state screen
// keeps it. The server refuses the pull too (release gate G9); this pins what
// the operator is offered.
func TestOperatorReleaseChangeover(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping release changeover test")
	}
	script := filepath.Join("static", "operator-station", "operator-release-changeover.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("release changeover test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("release changeover: %s", out)
}
