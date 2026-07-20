package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: this must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file is silently excluded from every normal build — it
// compiles, it is tracked, and it never runs.
//
// TestSourcingReloadTriggersJS runs the Node-based unit tests for the /sourcing
// page's live-update triggers. Skipped if `node` is not on PATH, matching the
// other JS test wrappers.
//
// The regression it guards: the page shipped with onSSE('connected', reload),
// an infinite loop — load, SSE connects, 'connected' fires, reload, connects
// again — so it pulsed forever on an idle plant (field-observed at Springfield).
// The headline assertion is that 60 simulated seconds of an idle page produce
// zero reloads.
func TestSourcingReloadTriggersJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "sourcing.reload.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sourcing reload-trigger tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("sourcing reload triggers: %s", out)
}
