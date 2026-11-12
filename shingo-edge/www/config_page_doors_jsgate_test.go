package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestConfigPageDoorsJS runs the Configuration page, unmodified, in the helper
// pins' realm (shared/testdata/helper-pins-harness/world.js) and presses its
// own buttons against each answer (testdata/static/js/pages/config.doors.test.js):
// a refusal says the server's sentence, no answer the door's, and a lapsed
// session (the 303 to the login page) is never "Backed up.", "The storage
// answered.", an empty backup list or "Password changed": the notice carries
// "Signed out" with its Sign-in link and the line says only that it failed.
func TestConfigPageDoorsJS(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the config page JS test")
	}
	script := filepath.Join("testdata", "static", "js", "pages", "config.doors.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("config page door cases failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("config page doors: %s", out)
}
