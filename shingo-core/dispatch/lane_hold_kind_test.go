package dispatch

import (
	"errors"
	"testing"

	"shingocore/store/reservations"
)

// TestClassifyLaneHoldCause_SplitsExcavationFromSourceLock pins the four-row
// precedence table classifyLaneHoldCause's header states, after §R.101 added the
// middle row.
//
// The old table had three rows and "a dig" in it meant mode='dig' — the same set
// of rows as "an excavation" until §R.101 gave every demand's source hold that
// mode. After it, lane-held-dig was written for ordinary retrieves and
// lane-held-traffic became nearly unreachable, because a source lock outranked it
// on any lane a demand had resolved onto.
//
// A pure test because the classifier is pure, which is why it was split out from
// the gathering loop in the first place: the unreadable case cannot be built in a
// shared test database (there is no way to make one lane's SELECT fail), and a
// classifier handed its reads directly can simply be given the failure.
//
// MUTATION (driven this session, fires): drop the IsExcavation check and return
// CauseLaneHeldDig for any foreign dig-mode row, as the arm read before. The
// source-lock and precedence sub-tests fail.
func TestClassifyLaneHoldCause_SplitsExcavationFromSourceLock(t *testing.T) {
	t.Parallel()

	const self = int64(7)
	excavation := func(owner int64) reservations.MouthHold {
		return reservations.MouthHold{OrderID: owner, Mode: reservations.ModeDig, ReservedBy: reservations.ByExcavation}
	}
	sourceLock := func(owner int64) reservations.MouthHold {
		return reservations.MouthHold{OrderID: owner, Mode: reservations.ModeDig, ReservedBy: reservations.BySourceLock}
	}
	traffic := func(owner int64) reservations.MouthHold {
		return reservations.MouthHold{OrderID: owner, Mode: reservations.ModeOutbound, ReservedBy: reservations.BySourceLock}
	}

	cases := []struct {
		name  string
		reads []laneHoldRead
		want  QueueCause
		why   string
	}{
		{
			name:  "a foreign excavation is a dig",
			reads: []laneHoldRead{{rows: []reservations.MouthHold{excavation(11)}}},
			want:  CauseLaneHeldDig,
			why:   "a reshuffle really is working this lane",
		},
		{
			name:  "a foreign source lock is NOT a dig",
			reads: []laneHoldRead{{rows: []reservations.MouthHold{sourceLock(11)}}},
			want:  CauseLaneHeldSource,
			why: "§R.101's source hold carries mode='dig' and is not an excavation; calling it one " +
				"sends an engineer looking for a reshuffle that was never planned",
		},
		{
			name:  "a different-mode holder is still traffic",
			reads: []laneHoldRead{{rows: []reservations.MouthHold{traffic(11)}}},
			want:  CauseLaneHeldTraffic,
			why:   "the pre-§R.101 arm, which the source-lock row must not swallow",
		},
		{
			name:  "the order's OWN rows are not a conflict",
			reads: []laneHoldRead{{rows: []reservations.MouthHold{excavation(self), sourceLock(self)}}},
			want:  CauseLaneHeldTraffic,
			why:   "the owner exemption is unchanged by the split",
		},
		{
			name: "an excavation on a LATER lane still wins over a source lock",
			reads: []laneHoldRead{
				{rows: []reservations.MouthHold{sourceLock(11)}},
				{rows: []reservations.MouthHold{excavation(12)}},
			},
			want: CauseLaneHeldDig,
			why: "a dig SEEN is the strongest fact available and must not depend on which lane the " +
				"gathering loop happened to read first — the ordering bug the classifier was " +
				"extracted to fix, in its new clothes",
		},
		{
			name: "an excavation still wins over an unreadable sibling",
			reads: []laneHoldRead{
				{err: errors.New("read failed")},
				{rows: []reservations.MouthHold{excavation(12)}},
			},
			want: CauseLaneHeldDig,
			why:  "reporting unreadable there would hide an answer we actually have",
		},
		{
			name: "unreadable outranks a source lock",
			reads: []laneHoldRead{
				{rows: []reservations.MouthHold{sourceLock(11)}},
				{err: errors.New("read failed")},
			},
			want: CauseLaneHeldUnreadable,
			why: "a lane that could not be read may hold an excavation, and 'we cannot tell' must " +
				"not be reported as the weaker definite answer",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLaneHoldCause(self, tc.reads); got != tc.want {
				t.Errorf("classifyLaneHoldCause = %q, want %q — %s", got, tc.want, tc.why)
			}
		})
	}
}
