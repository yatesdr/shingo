package dispatch

import "testing"

// finder_outcome_test.go — C(ii): the shared admission point for finder
// outcomes. The default arm of a consumer switch is where a newly added
// Outcome dies silently; MapFinderOutcome exists so that failure mode is a
// loud structural fail instead. These are pure — no store, no docker.

func TestMapFinderOutcome_MembersPassThrough(t *testing.T) {
	t.Parallel()
	for _, o := range []Outcome{OutcomeFound, OutcomeWait, OutcomeReshuffle, OutcomeStructural} {
		if got := MapFinderOutcome(SourceResult{Outcome: o}); got != o {
			t.Errorf("MapFinderOutcome(%v) = %v, want identity for a member", o, got)
		}
	}
}

func TestMapFinderOutcome_UnknownDegradesStructural(t *testing.T) {
	t.Parallel()
	got := MapFinderOutcome(SourceResult{Outcome: Outcome(99)})
	if got != OutcomeStructural {
		t.Fatalf("MapFinderOutcome(unknown) = %v, want OutcomeStructural — an unrecognized outcome must fail LOUD, not park or dispatch", got)
	}
}
