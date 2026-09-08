package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorBootWatchdogJS runs the Node-based unit tests for the boot
// watchdog — the classic inline script in templates/operator-display.html that
// reloads an operator board whose ES module graph failed to link.
//
// THE INCIDENT IT EXISTS FOR. Hopkinsville 2026-09-08: a stray `}` in
// operator-release.js meant the operator-station module graph never linked, so
// operator.js executed zero lines and every HMI sat on the "Loading..."
// placeholder with a server that looked perfectly healthy — services active,
// assets 200, view API 200, no panics. createSSE already reloads a board when
// the build id changes, which would have healed all of them on the next deploy,
// except that it lives INSIDE the graph it protects. The break disabled its own
// recovery and the boards were refreshed by hand on the floor.
//
// What the JS side pins is the handful of ways a watchdog goes subtly wrong: a
// healthy board must not arm (or every tablet holds a second EventSource
// forever), an unchanged build id must not reload (that is a spin loop against
// a still-broken build), and a changed one must reload exactly once. It reads
// the script out of the shipping template rather than a copy, so a watchdog
// that drifts from the page fails here.
//
// Skipped if `node` is not on PATH, matching the other JS test wrappers. Note
// the parse gate scripts/check-js-parses.sh deliberately does NOT skip that
// way: silently passing on unread JS is the failure mode that shipped the
// stray brace in the first place.
func TestOperatorBootWatchdogJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-boot-watchdog.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator boot watchdog JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator boot watchdog: %s", out)
}
