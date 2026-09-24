package plc

import (
	"sync"
	"testing"
)

// The PLC is the truth (close-out 2b). A counter that leapt past the jump
// threshold and a counter that went backward are both counts: the poll hands
// the engine their delta in the pass that read them, so the units are charged
// to the carrier bound at the read, and both ship to the heartbeat carrying
// their anomaly as a record. Read off the real poll pass over the tick rig,
// with an emitter that records the counter deltas the pass hands the engine.

// emittedDelta is one EmitCounterDelta the poll made.
type emittedDelta struct {
	delta   int64
	anomaly string
}

// deltaRecorder is mockEmitter plus a record of every counter delta.
type deltaRecorder struct {
	mockEmitter
	dmu    sync.Mutex
	deltas []emittedDelta
}

func (d *deltaRecorder) EmitCounterDelta(rpID, processID, styleID, delta, newCount int64, anomaly string) {
	d.dmu.Lock()
	d.deltas = append(d.deltas, emittedDelta{delta: delta, anomaly: anomaly})
	d.dmu.Unlock()
}

func (d *deltaRecorder) emitted() []emittedDelta {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	return append([]emittedDelta(nil), d.deltas...)
}

// TestPLCTruth_JumpEmitsItsDeltaInThePass is the inverted
// TestPin_JumpIsHeldAtThePoll: the jump's units were held for an operator's
// Confirm, which charged whatever carrier was bound by then.
func TestPLCTruth_JumpEmitsItsDeltaInThePass(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newTickRigEmitting(t, em)

	r.pass(10)
	r.pass(500) // +490 over a threshold of 100

	got := em.emitted()
	if len(got) != 2 || got[0] != (emittedDelta{10, ""}) || got[1] != (emittedDelta{490, "jump"}) {
		t.Errorf("emitted deltas = %+v, want {10 \"\"} then {490 jump} in the pass that read it", got)
	}
	if ticks := r.shippedTicks(); len(ticks) != 2 || ticks[1].Anomaly != "jump" || ticks[1].Delta != 490 {
		t.Errorf("shipped = %+v, want the 10 and the 490 jump", ticks)
	}
}

// TestPLCTruth_ResetShipsAsATick is the inverted
// TestPin_ResetIsEmittedButNotShipped: the reset's newCount is parts, and the
// heartbeat gets it with the anomaly on it. (The poll always emitted it; the
// engine dropped it — see engine TestPLCTruth_ResetCountsNewCount.)
func TestPLCTruth_ResetShipsAsATick(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newTickRigEmitting(t, em)

	r.pass(10)
	r.pass(3)

	got := em.emitted()
	if len(got) != 2 || got[1] != (emittedDelta{3, "reset"}) {
		t.Errorf("emitted deltas = %+v, want {10 \"\"} then {3 reset}", got)
	}
	ticks := r.shippedTicks()
	if len(ticks) != 2 || ticks[1].Delta != 3 || ticks[1].Anomaly != "reset" || ticks[1].CountValue != 3 {
		t.Errorf("shipped = %+v, want the 10 and the reset's 3", ticks)
	}
}
