package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDesktopBodiesCharacterization freezes every request body the Processes
// page's surviving sheets send. The list-row Edit sheet is deleted on the
// one-editor-per-fact ruling — its four facts each already have an editor —
// and this pin is the proof its deletion moved nothing else: the bodies the
// survivors send read the same before and after. If a body must change, that
// is a ruling to write down, not a silent edit.
func TestDesktopBodiesCharacterization(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the desktop-bodies characterisation")
	}
	script := filepath.Join("static", "js", "pages", "desktop-bodies.characterization.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("desktop-bodies characterisation failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("desktop bodies: %s", out)
}
