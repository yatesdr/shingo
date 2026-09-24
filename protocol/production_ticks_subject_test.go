package protocol

import (
	"slices"
	"testing"
)

// TestProductionTicks_SubjectIsCoreInboundAndNoExpiry: the batched heartbeat
// feed is a Core-handled subject (the boot coverage assertion checks it has a
// handler) and carries no expiry.
func TestProductionTicks_SubjectIsCoreInboundAndNoExpiry(t *testing.T) {
	t.Parallel()
	if SubjectProductionTicks != "production.ticks" {
		t.Errorf("SubjectProductionTicks = %q", SubjectProductionTicks)
	}
	if !slices.Contains(CoreInboundSubjects(), SubjectProductionTicks) {
		t.Error("production.ticks is not in CoreInboundSubjects")
	}
	if !slices.Contains(CoreInboundSubjects(), SubjectProductionTick) {
		t.Error("production.tick left CoreInboundSubjects; mixed-version Edges still send it")
	}
	if ttl := DataTTLFor(SubjectProductionTicks); ttl != NoExpiry {
		t.Errorf("production.ticks TTL = %v, want NoExpiry", ttl)
	}
}
