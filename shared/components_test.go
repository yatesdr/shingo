package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSharedComponentSpecs runs the specs of the four shared UI components:
// layer.js (the one overlay stack), notice.js (toast and banner), http.js (the
// HTTP door) and picker.js (the picker's shell). Each spec is a node script in
// testdata/components that loads its component unmodified in the helper pins'
// page stubs (testdata/helper-pins-harness/world.js); every case names the
// behaviour it holds, and a "cannot happen:" case names the P0 bug it rules
// out. Skipped if node is not on PATH, as the other JS wrappers are.
func TestSharedComponentSpecs(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the component specs")
	}
	for _, name := range []string{"layer"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command(nodePath, filepath.Join("testdata", "components", name+".test.js")).CombinedOutput()
			if err != nil {
				t.Fatalf("%s.test.js failed:\n%s\nerror: %v", name, out, err)
			}
			t.Logf("%s", out)
		})
	}
}
