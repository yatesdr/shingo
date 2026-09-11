package www

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOperatorModalPairWaitJS runs the Node-based unit tests for
// pairWaitingLabel (static/operator-station/operator-modal.js) — the label on
// the disabled pair "waiting" button — and for which arm of cellCardAction a
// pair takes to reach it.
//
// WHICH PAIRS GET IT is the half that decides something. The arm keys on two
// linked live legs and on releases_as_pair, the server's declared answer to "is
// this pair released together". It tested swap_mode === 'two_robot', which left
// a press-index pair — released as one too — on a single leg's button.
//
// THE PARK IS ONE WAIT, NOT TWO. A held pair is one object at the release click
// and two objects at rest, and the rest state is what a person is looking at
// when a cell has stopped. This arm used to render a SINGLE leg's cause, chosen
// by list order, so a pair parked for two different reasons showed one of them —
// and the actionable half could be the one dropped. It now carries the position,
// the leg to watch, and every distinct cause across the pair.
//
// It changes no decision: the >=2 guard, the release affordance and the
// admission gates are untouched. Skipped if `node` is not on PATH (matches the
// other JS test wrappers).
func TestOperatorModalPairWaitJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	scriptPath := filepath.Join("static", "operator-station", "operator-modal-pair-wait.test.js")
	cmd := exec.Command(nodePath, scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator-modal pairWaitingLabel JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("operator-modal pairWaitingLabel: %s", out)
}
