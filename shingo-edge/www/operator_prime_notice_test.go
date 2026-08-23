package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorPrimeNotice runs the Node harness for primeNoticeText — the
// "press again when it lands" sentence.
//
// Round 1 shipped the press-index partial-empty prime and the operator could
// not see it: prime_orders crossed the wire and no consumer read it, so
// REQUEST SWAP appeared to do nothing. Tested at the rendering level because
// the deliverable is a sentence an operator reads, and round 1's changeover
// banner proved a source-level check passes over copy that is wrong out loud.
func TestOperatorPrimeNotice(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping prime-notice test")
	}
	script := filepath.Join("static", "operator-station", "operator-prime-notice.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("prime-notice test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("prime-notice: %s", out)
}
