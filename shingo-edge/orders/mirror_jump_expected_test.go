package orders

import (
	"testing"

	"shingo/protocol"
)

// TestExpectedMirrorJumps_OnlySilenceWhatCoreNeverAnnounced pins item 4's
// verdicts and, more importantly, pins that the ticket queue still works.
//
// The MIRROR JUMP row is a ticket queue: non-zero is supposed to mean "a
// transition is still not notifying, go find it". A soak produced 31 of them and
// every one crossed a transition that is silent BY CONSTRUCTION — so the queue
// was permanently non-zero for expected reasons, which is how a net stops being
// read at all (standing law 9).
//
// Classifying is therefore only defensible if the UNCLASSIFIED case still
// tickets. That is the assertion this test exists for; the two verdicts are the
// easy half.
func TestExpectedMirrorJumps_OnlySilenceWhatCoreNeverAnnounced(t *testing.T) {
	// THE VERDICTS. Both cross a pure transition — one with no actionMap entry,
	// and dispatch/lifecycle.go's transition() applies only actionMap actions, so
	// there is no general per-status push that could have told the Edge.
	for _, tc := range []struct{ from, to protocol.Status }{
		{protocol.StatusPending, protocol.StatusAcknowledged}, // via pending->queued
		{protocol.StatusSourcing, protocol.StatusInTransit},   // via sourcing->dispatched
	} {
		why, ok := expectedMirrorJumps[[2]protocol.Status{tc.from, tc.to}]
		if !ok {
			t.Errorf("%s->%s is not classified — it will ticket every soak and the queue stays unreadable",
				tc.from, tc.to)
			continue
		}
		if why == "" {
			t.Errorf("%s->%s is classified with no reason — an entry here is a CLAIM that Core intends "+
				"the silence, and it has to be argued where the next reader looks", tc.from, tc.to)
		}
		// It must actually be a jump. Classifying an ADJACENT pair would silence a
		// transition that never needed silencing and hide a future real gap.
		if !protocol.IsForwardJump(tc.from, tc.to) {
			t.Errorf("%s->%s is classified but is not a forward jump — the list may only excuse jumps",
				tc.from, tc.to)
		}
	}

	// THE ASSERTION THAT MAKES THE CLASSIFICATION HONEST. A forward jump nobody
	// has argued for must still reach the MIRROR JUMP path.
	unclassified := [2]protocol.Status{protocol.StatusQueued, protocol.StatusDelivered}
	if !protocol.IsForwardJump(unclassified[0], unclassified[1]) {
		t.Fatalf("fixture: %s->%s is not a forward jump, pick another", unclassified[0], unclassified[1])
	}
	if _, ok := expectedMirrorJumps[unclassified]; ok {
		t.Errorf("%s->%s is classified expected — nothing has argued for it, and a queue that "+
			"excuses everything reports nothing", unclassified[0], unclassified[1])
	}

	// And the list stays small enough to read. Each entry is a standing claim
	// about Core's intent; a list that grows silently is the same failure as a
	// queue that never empties.
	if len(expectedMirrorJumps) > 4 {
		t.Errorf("expectedMirrorJumps has %d entries — every one is a claim that Core intends the "+
			"silence; if the list is growing, the transitions probably want notifying instead",
			len(expectedMirrorJumps))
	}
}
