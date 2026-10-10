package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalLoaderClearJS runs the Node-based unit tests for loaderOffersClear
// (static/operator-station/operator-modal.js): a loader window holding a carrier
// that is labelled but reads 0 offers CLEAR BIN, the one way to free it (row L
// item 5). Skipped if `node` is not on PATH (matches the other JS test wrappers).
func TestOperatorModalLoaderClearJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-loader-clear.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal loaderOffersClear JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal loaderOffersClear: %s", out)
}
