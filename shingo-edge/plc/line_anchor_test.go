package plc

// line_anchor_test.go — the parts in the line are anchored at a production
// counter reset that is a program change: one that came with a confirmed
// CATID change, read fresh. Any other reset unanchors.

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
)

// identityClock is the monitor's answer to "when did the CATID last change",
// set by the test.
type identityClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *identityClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

func (c *identityClock) changedAt(int64) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func anchoredRig(t *testing.T) (*lineRig, *identityClock) {
	t.Helper()
	r := newLineRig(t, true)
	ic := &identityClock{}
	r.mgr.SetIdentityChangeSource(ic.changedAt)
	r.pass(300, 500) // both seed
	r.pass(310, 520) // both count
	return r, ic
}

// anchor reads the station counter's anchor (nil: unanchored).
func (r *lineRig) anchor() (*int64, *time.Time) {
	r.t.Helper()
	c, err := r.db.GetLineCounter(r.lc)
	testutil.MustNoErr(r.t, err, "station counter")
	return c.AnchorCount, c.AnchorAt
}

// TestInLine_AnchorsAtAResetWithACATIDChange: the counter resets at a program
// change and the monitor confirms the new CATID after its debounce, a pass
// later: the station counter is anchored at the count the reset's pass listed
// (the previous pass's read, 520), not at that pass's own read (523), and
// dated by the reset. A confirm that came first, inside the window, anchors in
// the reset's own pass.
func TestInLine_AnchorsAtAResetWithACATIDChange(t *testing.T) {
	t.Parallel()
	t.Run("the confirm after the reset", func(t *testing.T) {
		t.Parallel()
		r, ic := anchoredRig(t)
		resetAt := time.Now()
		r.pass(0, 523)
		if a, _ := r.anchor(); a != nil {
			t.Fatalf("anchored at %d before the monitor confirmed a CATID change", *a)
		}
		ic.set(resetAt.Add(2 * time.Second))
		r.pass(1, 525)
		a, at := r.anchor()
		if a == nil || *a != 520 {
			t.Fatalf("anchor = %v after the confirm, want 520 (the count the reset's pass listed)", a)
		}
		if at == nil || at.Sub(resetAt).Abs() > 2*time.Second {
			t.Errorf("anchored at %v, want the reset (%v)", at, resetAt)
		}
	})
	t.Run("the confirm before the reset", func(t *testing.T) {
		t.Parallel()
		r, ic := anchoredRig(t)
		ic.set(time.Now().Add(-3 * time.Second))
		r.pass(0, 523)
		if a, _ := r.anchor(); a == nil || *a != 520 {
			t.Fatalf("anchor = %v, want 520 in the reset's own pass", a)
		}
	})
}

// TestInLine_ResetWithoutACATIDChangeClearsTheAnchor: a counter reset with no
// CATID change near it (a hand reset, a PLC restore: the line may hold parts)
// unanchors, and a confirm far from it never anchors.
func TestInLine_ResetWithoutACATIDChangeClearsTheAnchor(t *testing.T) {
	t.Parallel()
	r, ic := anchoredRig(t)
	testutil.MustNoErr(t, r.db.AnchorLineCounter(r.lc, rigPLC, rigStationTag, 400, time.Now().Add(-time.Hour)), "an anchor")
	ic.set(time.Now().Add(-time.Minute)) // the last program change, long before
	r.pass(0, 523)
	r.pass(1, 525)
	if a, _ := r.anchor(); a != nil {
		t.Fatalf("anchor = %d after a reset with no CATID change, want none", *a)
	}
}

// TestInLine_ResetSeenLateLeavesItUnanchored: a reset first read past 1 (the
// Edge was not polling across the change, so the station's last read is not
// the line's start) unanchors, even with the CATID change confirmed beside it.
func TestInLine_ResetSeenLateLeavesItUnanchored(t *testing.T) {
	t.Parallel()
	r, ic := anchoredRig(t)
	testutil.MustNoErr(t, r.db.AnchorLineCounter(r.lc, rigPLC, rigStationTag, 400, time.Now().Add(-time.Hour)), "an anchor")
	ic.set(time.Now())
	r.pass(5, 540)
	r.pass(6, 541)
	if a, _ := r.anchor(); a != nil {
		t.Fatalf("anchor = %d after a reset read at 5, want none", *a)
	}
}
