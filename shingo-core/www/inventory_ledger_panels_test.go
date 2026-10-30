package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// NOTE ON THE FILENAME: this must not end in _js_test.go. Go reads a trailing
// _js before _test.go as an implicit GOOS=js build constraint (the WASM
// target), so the file would be silently excluded from every normal build.
// Same trap documented on sourcing_reload_triggers_test.go.
//
// TestInventoryLedgerPanelsJS runs the Node-based render tests for the count
// ledger lists on /inventory/count-history — the dropped holds, the
// declared-full stamps and the bins below zero — and for the two sections
// count history gave away, where they went: the keeper's levels on the Nodes
// page, the lineside buckets on the Bins page
// (static/pages/count-history.ledger.test.js). Skipped if `node` is not on
// PATH, matching the other JS test wrappers. A kind is not done until the page
// renders it; this is the pin that it does.
func TestInventoryLedgerPanelsJS(t *testing.T) {
	runPageNodeTest(t, "count-history.ledger.test.js", "count history ledger lists")
}

// TestInventoryLoopJS runs the Node-based render tests for /inventory's one
// table (static/pages/inventory.loop.test.js): the doubt line, the chips, the
// fold, the meter and the units.
func TestInventoryLoopJS(t *testing.T) {
	runPageNodeTest(t, "inventory.loop.test.js", "inventory table")
}

// runPageNodeTest runs one static/pages Node test file.
func runPageNodeTest(t *testing.T, file, what string) {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	out, err := exec.Command(nodePath, filepath.Join("static", "pages", file)).CombinedOutput()
	if err != nil {
		t.Fatalf("%s tests failed:\n%s\nerror: %v", what, out, err)
	}
	t.Logf("%s: %s", what, out)
}
