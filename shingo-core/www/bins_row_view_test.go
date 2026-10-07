package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: this must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file would be silently excluded from every normal build.
//
// TestBinsRowViewJS runs the Node-based unit tests for bins-row.js, the Bins
// table row drawn from a bin's detail answer, and bins-echo.js, the counting of
// the page's own-action echoes (a counted echo is dropped, anything beyond the
// count is a real update; TestPinBins_ActionEveryVerb checks the per-verb
// counts against the handlers). Skipped if `node` is not on PATH, matching the
// other JS test wrappers.
//
// The regression it guards: the row repaint after an action wrote the raw
// _TRANSIT / _ROBOT:<id> node name into Location, dropped the Return button
// and left the sort and search values stale. The row must draw as
// templates/bins.html draws it.
func TestBinsRowViewJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "bins.row.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bins row view tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("bins row view: %s", out)
}
