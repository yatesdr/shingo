package www

import (
	"strings"
	"testing"
)

// A MOVE IS NOT A DROPOFF, so an occupied destination must not refuse it.
//
// apiRobotMoveTo used to run the dropoff-capacity gate and answer 409
// "Waiting for a slot at ALN_003 — 1 bin there now" for a command that picks up
// nothing, places nothing, and writes no order row. It refused precisely the
// case the endpoint is most useful for: sending a robot somewhere BECAUSE
// something is there.
//
// Asserted on the source rather than through a live dispatcher, because what
// went wrong was a CALL that should not exist — and the cheapest honest way to
// pin "this handler does not consult bin capacity" is that it does not.
func TestApiRobotMoveTo_DoesNotConsultDropoffCapacity(t *testing.T) {
	t.Parallel()
	src := readSourceFile(t, "handlers_robots.go")
	body := funcBody(t, src, "func (h *Handlers) apiRobotMoveTo(")

	for _, banned := range []string{"rejectIfOccupied", "PreviewDropoffCapacity"} {
		if strings.Contains(body, banned) {
			t.Errorf("apiRobotMoveTo calls %s — a move places no bin, so a bin-slot "+
				"gate can only refuse a command that would have worked", banned)
		}
	}
}

// And the helper goes with it: its only caller was the move handler, and a
// guard kept alive with no caller is one somebody re-applies by pattern-match.
func TestRejectIfOccupied_IsGone(t *testing.T) {
	t.Parallel()
	if strings.Contains(readSourceFile(t, "helpers.go"), "func (h *Handlers) rejectIfOccupied") {
		t.Error("rejectIfOccupied still defined; it has no callers, and the real " +
			"order doors use PreviewDropoffCapacity directly")
	}
}
