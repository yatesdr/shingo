package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestChangeoverRefreshJS runs the Node harness for changeover.js's live
// refresh: one reload per action's burst of triggers with order traffic kept
// 2 s apart (LC4), and a reload that keeps the chosen target style and its
// open preview (lane F).
func TestChangeoverRefreshJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping changeover refresh test")
	}
	script := filepath.Join("static", "js", "pages", "changeover.refresh.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("changeover refresh test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("changeover refresh: %s", out)
}
