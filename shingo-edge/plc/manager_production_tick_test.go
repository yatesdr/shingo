package plc

import (
	"testing"
	"time"

	"shingo/protocol/clock"
)

// TestProductionTick_PreservesPerTickAcrossBinSwapGap (P1) proves the
// PRESERVING half of the §8 #13 / §12 premise: the heartbeat feed carries
// exactly one wire event per shippable counter tick, each with its own
// recorded_at and count value, regardless of bin-binding state — because it is
// taken in the PLC poll UPSTREAM of the engine's inventory hold-and-replay.
//
// It drives the real poll pass, so the snapshot INSERT, the style gate and the
// ship filter are all on the path. The sequence has three ticks "while bin A is
// bound", two in "the finalize→new-empty-bin gap" (the engine is not involved
// here, which is the point: nothing upstream of hold-and-replay can lump them),
// a tick carrying three strokes, and a jump — which still ships, because the
// heartbeat must know the cell fired while inventory attribution waits on the
// operator (§8 #20).
//
// Each wire event carries all seven fields; recorded_at is stamped in Go at
// poll time with sub-second precision.
//
// The DESTRUCTIVE half (the BinUOPDelta stream lumping the gap ticks) is proven
// in engine/wiring_counter_delta_holdreplay_test.go.
//
// Survived the move off the outbox unchanged: the assertion is on the wire
// event, and only shippedTicks changed with the transport.
func TestProductionTick_PreservesPerTickAcrossBinSwapGap(t *testing.T) {
	t.Parallel()
	r := newTickRig(t)
	rp := r.rp()
	if rp.ProcessID != r.proc || rp.StyleID != r.sty {
		t.Fatalf("rig reporting point = process %d style %d, want %d/%d", rp.ProcessID, rp.StyleID, r.proc, r.sty)
	}

	type step struct {
		count   int64
		delta   int64
		anomaly string
	}
	steps := []step{
		{1, 1, ""}, {2, 1, ""}, {3, 1, ""}, // bin A bound
		{4, 1, ""}, {5, 1, ""}, // swap gap
		{8, 3, ""},         // three strokes inside one poll interval
		{600, 592, "jump"}, // unconfirmed PLC gap
	}
	before := clock.Now().UTC()
	for _, s := range steps {
		r.pass(s.count)
	}
	after := clock.Now().UTC()

	got := r.shippedTicks()
	if len(got) != len(steps) {
		t.Fatalf("wire events = %d, want %d (one per shippable tick, no lumping)", len(got), len(steps))
	}
	subSecond := false
	var last time.Time
	for i, w := range got {
		s := steps[i]
		if w.CountValue != s.count || w.Delta != s.delta || w.Anomaly != s.anomaly {
			t.Errorf("tick %d: count/delta/anomaly = %d/%d/%q, want %d/%d/%q",
				i, w.CountValue, w.Delta, w.Anomaly, s.count, s.delta, s.anomaly)
		}
		if w.EdgeSnapshotID <= 0 {
			t.Errorf("tick %d: EdgeSnapshotID = %d, want the counter_snapshots id", i, w.EdgeSnapshotID)
		}
		if i > 0 && w.EdgeSnapshotID == got[i-1].EdgeSnapshotID {
			t.Errorf("tick %d: EdgeSnapshotID repeats %d", i, w.EdgeSnapshotID)
		}
		if w.ProcessID != r.proc || w.StyleID != r.sty {
			t.Errorf("tick %d: process/style = %d/%d, want %d/%d", i, w.ProcessID, w.StyleID, r.proc, r.sty)
		}
		if w.Station != "stn-test" {
			t.Errorf("tick %d: envelope station = %q, want stn-test", i, w.Station)
		}
		if w.RecordedAt.IsZero() {
			t.Fatalf("tick %d: recorded_at is zero", i)
		}
		// Millisecond precision on the wire: the stamp lies inside the poll
		// window at ms resolution.
		if w.RecordedAt.Before(before.Truncate(time.Millisecond)) || w.RecordedAt.After(after) {
			t.Errorf("tick %d: recorded_at %v outside the poll window [%v, %v]", i, w.RecordedAt, before, after)
		}
		if w.RecordedAt.Nanosecond()/int(time.Millisecond) != 0 {
			subSecond = true
		}
		if !last.IsZero() && w.RecordedAt.Before(last) {
			t.Errorf("tick %d: recorded_at %v before previous %v", i, w.RecordedAt, last)
		}
		last = w.RecordedAt
	}
	if !subSecond {
		t.Errorf("no tick carried a non-zero millisecond — recorded_at is being truncated to the second")
	}
}

// TestProductionTick_ShipFilter (P2) pins what does NOT reach the wire: a
// reset, a pass with no change, and a reporting point with no style
// (manager.go: the early return on delta == 0, the StyleID gate, and the
// `delta > 0 && anomaly != "reset"` guard). A shipper that reads
// counter_snapshots must apply the same filter the poll applied inline.
func TestProductionTick_ShipFilter(t *testing.T) {
	t.Parallel()
	r := newTickRig(t)

	r.pass(10) // shippable: delta 10
	r.pass(10) // no change: no snapshot, no tick
	r.pass(3)  // backward: reset, snapshot written, no tick

	// Style 0: the poll's gate. Driven with the struct because the stored
	// reporting point always names a style.
	rp := r.rp()
	rp.StyleID = 0
	r.setCount(7)
	r.mgr.pollReportingPoint(rp)

	got := r.shippedTicks()
	if len(got) != 1 {
		t.Fatalf("wire events = %d (%+v), want 1: only the first pass is shippable", len(got), got)
	}
	if got[0].CountValue != 10 || got[0].Delta != 10 {
		t.Errorf("shipped tick count/delta = %d/%d, want 10/10", got[0].CountValue, got[0].Delta)
	}
}

// TestProductionTick_ReachesTransportWithinThePass (P10) guards the live
// display: a tick must reach the transport within the poll pass that saw it,
// with no flush interval in between. A 5 s coalescing hold would make every
// cell with a sub-6 s target read "slowed" or "micro-stop" on the tile
// (heartbeat.go state thresholds), so the hold is the regression this blocks.
//
// The transport is now the shipper goroutine, rung by the pass: the running
// shipper, woken by the pass's ring and nothing else, has published the tick
// well inside one poll interval. (It used to be the outbox row, written inside
// the pass.)
func TestProductionTick_ReachesTransportWithinThePass(t *testing.T) {
	t.Parallel()
	r := newTickRig(t)
	s := r.ship()
	s.Start()
	t.Cleanup(s.Stop)
	// Start ships what is already pending; let that settle so the ring below is
	// the only thing that can deliver the tick.
	time.Sleep(50 * time.Millisecond)
	r.mgr.SetProductionTickNotifier(s.Notify)

	r.pass(1)
	deadline := time.Now().Add(500 * time.Millisecond)
	for len(r.decodeSent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.decodeSent(); len(got) != 1 {
		t.Fatalf("500 ms after one pass: %d ticks at the transport, want 1 — the tick waited for something", len(got))
	}
}
