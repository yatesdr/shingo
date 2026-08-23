package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorQueueCause runs the Node harness for withQueueCause /
// distinctQueueCauses — the shared shape behind "a parked order says why".
//
// It also pins the constraint the change was scoped by: `queued` and `sourcing`
// keep their own words. The helper appends Core's cause sentence and never
// substitutes for the status, so a later simplification that normalised the two
// parked lifecycles into one label fails here.
func TestOperatorQueueCause(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping queue-cause test")
	}
	script := filepath.Join("static", "operator-station", "operator-queue-cause.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("queue-cause test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("queue-cause: %s", out)
}
