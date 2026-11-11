package protocol

import "testing"

// TestWaitKindStation_TheWireValue pins the station wait kind's value, not its
// name. The Edge stamps it on the steps it authors (wait_kind) and Core's
// fence reads it off them, and the two run different builds: every reader
// compares through the constant (TestWaitOwnershipIsDeclaredOnce), so a
// changed value agrees with itself in each build and with nothing across them.
func TestWaitKindStation_TheWireValue(t *testing.T) {
	t.Parallel()
	if WaitKindStation != "station" {
		t.Errorf("WaitKindStation = %q, want %q: the value crosses the wire between an Edge and a Core of other builds",
			WaitKindStation, "station")
	}
}

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
		{WaitKindTurn, true},
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
