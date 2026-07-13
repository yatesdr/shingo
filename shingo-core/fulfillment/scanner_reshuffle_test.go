package fulfillment

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// The scanner's OutcomeReshuffle arm. Reshuffle planning cannot live at intake
// alone: planTransport runs once, but burial arises over TIME — an order that
// queued with an accessible source can be buried by a later store while it waits,
// and this scanner is the only thing that looks at it again. Before this arm
// existed, such an order re-queued forever ("source bin buried; awaiting
// reshuffle") and nothing would ever unbury its lane.
//
// End-to-end proof of the stranding lives in
// engine/engine_buried_replay_test.go (real DB, real scanner). These pin the
// scanner's three dispositions cheaply.

func buriedFinder() *fakeFinder {
	return &fakeFinder{result: dispatch.SourceResult{
		Outcome: dispatch.OutcomeReshuffle,
		Buried: &dispatch.BuriedError{
			Bin:    &bins.Bin{ID: 7},
			Slot:   &nodes.Node{ID: 71, Name: "LANE-1-S2"},
			LaneID: 70,
		},
	}}
}

// A buried source on replay must be PLANNED, not re-queued forever.
func TestScanner_BuriedSource_PlansReshuffleOnReplay(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 1, "LINE-A")
	d := &recordingDispatcher{}

	s := newScannerWith(t, f, buriedFinder(), d, nil)
	s.RunOnce()

	if len(d.reshuffleCalls) != 1 || d.reshuffleCalls[0] != order.ID {
		t.Fatalf("reshuffle calls = %v, want exactly [%d] — a buried source on replay must plan a compound, "+
			"otherwise nothing in the system will ever unbury the lane", d.reshuffleCalls, order.ID)
	}
	// The compound's first child is dispatched by createCompound; the scanner must
	// not also dispatch the parent directly.
	if len(d.directCalls) != 0 {
		t.Errorf("scanner dispatched the parent directly (%v) — the compound owns the delivery", d.directCalls)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("scanner claimed a bin (%v) for a buried source — the compound's legs claim, not the parent", f.claimedBins)
	}
}

// Congestion is NOT a fault: stay queued and retry, never fail. Covers both
// ErrReshuffleWait causes -- the lane is mid-reshuffle for another order, and
// (the D79 rider) there is no free shuffle slot to park blockers in right now.
//
// The no-shuffle-slot half is the one that mattered: it used to fail the order
// TERMINALLY at intake (sim order 21, 2026-07-10 houseserver run: "cannot plan
// reshuffle: need 1 slot, 0 available"). A crowded lane is not a broken lane.
func TestScanner_BuriedSource_Congestion_WaitsNotFails(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 2, "LINE-B")

	var failed []int64
	failFn := func(orderID int64, _, _ string) { failed = append(failed, orderID) }

	// Both congestion causes arrive as ErrReshuffleWait; the detail says which.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"lane busy with another reshuffle", fmt.Errorf("%w: lane 70 is locked by another reshuffle", dispatch.ErrReshuffleWait)},
		{"no free shuffle slot", fmt.Errorf("%w: cannot plan reshuffle yet: no free shuffle slot: need 1 shuffle slots but only 0 available", dispatch.ErrReshuffleWait)},
	} {
		failed = nil
		f.queueReasons = nil
		d := &recordingDispatcher{reshuffleErr: tc.err}
		s := newScannerWith(t, f, buriedFinder(), d, failFn)
		s.RunOnce()

		if len(failed) != 0 {
			t.Fatalf("[%s] order %v was FAILED — this is congestion, not a broken lane; "+
				"the order must stay queued and retry (D18-Q4 wait-not-fail)", tc.name, failed)
		}
		if got := f.ordersByID[order.ID]; got.Status != protocol.StatusQueued {
			t.Errorf("[%s] status = %q, want %q (stay queued, retry next tick)", tc.name, got.Status, protocol.StatusQueued)
		}
		if len(f.queueReasons) != 1 || !strings.Contains(f.queueReasons[0].Reason, "buried") {
			t.Errorf("[%s] queue_reason = %v, want one naming the buried-source wait so it is diagnosable",
				tc.name, f.queueReasons)
		}
	}
}

// A STRUCTURAL reshuffle failure -- real lane geometry, not congestion -- must
// still fail the order through failFn, matching intake's disposition on a
// non-transient planning error. The wait-not-fail rule above must not swallow a
// genuinely unplannable lane into an infinite queue.
func TestScanner_BuriedSource_StructuralFailure_FailsOrder(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 3, "LINE-C")

	var failed []int64
	failFn := func(orderID int64, _, _ string) { failed = append(failed, orderID) }

	d := &recordingDispatcher{reshuffleErr: errors.New("target slot has no parent lane")}
	s := newScannerWith(t, f, buriedFinder(), d, failFn)
	s.RunOnce()

	if len(failed) != 1 || failed[0] != order.ID {
		t.Fatalf("failed = %v, want [%d] — a structurally unplannable lane must fail the order, "+
			"not queue it forever", failed, order.ID)
	}
}
