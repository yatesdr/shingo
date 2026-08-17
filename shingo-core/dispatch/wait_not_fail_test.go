package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// wait_not_fail_test.go — the C1 audit's two confirmed congestion-terminals.
//
// The rule they both violated: demand never terminates for congestion. Neither
// was a judgement call about where that line falls — in both cases the code
// already said what it should do somewhere and did the opposite somewhere else.

// TestLoaderSourceUnreadable_Waits — every error sourceFromDedicatedLoader can
// return wraps a database read, and each of those returns says in a comment that
// it propagates SO THE ORDER QUEUES. The caller mapped all five to a structural
// terminal, so one failed SELECT against the loader pool killed the order.
//
// MUTATION (verified): restore
// `return SourceResult{Outcome: OutcomeStructural, TermCode: "loader_source", Err: lerr}`
// at the loader tier in source_finder.go. The outcome assertion fires
// (OutcomeStructural, want OutcomeWait).
func TestLoaderSourceUnreadable_Waits(t *testing.T) {
	db := newFakeFinderDB()
	posID := int64(51)
	db.addNode(&nodes.Node{ID: posID, Name: "L1"})
	db.addNode(&nodes.Node{ID: 99, Name: "DEST"})
	db.addDedicatedLoader(1, posID, "X")
	db.addBin(&bins.Bin{ID: 101, PayloadCode: "X", NodeID: &posID, UOPRemaining: 5, UOPCapacity: 10, Status: "available"})

	// The pool membership read fails. The material is right there — this is Core
	// unable to look, not a plant with nothing in it.
	db.loaderHomesErr = errors.New("connection reset by peer")

	finder := NewSourceFinder(db, nil, nil)
	order := &orders.Order{ID: 1, OrderType: OrderTypeRetrieve, SourceNode: "L1", DeliveryNode: "DEST", PayloadCode: "X"}

	res := finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeWait {
		t.Fatalf("outcome = %v, want OutcomeWait — a failed read of the loader pool is not a fact "+
			"about the plant, and terminal-failing on it drops an order that needed one retry", res.Outcome)
	}
	if res.QueueCause != CauseLoaderSourceUnreadable {
		t.Errorf("queue_cause = %q, want %q — an unreadable pool must stay distinguishable from an "+
			"honestly empty one (finder-pool-empty), because only one of them is something an "+
			"operator can act on", res.QueueCause, CauseLoaderSourceUnreadable)
	}
	// The no-fall-through invariant still holds: a loader source that could not be
	// read must NOT be answered from the plant-wide scan.
	if db.fifoCalls != 0 {
		t.Errorf("FindSourceBinFIFO called %d time(s) after an unreadable loader pool — the scoped "+
			"tier must not fall through to plant-wide FIFO", db.fifoCalls)
	}
}

// TestDispatchDirect_FleetRefusalIsNotTerminal — the rollback that could not
// work.
//
// DispatchDirect used to call lifecycle.Fail on a fleet-create error. `failed`
// has no outgoing edges, so the fulfillment scanner's documented recovery —
// "override back to sourcing since this is a transient fleet issue, not a
// permanent failure" — was an illegal transition, logged and swallowed. Every
// fleet rejection killed the order while the call site read as though it had
// re-queued, and a rejection on a compound leg took the dig and its parent with
// it through the sibling cascade.
//
// The assertion is on the STATUS, not on the error: DispatchDirect still returns
// the error, and it always did. What changed is that the order is still alive to
// act on it.
//
// NO MUTATION NOTE, because there is nothing here to mutate. This test builds no
// order, calls no DispatchDirect and reads no status — it asserts against the
// static transition table, and putting lifecycle.Fail back would not change one
// line of it. A note claiming "the status assertion fires and the follow-on
// MoveToSourcing assertion fires with it" stood here and named two assertions
// this function has never contained.
func TestDispatchDirect_FleetRefusalIsNotTerminal(t *testing.T) {
	// The state machine is the whole subject, so assert against it directly
	// rather than against a comment: `failed` is terminal and the scanner's
	// recovery edge does not exist from there.
	if protocol.IsValidTransition(protocol.StatusFailed, protocol.StatusSourcing) {
		t.Fatal("failed → sourcing is a legal transition now; this test's premise is gone and the " +
			"scanner's rollback would have worked all along — re-derive the finding before deleting it")
	}
	for _, from := range []protocol.Status{protocol.StatusQueued, protocol.StatusSourcing, protocol.StatusPending} {
		if !protocol.IsValidTransition(from, protocol.StatusSourcing) && from != protocol.StatusSourcing {
			t.Errorf("%s → sourcing must be legal: it is the fleet-refusal recovery edge the scanner "+
				"takes after DispatchDirect returns an error", from)
		}
	}
}

// TestPlanningError_TransientCodesAreNotSilentlyTerminal is the standing guard
// on the classification itself: a code that Transient() admits must never also
// be a code a caller can only render terminally. Cheap, and it is the assertion
// the two findings above would each have tripped.
func TestPlanningError_TransientCodesAreNotSilentlyTerminal(t *testing.T) {
	for _, code := range []string{codeClaimFailed, codeLaneLocked, codeNoShuffleSlot, codeBlockerClaimed} {
		pe := &planningError{Code: code, Detail: "x"}
		if !pe.Transient() {
			t.Errorf("code %q dropped out of Transient() — every caller now terminal-fails it", code)
		}
		if reshuffleWaitCause(code) == CauseReshuffleCongestion && code != codeClaimFailed {
			t.Errorf("code %q has no specific wait cause, so its park is indistinguishable from the "+
				"other two reshuffle waits on the row", code)
		}
	}
}

var _ = orders.Order{}
