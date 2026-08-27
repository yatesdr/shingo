package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorReleaseSingleOutcome runs the Node harness for
// singleOutcomeReleaseBody — the guard that decides whether tapping RELEASE
// opens the disposition modal.
//
// Both plants reported having to tap a similar button twice for one action.
// The release prompt was the cause: on a produce node the engine discards the
// disposition entirely, and on a consume node at zero UoP with no payload
// chips the prompt renders one actionable button, so RELEASE was answered with
// RELEASE FULL / RELEASE EMPTY. The harness pins which shapes skip the prompt
// AND which must keep it — the remaining_uop > 0 case is the operator choosing
// between a partial return and an under-count declaration, and collapsing that
// would fabricate a missing-inventory record.
func TestOperatorReleaseSingleOutcome(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping release single-outcome test")
	}
	script := filepath.Join("static", "operator-station", "operator-release-single-outcome.test.js")
	out, err := exec.Command(nodePath, script).CombinedOutput()
	if err != nil {
		t.Fatalf("release single-outcome test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("release single-outcome: %s", out)
}
