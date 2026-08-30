package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verdict_names_cause_test.go — THE ARM THAT DECIDES IS THE ARM THAT NAMES.
//
// Three refusals in this package used to be classified twice: once by the code
// that made the decision, and again — in its own words, from its own second read
// — by whichever caller had to park an order under it. Each pair agreed on the
// day it was written and each was a place for the next refusal shape to be
// absorbed into an old cause.
//
//	dig blocker    FOUR arms wrote `promised ? promised : claimed`: the lane-gate
//	               acceptance path, the complex reshuffle path,
//	               parkOnClaimedBlocker, and digBlockerCause off the typed error.
//	               Now: digBlockerCause decides, laneClearResult.blockerCause
//	               carries it, the three readers read the field.
//	lane mouth     the refusal rolled back and returned a bare false; both callers
//	               then re-queried every lane's mouth rows to label it. Lane
//	               mouths are exactly the state that moves between two reads —
//	               what refuses this order is another order finishing with the
//	               lane. Now: laneAdmission carries the cause.
//	storage slot   both fulfillment scanner sites derived `unresolved-group if
//	               IsSyntheticUnresolved, otherwise slot-contended`, so a hard
//	               database error parked as "the destination slot is contended".
//	               Now: StorageDropoff carries the cause.
//
// A SOURCE TEST, because the claim is about the code's shape. No fixture can
// prove that a second classifier does not exist somewhere else in the package —
// and the failure it guards against is not a wrong answer, it is two answers
// that agree until they do not.

// TestDigBlockerCauseIsDecidedOnceScan pins the four→one collapse. The two
// constants may be MENTIONED anywhere (a releaser row, a doc, a test), but the
// fork that chooses between them belongs to digBlockerCause alone.
func TestDigBlockerCauseIsDecidedOnceScan(t *testing.T) {
	t.Parallel()

	// The decider, plus the files allowed to name a specific member for a reason
	// other than forking on it.
	allowed := map[string]string{
		"dispatcher.go": "digBlockerCause itself — the one place that reads a claimed refusal " +
			"apart from a promised one",
		"lane_clear_dig.go": "the PRODUCER: it calls digBlockerCause once and stores the answer on " +
			"laneClearResult.blockerCause, plus the pre-check arm, which catches only hard claims " +
			"by construction (binIsUnclaimed reads a claim; a promise is not one)",
		"queue_cause.go":     "the constants' own declarations and their doc comments",
		"queue_releasers.go": "the releaser rows, which describe what ends each wait",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the dispatch package: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, known := allowed[name]; known {
			continue
		}
		src, rErr := os.ReadFile(filepath.Clean(name))
		if rErr != nil {
			t.Fatalf("read %s: %v", name, rErr)
		}
		body := string(src)
		if strings.Contains(body, "CauseDigBlockerPromised") && strings.Contains(body, "CauseDigBlockerClaimed") {
			t.Errorf("%s names BOTH dig-blocker causes, which is what forking on them looks like, "+
				"and it is not the decider.\n"+
				"A claimed blocker is waiting out a robot's drive; a promised one (the ranked take) "+
				"is waiting on a holder that has no robot at all. Four arms used to choose between "+
				"them in four sets of words, off one fact. Read laneClearResult.blockerCause, which "+
				"the producer set from digBlockerCause — or, if you hold the typed error and no "+
				"result, call digBlockerCause. Then say here why this file is different.", name)
		}
	}
}

// TestRefusalClassifiersAreNotReimplementedScan pins the other two collapses by
// their inputs: a caller that has a verdict must not go back to the store to
// work out what the verdict already said.
func TestRefusalClassifiersAreNotReimplementedScan(t *testing.T) {
	t.Parallel()

	// symbol -> the file that owns the classification, and why.
	owners := map[string]struct{ file, why string }{
		"causeForLaneHolds(": {"lane_gate.go",
			"acquireOrderLanes calls it at the refusal, in the same breath as the rollback, and " +
				"returns the answer on laneAdmission. A caller that calls it again is taking a " +
				"SECOND read of lane mouth state that has already moved on"},
		"causeForStorageDropoff(": {"store_slot.go",
			"ReserveStorageDropoff calls it and returns the answer on StorageDropoff.Cause. " +
				"Re-deriving it is what parked a failed database read as slot contention"},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the dispatch package: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rErr := os.ReadFile(filepath.Clean(name))
		if rErr != nil {
			t.Fatalf("read %s: %v", name, rErr)
		}
		body := string(src)
		for sym, owner := range owners {
			if name == owner.file || !strings.Contains(body, sym) {
				continue
			}
			t.Errorf("%s calls %s, which belongs to %s.\n%s", name, sym, owner.file, owner.why)
		}
	}
}
