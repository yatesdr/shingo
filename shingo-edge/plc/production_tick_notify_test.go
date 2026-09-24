package plc

import (
	"sync/atomic"
	"testing"
)

// TestProductionTick_NotifiesOncePerShippablePass: the poll pass rings the
// shipper once when it wrote at least one shippable row, and not at all for a
// pass that wrote none (no change, a reset). The ring is a non-blocking signal;
// the poll never waits on the publish.
func TestProductionTick_NotifiesOncePerShippablePass(t *testing.T) {
	t.Parallel()
	r := newTickRig(t)
	var rings atomic.Int64
	r.mgr.SetProductionTickNotifier(func() { rings.Add(1) })

	r.pass(3) // shippable
	if got := rings.Load(); got != 1 {
		t.Fatalf("rings after a shippable pass = %d, want 1", got)
	}
	r.pass(3) // no change
	r.pass(1) // reset
	if got := rings.Load(); got != 1 {
		t.Errorf("rings after two non-shippable passes = %d, want still 1", got)
	}
}
