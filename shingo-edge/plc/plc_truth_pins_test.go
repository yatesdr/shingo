package plc

import (
	"sync"
	"testing"
)

// The PLC-truth pins (close-out 2b): what the poll does today with a counter
// that leapt past the jump threshold and with one that went backward. Each is
// read off the real poll pass over the tick rig, with an emitter that records
// the counter deltas the pass hands the engine.

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

// jumpConfirmed reads operator_confirmed off the rig's one jump row.
func jumpConfirmed(t *testing.T, r *tickRig) bool {
	t.Helper()
	var confirmed bool
	if err := r.db.DB.QueryRow(`SELECT operator_confirmed FROM counter_snapshots WHERE anomaly = 'jump'`).Scan(&confirmed); err != nil {
		t.Fatalf("read the jump row: %v", err)
	}
	return confirmed
}

// TestPin_JumpIsHeldAtThePoll: a pass whose counter leapt past the threshold
// writes its row unconfirmed and ships it to the heartbeat, but hands the
// engine nothing. The units wait for an operator's Confirm.
func TestPin_JumpIsHeldAtThePoll(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newTickRigEmitting(t, em)

	r.pass(10)
	r.pass(500) // +490 over a threshold of 100

	got := em.emitted()
	if len(got) != 1 || got[0] != (emittedDelta{10, ""}) {
		t.Errorf("emitted deltas = %+v, want only {10 \"\"}: the jump is held", got)
	}
	if jumpConfirmed(t, r) {
		t.Errorf("jump row operator_confirmed = true, want false (awaiting the operator)")
	}
	if ticks := r.shippedTicks(); len(ticks) != 2 || ticks[1].Anomaly != "jump" || ticks[1].Delta != 490 {
		t.Errorf("shipped = %+v, want the 10 and the 490 jump", ticks)
	}
}

// TestPin_ResetIsEmittedButNotShipped: a counter that went backward with no
// plausible rollover is a reset. The poll emits newCount as its delta, tagged
// "reset" (the engine then drops it: engine/plc_truth_pins_test.go), and the
// tick feed does not ship it.
func TestPin_ResetIsEmittedButNotShipped(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newTickRigEmitting(t, em)

	r.pass(10)
	r.pass(3)

	got := em.emitted()
	if len(got) != 2 || got[1] != (emittedDelta{3, "reset"}) {
		t.Errorf("emitted deltas = %+v, want {10 \"\"} then {3 reset}", got)
	}
	if ticks := r.shippedTicks(); len(ticks) != 1 {
		t.Errorf("shipped = %+v, want only the first pass (a reset does not ship)", ticks)
	}
}
