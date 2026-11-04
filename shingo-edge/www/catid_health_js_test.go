package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCatidHealthListJS runs the health strip's renderer test under node.
func TestCatidHealthListJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the catid-health renderer test")
	}
	script := filepath.Join("static", "js", "pages", "catid-health.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("catid-health renderer test failed:\n%s\nerror: %v", out, err)
	}
}
