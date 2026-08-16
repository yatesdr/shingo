package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// TestClassifyPlanError is SITUATION 3's pin and the totality check for the other
// three (PLAN §R.45).
//
// ── WHY THIS TEST EXISTS IN THIS FORM ─────────────────────────────────────
//
// The four situations the owner ruled on are four planner error paths. Three of
// them — the blockers read, the bins-in-a-slot read, and the shuffle-pool read —
// go through the same store against the same tables, so from OUTSIDE the planner
// they are not separable: the mechanism that breaks one (renaming a column the
// read needs) breaks whichever read happens first, and the blockers read is
// always first. Situation 3 therefore has no honest end-to-end fixture.
//
// So the decision was extracted to where it is made and is pinned exhaustively
// here, with the end-to-end dispositions for situations 1, 2 and 4 pinned in
// dig_unplannable_disposition_docker_test.go. Between them every arm is covered
// twice: once for "the classifier says the right thing" and once for "the caller
// does the right thing with it".
//
// The ORDER of the arms is the thing most likely to rot, and it is the one that
// fails silently: readFailed() is true for any non-nil error that is not
// sql.ErrNoRows, so moving it above the sentinels makes a configuration fault
// park forever under a cause nothing can clear. The sentinel cases below fail
// immediately if that happens.
func TestClassifyPlanError(t *testing.T) {
	// A transport-shaped error, the way the store actually returns one: wrapped,
	// several layers deep, never sql.ErrNoRows.
	transport := fmt.Errorf("find shuffle slots: %w",
		fmt.Errorf("list child nodes of group 4: %w", errors.New("read tcp 127.0.0.1:5432: connection reset by peer")))

	cases := []struct {
		name string
		err  error
		want serviceDigOutcome
	}{
		{"no error", nil, serviceDigStarted},

		// The two that already waited, unchanged by the ruling.
		{"no free shuffle slot", fmt.Errorf("%w: need 2 but 0 available", ErrNoShuffleSlot), serviceDigNoShuffleSlot},
		{"nothing in the way", fmt.Errorf("%w: slot S3", ErrNothingInTheWay), serviceDigNothingInTheWay},

		// SITUATION 4 — the configuration fault. Asked BEFORE readFailed, or it
		// would be swallowed as a stutter.
		{"slot in no lane", fmt.Errorf("%w: GRP-L1-S2", ErrSlotNotInLane), serviceDigSlotNotInLane},

		// SITUATIONS 1-3 — every planner read failure, whatever its depth.
		{"situation 3: shuffle-pool read failed", transport, serviceDigReadFailed},
		{"situation 1: blockers read failed",
			fmt.Errorf("blockers in front of slot 7: %w", errors.New("driver: bad connection")), serviceDigReadFailed},
		{"situation 2: bins-at-slot read failed",
			fmt.Errorf("list bins at blocker slot S1: %w", errors.New("driver: bad connection")), serviceDigReadFailed},

		// ABSENCE IS NOT A FAILED READ, which is the whole distinction the
		// disposition rests on. sql.ErrNoRows means "there is nothing there" — a
		// fact, not a hiccup — so it must NOT be treated as a stutter to wait out.
		{"genuine absence is not a stutter", fmt.Errorf("lookup: %w", sql.ErrNoRows), serviceDigUnplannable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPlanError(tc.err); got != tc.want {
				t.Errorf("classifyPlanError(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyPlanError_SentinelsBeatReadFailed states the ordering hazard as its
// own assertion rather than leaving it implicit in the table above.
//
// Every sentinel this planner returns is a non-nil error that is not
// sql.ErrNoRows, so readFailed() is true for ALL of them. The classifier is
// correct only because it asks the named questions first, and nothing about the
// code makes that obvious to the next reader — a `switch` reads as a set of
// alternatives, not as a priority list.
//
// MUTATION (verified): move the readFailed arm above the sentinels and every case
// here fails at once.
func TestClassifyPlanError_SentinelsBeatReadFailed(t *testing.T) {
	for _, e := range []error{
		fmt.Errorf("%w: x", ErrNoShuffleSlot),
		fmt.Errorf("%w: x", ErrNothingInTheWay),
		fmt.Errorf("%w: x", ErrSlotNotInLane),
	} {
		if !readFailed(e) {
			t.Fatalf("premise changed: readFailed(%v) is now false, so the ordering hazard this test "+
				"guards no longer exists — re-read classifyPlanError before deleting anything", e)
		}
		if got := classifyPlanError(e); got == serviceDigReadFailed {
			t.Errorf("%v classified as a read failure — the sentinels must be asked before "+
				"readFailed(), or a configuration fault parks forever under a cause nothing clears", e)
		}
	}
}
