package dispatch

import (
	"testing"

	"shingo/protocol"
)

// TestStepsAsResolved_KeepsTheWaitKind: the capacity-queued intake keeps the
// wire steps verbatim for the replay, and that includes who owns each wait
// (S3). It dropped WaitKind, so a queued plan's waits came back untagged.
func TestStepsAsResolved_KeepsTheWaitKind(t *testing.T) {
	t.Parallel()
	out := stepsAsResolved([]protocol.ComplexOrderStep{
		{Action: protocol.ActionWait, Node: "SYN-PRESS", WaitKind: protocol.WaitKindStation},
		{Action: protocol.ActionPickup, Node: "SYN-PRESS"},
	})
	if out[0].WaitKind != WaitKindStation {
		t.Errorf("stepsAsResolved dropped the wait kind: %+v", out[0])
	}
}
