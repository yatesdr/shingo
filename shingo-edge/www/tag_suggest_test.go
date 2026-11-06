package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTagSuggestJS runs the tag combo's option test under node.
func TestTagSuggestJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the tag-suggest test")
	}
	out, err := exec.Command(nodePath, filepath.Join("static", "js", "pages", "tag-suggest.test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("tag-suggest test failed:\n%s\nerror: %v", out, err)
	}
}
