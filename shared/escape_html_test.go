package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUtilsEscapeJS runs utils.escape.test.js: the HTML escape helper's
// output on one input table. Skipped if `node` is not on PATH, matching the
// other JS wrappers here. Its own file because no wrapper here runs more than
// one script, and the escape is not a clock or a table concern.
func TestUtilsEscapeJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	out, err := exec.Command(nodePath, filepath.Join(".", "utils.escape.test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("escape JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("escape JS: %s", out)
}
