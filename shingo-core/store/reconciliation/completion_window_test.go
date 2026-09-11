package reconciliation

// Internal test: countRecent is the whole of the windowing rule and it is a
// pure function, so it is exercised here rather than behind a docker tag. The
// query that feeds it is covered in reconciliation_test.go.

import (
	"testing"
	"time"
)

func TestCountRecent_ExcludesAnomaliesOlderThanTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	at := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	// The Springfield shape that motivated the window: ten anomalies, every one
	// four months old, latching the verdict red while nothing was wrong.
	old := make([]*CompletionAnomaly, 0, 10)
	for i := range 10 {
		old = append(old, &CompletionAnomaly{OrderID: int64(i), ObservedAt: at(-120 * 24 * time.Hour)})
	}
	if got := countRecent(old, now, CompletionAnomalyWindow); got != 0 {
		t.Fatalf("ten four-month-old anomalies: got %d in window, want 0", got)
	}

	// And the case the window must NOT suppress: something wrong right now.
	fresh := []*CompletionAnomaly{
		{OrderID: 100, ObservedAt: at(-time.Hour)},
		{OrderID: 101, ObservedAt: at(-23 * time.Hour)},
	}
	if got := countRecent(append(old, fresh...), now, CompletionAnomalyWindow); got != 2 {
		t.Fatalf("two fresh among ten stale: got %d, want 2", got)
	}
}

func TestCountRecent_BoundaryAndMissingTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	// Exactly at the cutoff is OUT — the window is "after the cutoff", and a
	// row sitting precisely on a 24h boundary is the older reading.
	edge := []*CompletionAnomaly{{ObservedAt: at(-CompletionAnomalyWindow)}}
	if got := countRecent(edge, now, CompletionAnomalyWindow); got != 0 {
		t.Fatalf("anomaly exactly at the cutoff: got %d, want 0", got)
	}
	if got := countRecent([]*CompletionAnomaly{{ObservedAt: at(-CompletionAnomalyWindow + time.Second)}}, now, CompletionAnomalyWindow); got != 1 {
		t.Fatalf("anomaly one second inside the cutoff: got %d, want 1", got)
	}

	// A NULL timestamp counts as recent. Dropping it would render an unknown
	// age as health, which is the failure this panel exists to catch — so the
	// unsafe direction is the one that is asserted.
	if got := countRecent([]*CompletionAnomaly{{OrderID: 1}}, now, CompletionAnomalyWindow); got != 1 {
		t.Fatalf("anomaly with no timestamp: got %d, want 1 (unknown must not read as healthy)", got)
	}
}

// TestStationDwellCauseLiteralsMatchDispatch pins this package's copies of two
// queue-cause strings it must recognise but cannot import.
//
// dispatch imports store, so store importing dispatch is a cycle — the same
// reason the Edge duplicates waitKindStation. The literals are therefore the
// contract, and the contract is only kept if BOTH sides are pinned to it: the
// dispatch constants are asserted against these same strings in
// TestQueueCause_ValuesAreUnchanged.
//
// What a silent drift costs: stationDwellRow stops matching, every dwelling
// station wait falls back to the generic active_order_stuck row, and the board
// tells an operator to go and find out what a robot is doing when the answer is
// that somebody needs to press Release. That is the exact confusion the sharper
// row was added to end (run 12d, order 84), and nothing about it would fail.
//
// MUTATION (verified): change causeStationWaitLiteral to "station_wait". This
// fires naming both spellings.
func TestStationDwellCauseLiteralsMatchDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ got, want string }{
		{causeStationWaitLiteral, "station-wait"},
		{causeSwapPartnerFinishedLiteral, "swap-partner-finished"},
	} {
		if tc.got != tc.want {
			t.Errorf("cause literal = %q, want %q — this must equal the dispatch constant of the "+
				"same name, which is what actually gets written on the order row", tc.got, tc.want)
		}
	}
}

// TestMaterialWaitCauseLiteralsMatchDispatch pins this package's copies of the
// material-wait causes, for the same reason and in the same way as the station
// literals above: dispatch imports store, so store cannot import dispatch, and
// the strings ARE the contract.
//
// What a silent drift costs here is the opposite of a missing row. A renamed
// cause simply stops matching, the order falls to the 30-minute bound, and the
// anomaly board starts flagging every ordinary material wait in the plant — the
// exact noise the longer bound exists to prevent, arriving without a single test
// failing. The dispatch constants are asserted against these same strings in
// TestQueueCause_ValuesAreUnchanged.
func TestMaterialWaitCauseLiteralsMatchDispatch(t *testing.T) {
	t.Parallel()
	want := []string{
		"finder-node-empty",
		"finder-group-empty",
		"finder-pool-empty",
		"finder-plant-empty",
		"finder-no-full-carrier",
		"reserve-holding",
	}
	if len(materialWaitCauseLiterals) != len(want) {
		t.Fatalf("material-wait causes = %v, want %v.\nAdding one is a decision about which waits "+
			"may last a shift; state it where the list is declared.", materialWaitCauseLiterals, want)
	}
	for i := range want {
		if materialWaitCauseLiterals[i] != want[i] {
			t.Errorf("material-wait cause[%d] = %q, want %q — this must equal the dispatch constant "+
				"of the same name, which is what actually gets written on the order row",
				i, materialWaitCauseLiterals[i], want[i])
		}
	}
	// The resolver's two causes, which take the long bound only under the
	// material code — same contract, same reason to pin the strings.
	if got := materialWaitResolverCauseLiterals; len(got) != 2 || got[0] != "intake-resolve" || got[1] != "ngrp-resolve" {
		t.Errorf("resolver material-wait causes = %v, want [intake-resolve ngrp-resolve] — each must equal "+
			"its dispatch constant (CauseIntakeResolve, CauseNGRPResolve)", got)
	}
	// AND THE ALARM FAMILIES STAY OUT. These are the ones the record names by
	// hand: an outage must never read as a shortage, and a resting claim-failed
	// is the anomaly rather than a wait.
	for _, banned := range []string{
		"read-failed", "claim-failed", "held-bin-missing", "lock-race",
		"finder-accessibility-unreadable", "finder-source-unreadable",
	} {
		for _, got := range materialWaitCauseLiterals {
			if got == banned {
				t.Errorf("%q is in the material-wait set. It is not a shortage — it is Core "+
					"declining to answer or a hold that broke, and two hours of silence on it "+
					"is two hours nobody is told", banned)
			}
		}
	}
}
