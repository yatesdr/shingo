package dispatch

import (
	"fmt"
	"testing"
)

// dig_standoff_tripwire_test.go — the cycle walk, tested as arithmetic.
//
// The graph derivation needs a database and is exercised by the docker suite; the
// WALK is a pure function over a map and this is where its edge cases live. They
// are the ones that make a tripwire lie: a standoff reported three times because
// three digs are in it, a standoff missed because the walk entered the loop from
// outside it, or a self-loop reported as a mutual hold when it is one dig blocked
// by its own lock.

func TestClosedWalks_FindsATwoCycle(t *testing.T) {
	got := closedWalks(map[int64]int64{18: 35, 35: 18})
	if len(got) != 1 {
		t.Fatalf("found %d cycles, want exactly 1: %v", len(got), got)
	}
	if fmt.Sprint(got[0]) != "[18 35]" {
		t.Errorf("cycle is %v, want [18 35] — canonicalised to start at the smallest id so the "+
			"same standoff found from either end produces one record", got[0])
	}
}

// TestClosedWalks_ReportsAThreeCycleOnce is the rig's own specimen: digs 18, 23
// and 35 holding each other. Reported once, not once per participant.
func TestClosedWalks_ReportsAThreeCycleOnce(t *testing.T) {
	got := closedWalks(map[int64]int64{18: 35, 35: 23, 23: 18})
	if len(got) != 1 {
		t.Fatalf("found %d cycles, want exactly 1 — a three-way standoff is ONE finding, and "+
			"reporting it once per member is how an alarm teaches people to ignore it: %v",
			len(got), got)
	}
	if fmt.Sprint(got[0]) != "[18 35 23]" {
		t.Errorf("cycle is %v, want [18 35 23] (rotated to the smallest id, order preserved)", got[0])
	}
}

// TestClosedWalks_IgnoresAChainThatDoesNotClose is the quiet-when-zero half. A
// waits on B waits on C, and C is not waiting on anybody — that is ordinary
// congestion draining in order, and it must produce no alarm at all.
func TestClosedWalks_IgnoresAChainThatDoesNotClose(t *testing.T) {
	if got := closedWalks(map[int64]int64{1: 2, 2: 3}); len(got) != 0 {
		t.Errorf("found %v in an open chain. A chain drains — C is not blocked, so it finishes, "+
			"and B and A follow. Alarming here is crying wolf on the normal case", got)
	}
}

// TestClosedWalks_FindsACycleEnteredFromOutside is the miss that matters most.
// Dig 1 waits on a two-cycle it is not part of. Walking from 1 must still find
// {2,3}, and must not report 1 as a member — 1 is a victim, not a participant,
// and telling an operator to look at it sends them to the wrong robot.
func TestClosedWalks_FindsACycleEnteredFromOutside(t *testing.T) {
	got := closedWalks(map[int64]int64{1: 2, 2: 3, 3: 2})
	if len(got) != 1 {
		t.Fatalf("found %d cycles, want 1: %v", len(got), got)
	}
	if fmt.Sprint(got[0]) != "[2 3]" {
		t.Errorf("cycle is %v, want [2 3]. Dig 1 is stuck BEHIND the standoff, not in it", got[0])
	}
}

// TestClosedWalks_ASelfLoopIsACycleOfOne. A dig blocked by its own lock is not a
// mutual hold, but it is just as stuck, and it is a sharper defect: right of way
// exempts a dig from its own lane, so this should be impossible.
func TestClosedWalks_ASelfLoopIsACycleOfOne(t *testing.T) {
	got := closedWalks(map[int64]int64{7: 7})
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != 7 {
		t.Fatalf("got %v, want exactly one cycle [7] — a dig waiting on itself is stuck and must "+
			"not be swallowed by a walk that only looks for pairs", got)
	}
}

// TestClosedWalks_FindsTwoIndependentStandoffs — two separate groups can be in
// trouble at once, and collapsing them into one record loses a whole incident.
func TestClosedWalks_FindsTwoIndependentStandoffs(t *testing.T) {
	got := closedWalks(map[int64]int64{1: 2, 2: 1, 10: 11, 11: 10})
	if len(got) != 2 {
		t.Fatalf("found %d cycles, want 2: %v", len(got), got)
	}
	if fmt.Sprint(got[0]) != "[1 2]" || fmt.Sprint(got[1]) != "[10 11]" {
		t.Errorf("got %v, want [[1 2] [10 11]] — sorted so the record order is stable", got)
	}
}

func TestClosedWalks_EmptyGraphIsQuiet(t *testing.T) {
	if got := closedWalks(nil); len(got) != 0 {
		t.Errorf("got %v from an empty graph, want nothing (law 9: quiet when zero)", got)
	}
}
