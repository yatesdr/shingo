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
// TestInventoryLedgerPanelsJS runs the Node-based render tests for the
// /inventory page's dropped-hold and declared-full lists
// (static/pages/inventory.ledger.test.js). Skipped if `node` is not on PATH,
// matching the other JS test wrappers. A kind is not done until the page
// renders it; this is the pin that it does.
func TestInventoryLedgerPanelsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "pages", "inventory.ledger.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("inventory ledger list tests failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("inventory ledger lists: %s", out)
}
