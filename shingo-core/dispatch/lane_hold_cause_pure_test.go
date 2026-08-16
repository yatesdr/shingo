package dispatch

import (
	"errors"
	"testing"

	"shingocore/store/reservations"
)

func digRow(orderID int64) reservations.MouthHold {
	return reservations.MouthHold{OrderID: orderID, Mode: reservations.ModeDig}
}

func inboundRow(orderID int64) reservations.MouthHold {
	return reservations.MouthHold{OrderID: orderID, Mode: reservations.ModeInbound}
}

// TestClassifyLaneHoldCause_UnreadableLaneIsNotTraffic is a STATED BEHAVIOUR
// CHANGE, not a characterisation — the old answer here was wrong and this test
// exists to say what it was.
//
// causeForLaneHolds used to `continue` past a mouth-row read it could not make
// and fall through to lane-held-traffic. So a lane genuinely held by a DIG,
// whose row happened to be unreadable at that instant, was filed as ordinary
// traffic contention.
//
// Nothing about that was unsafe. Admission had already refused the order; this
// only labels the refusal, and the operator sees the same "Waiting for a slot"
// sentence either way. It is the §17.5/§17.8 family in its third form: an alarm
// that fires under the wrong name. A dig stall and a traffic stall are
// investigated differently — one means a reshuffle is running, the other means
// two ordinary orders collided — so the wrong tag sends the next person reading
// the histogram to the wrong subsystem, and unlike a missing alarm it looks like
// data.
//
// The classifier is pure and takes the reads directly BECAUSE of this arm: there
// is no way to make one SELECT fail for one lane in a shared test database
// without breaking every other test using the table, and an error injected at
// the seam is a truthful stand-in for one raised below it. What is not covered
// here is the gathering loop, which is a call and an append.
//
// MUTATION (verified): restore the old body — `if r.err != nil { continue }`
// with no `unreadable` flag. This test fires with lane-held-traffic, which is
// exactly the label the plant would have recorded.
func TestClassifyLaneHoldCause_UnreadableLaneIsNotTraffic(t *testing.T) {
	t.Parallel()
	const self = 7
	boom := errors.New("mouth rows unreadable")

	got := classifyLaneHoldCause(self, []laneHoldRead{{err: boom}})
	if got != CauseLaneHeldUnreadable {
		t.Errorf("cause = %q, want %q — the lane was never read, so a dig cannot be ruled out, and "+
			"reporting routine traffic contention states a fact nobody established", got, CauseLaneHeldUnreadable)
	}

	// One lane readable and dig-free, another unreadable: still not traffic. The
	// order is contended on BOTH, so a clean read of one says nothing about why.
	got = classifyLaneHoldCause(self, []laneHoldRead{
		{rows: []reservations.MouthHold{inboundRow(99)}},
		{err: boom},
	})
	if got != CauseLaneHeldUnreadable {
		t.Errorf("cause = %q with one lane unread, want %q", got, CauseLaneHeldUnreadable)
	}
}

// TestClassifyLaneHoldCause_DefiniteAnswersAreUnchanged is the characterisation
// half: every arm that had an answer before still gives the same one.
//
// Without this the commit above could have been a rename of "traffic" to
// "unreadable" and the suite would not have noticed. The two definite verdicts
// are what the change must NOT touch.
//
// The last arm is the precedence rule and the one that could plausibly have gone
// the other way: a dig SEEN beats a lane not seen. Reporting unreadable there
// would discard an answer we actually have in favour of admitting ignorance,
// which is caution pointed the wrong way — the point of the new value is to stop
// claiming knowledge, not to stop reporting it.
//
// MUTATION (verified): make the unreadable check win by moving it above the row
// scan. The dig-plus-unreadable arm fires; the first two arms stay green, which
// is the split that shows precedence is what is under test.
func TestClassifyLaneHoldCause_DefiniteAnswersAreUnchanged(t *testing.T) {
	t.Parallel()
	const self = 7

	if got := classifyLaneHoldCause(self, []laneHoldRead{
		{rows: []reservations.MouthHold{digRow(42)}},
	}); got != CauseLaneHeldDig {
		t.Errorf("someone else's dig: cause = %q, want %q", got, CauseLaneHeldDig)
	}

	if got := classifyLaneHoldCause(self, []laneHoldRead{
		{rows: []reservations.MouthHold{inboundRow(42)}},
	}); got != CauseLaneHeldTraffic {
		t.Errorf("a different-mode holder: cause = %q, want %q", got, CauseLaneHeldTraffic)
	}

	// The order's OWN dig row is not a conflict with itself.
	if got := classifyLaneHoldCause(self, []laneHoldRead{
		{rows: []reservations.MouthHold{digRow(self)}},
	}); got != CauseLaneHeldTraffic {
		t.Errorf("own dig row: cause = %q, want %q — an order is never held by itself", got, CauseLaneHeldTraffic)
	}

	// Precedence: a dig we can SEE outranks a lane we could not read — in EITHER
	// order. Both arms are here because the first version of this test had only
	// the dig-first one, and a mutation that returned unreadable the moment it met
	// an error still passed it: the loop hit the dig and returned before reaching
	// the failure. A precedence rule that is only tested in the order that
	// short-circuits is not tested at all.
	for _, tc := range []struct {
		name  string
		reads []laneHoldRead
	}{
		{"dig read first", []laneHoldRead{
			{rows: []reservations.MouthHold{digRow(42)}},
			{err: errors.New("unreadable")},
		}},
		{"unreadable first", []laneHoldRead{
			{err: errors.New("unreadable")},
			{rows: []reservations.MouthHold{digRow(42)}},
		}},
	} {
		if got := classifyLaneHoldCause(self, tc.reads); got != CauseLaneHeldDig {
			t.Errorf("%s: cause = %q, want %q — reporting ignorance here would throw away an "+
				"answer we have, and the verdict must not depend on which lane was read first",
				tc.name, got, CauseLaneHeldDig)
		}
	}
}
