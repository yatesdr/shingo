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
// TestMissionDetailLegFormsJS runs the Node-based unit tests for the mission
// detail leg bar's three duration forms. Skipped if `node` is not on PATH,
// matching the other JS test wrappers.
//
// What it guards: `no data`, `zero` and `not applicable` are three different
// answers, and the bar shipped able to render only two. A block whose
// startTime equals its terminateTime rendered `0s` — correct for one that ran
// and finished inside the vendor's one-second resolution, and a false
// measurement for a trailing block on a mission the fleet STOPPED, where the
// equal stamps are the teardown recording blocks it never executed. The
// headline assertions are both directions: a trailing zero on a STOPPED
// mission reads `not run` and claims no share of the timeline, and a zero
// anywhere else is still a reading and is still drawn.
func TestMissionDetailLegFormsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "mission-detail.legs.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mission-detail leg-form tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("mission detail leg forms: %s", out)
}
