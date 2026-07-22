package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: this must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file would be silently excluded from every normal build —
// it compiles, it is tracked, and it never runs. Same trap documented on
// sourcing_reload_triggers_test.go.
//
// TestLoadersFlowGatingJS runs the Node-based unit tests for the loaders
// page's Material-flow gating and the loader-box flow line. Skipped if `node`
// is not on PATH, matching the other JS test wrappers.
//
// The regression it guards: the Material-flow section was hidden wholesale for
// dedicated_positions loaders. Only the outbound half of that was ever true —
// inbound_source is where the Edge retrieves empties FROM, and with it blank
// the whole threshold→empty-to-home chain no-ops at debug level. Springfield
// ran a dedicated loader with a blank inbound and nothing on any screen said
// so. The headline assertion is that gating never blanks a field value:
// submitLoader reads .value off all three inputs on every save, so a disable
// that cleared them would silently drop loader config.
func TestLoadersFlowGatingJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "loaders.flow.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("loaders flow-gating tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("loaders flow gating: %s", out)
}
