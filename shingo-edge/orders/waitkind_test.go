package orders

import "testing"

// TestWaitKindStation_MatchesCore pins the Edge's one definition of the
// station wait kind. It must equal Core's dispatch.WaitKindStation, which is
// what Core's release fence and its population partition read; Edge cannot
// import Core, so the literal here is the contract, and a rename has to touch
// this test too. (The comment on the constant used to name a test that did not
// exist; this is it.)
func TestWaitKindStation_MatchesCore(t *testing.T) {
	t.Parallel()
	if WaitKindStation != "station" {
		t.Errorf("orders.WaitKindStation = %q, want %q (dispatch.WaitKindStation)", WaitKindStation, "station")
	}
}
