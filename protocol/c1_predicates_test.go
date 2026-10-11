package protocol

import "testing"

// TestIsWaitingIsQueued and the acquiring set: who is asked again, and whose
// cause describes a present wait (BRIEF C1.1.4).
//
// RED at 647b65af: IsAcquiring is {queued, sourcing}; IsWaiting does not exist
// (so this file does not build there — the red is the compile).
func TestIsWaitingIsQueued(t *testing.T) {
	for _, s := range AllStatuses() {
		if got, want := IsWaiting(s), s == StatusQueued; got != want {
			t.Errorf("IsWaiting(%s) = %v, want %v", s, got, want)
		}
		if got, want := IsAcquiring(s), s == StatusPending || s == StatusQueued; got != want {
			t.Errorf("IsAcquiring(%s) = %v, want %v", s, got, want)
		}
		if got, want := IsPreDispatch(s), s == StatusPending || s == StatusQueued; got != want {
			t.Errorf("IsPreDispatch(%s) = %v, want %v", s, got, want)
		}
	}
	if WaitingStatusSQLList() != "'queued'" || AcquiringStatusSQLList() != "'pending','queued'" {
		t.Errorf("SQL twins: waiting %s, acquiring %s", WaitingStatusSQLList(), AcquiringStatusSQLList())
	}
}
