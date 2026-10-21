package protocol

import "testing"

// TestWaitKindsPartition: every wait kind is exactly one side's, and a kind
// this build does not know is Core's — never station-releasable. A newer Core
// can send a kind an older Edge has never heard of; the Edge must wait for
// Core rather than offer the operator a RELEASE Core will refuse.
func TestWaitKindsPartition(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind string
		core bool
	}{
		{"", false}, // the drain window: an untagged wait, or a Core that sends no kind
		{WaitKindStation, false},
		{WaitKindLane, true},
		{WaitKindVacate, true},
		{"supervisor", true}, // a kind added after this build
		{"Station", true},    // not the station's spelling, so not the station's
	} {
		if got := IsCoreWaitKind(c.kind); got != c.core {
			t.Errorf("IsCoreWaitKind(%q) = %v, want %v", c.kind, got, c.core)
		}
		if IsStationWaitKind(c.kind) == IsCoreWaitKind(c.kind) {
			t.Errorf("kind %q is owned by both sides or neither (station=%v core=%v); every kind is exactly one side's",
				c.kind, IsStationWaitKind(c.kind), IsCoreWaitKind(c.kind))
		}
	}
}
