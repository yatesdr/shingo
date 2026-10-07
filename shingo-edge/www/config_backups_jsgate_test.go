package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestConfigBackupsJS runs the Configuration page's backup-list tests under
// node (static/js/pages/config-backups.test.js). E5 is pinned there through
// delegateActions: a click on a listed backup's Restore posts exactly the
// listed key, quotes, brackets and colons included. Before U3 the key was
// built into the action string and arrived wrapped in quotes with a stray
// bracket.
func TestConfigBackupsJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the config backups JS test")
	}
	script := filepath.Join("static", "js", "pages", "config-backups.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("config backups JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("config backups: %s", out)
}
