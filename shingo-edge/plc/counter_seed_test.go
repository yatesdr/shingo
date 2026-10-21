package plc

import (
	"testing"
)

// A reporting point that has never been read SEEDS from its first read. The
// PLC's counter is a running total the reporting point has never seen, so the
// first read is the point it starts counting from, not parts made since the
// last read: Springfield Press 6, 2026-10-21, charged its whole total (8938)
// onto two bound bins at first wiring. Read off the real poll pass.

// TestCounterSeed_FirstReadChargesNothing: the first read of a new reporting
// point emits no delta, ships no tick, and stores the count and the poll time.
func TestCounterSeed_FirstReadChargesNothing(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newUnreadTickRig(t, em, 0)

	r.pass(8938)

	if got := em.emitted(); len(got) != 0 {
		t.Errorf("emitted deltas = %+v, want none: a first read is where counting starts", got)
	}
	if ticks := r.shippedTicks(); len(ticks) != 0 {
		t.Errorf("shipped = %+v, want no tick for a first read", ticks)
	}
	rp := r.rp()
	if rp.LastCount != 8938 {
		t.Errorf("last_count = %d, want 8938, the seed", rp.LastCount)
	}
	if rp.LastPollAt == nil {
		t.Error("last_poll_at is NULL after the first read, want the poll time: the next read would seed again")
	}
}

// TestCounterSeed_SecondReadEmitsTheDifference: after the seed, the counter
// counts from it.
func TestCounterSeed_SecondReadEmitsTheDifference(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newUnreadTickRig(t, em, 0)

	r.pass(8938)
	r.pass(8941)

	if got := em.emitted(); len(got) != 1 || got[0] != (emittedDelta{3, ""}) {
		t.Errorf("emitted deltas = %+v, want one {3 \"\"}", got)
	}
	if ticks := r.shippedTicks(); len(ticks) != 1 || ticks[0].Delta != 3 || ticks[0].CountValue != 8941 {
		t.Errorf("shipped = %+v, want one tick of 3 at 8941", ticks)
	}
}

// TestCounterSeed_FirstReadOfZeroIsARead: a counter that reads 0 the first
// time has been read. Its next read is parts, not a second seed.
func TestCounterSeed_FirstReadOfZeroIsARead(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newUnreadTickRig(t, em, 0)

	r.pass(0)
	if rp := r.rp(); rp.LastPollAt == nil {
		t.Fatal("last_poll_at is NULL after a first read of 0, want the poll time")
	}
	r.pass(5)

	if got := em.emitted(); len(got) != 1 || got[0] != (emittedDelta{5, ""}) {
		t.Errorf("emitted deltas = %+v, want one {5 \"\"}: the second read counts", got)
	}
}

// TestCounterSeed_FailedFirstReadLeavesItUnread: a first poll that cannot read
// the tag is not a read. The first good read seeds.
func TestCounterSeed_FailedFirstReadLeavesItUnread(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newUnreadTickRig(t, em, 0)

	r.mgr.pollAllReportingPoints() // no value in the cache: the read fails
	if rp := r.rp(); rp.LastPollAt != nil {
		t.Fatalf("last_poll_at = %v after a failed read, want NULL", rp.LastPollAt)
	}
	r.pass(120)

	if got := em.emitted(); len(got) != 0 {
		t.Errorf("emitted deltas = %+v, want none: the first good read seeds", got)
	}
	if rp := r.rp(); rp.LastCount != 120 || rp.LastPollAt == nil {
		t.Errorf("last_count/last_poll_at = %d/%v, want 120 and a poll time", rp.LastCount, rp.LastPollAt)
	}
}
