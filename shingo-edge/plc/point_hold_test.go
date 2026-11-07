package plc

import (
	"sync/atomic"
	"testing"
)

// TestPoll_AHeldPointCountsItsStrokesWhenLetGo: while the engine holds a
// point (a re-point's catch-up waiting for the new slot's part identity) its
// passes write nothing — no row, no delta, its count where it stood — and the
// first pass after it lets go counts every stroke since, in one row, past the
// jump threshold too (a jump is counted like any delta).
func TestPoll_AHeldPointCountsItsStrokesWhenLetGo(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newTickRigEmitting(t, em) // read at 0; the jump threshold 100
	var held atomic.Bool
	held.Store(true)
	r.mgr.SetPointHold(func(plcName, tagName string) bool { return held.Load() && plcName == rigPLC && tagName == rigTag })

	r.pass(140)
	r.pass(150)
	if got := em.emitted(); len(got) != 0 {
		t.Fatalf("emitted while held = %+v, want nothing", got)
	}
	if rp := r.rp(); rp.LastCount != 0 {
		t.Fatalf("the held point's count = %d, want 0 where it stood", rp.LastCount)
	}
	if ticks := r.shippedTicks(); len(ticks) != 0 {
		t.Fatalf("shipped while held = %+v, want nothing", ticks)
	}

	held.Store(false)
	r.pass(152)
	if got := em.emitted(); len(got) != 1 || got[0] != (emittedDelta{152, "jump"}) {
		t.Errorf("emitted after the hold = %+v, want one {152 jump}: every stroke since, counted", got)
	}
	if ticks := r.shippedTicks(); len(ticks) != 1 || ticks[0].Delta != 152 || ticks[0].CountValue != 152 {
		t.Errorf("shipped = %+v, want one tick of 152 at 152", ticks)
	}
}
