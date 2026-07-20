package fulfillment

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// Sentinel errors injected into the fakes to drive each requeue path.
var (
	errFleet   = errors.New("fleet create failed")
	errClaim   = errors.New("claim race")
	errReserve = errors.New("slot reserve conflict")
)

// TestScanner_RequeuePaths_SetQueueCode covers the four requeue paths that used
// to call lifecycle.Queue() with NO queue-reason write, leaving a stale reason
// on the row. Each must now park the order under a structured queue code so the
// operator/analytics see the current cause, not whatever was there before.
//
// The paths:
//   - fleet dispatch failure (plain + held-bin)  → fleet_unavailable
//   - claim contention requeue                    → waiting_for_material
//   - destination slot reserve conflict           → waiting_for_slot
func TestScanner_RequeuePaths_SetQueueCode(t *testing.T) {
	t.Parallel()

	// A dispatcher fake whose DispatchDirect always fails — drives the fleet-
	// unavailable requeue on BOTH the plain path and the held-bin path.
	failDispatch := &recordingDispatcher{directErr: errFleet}

	// --- Plain path: order with a found source, fleet dispatch fails ---
	t.Run("plain fleet fail sets fleet_unavailable", func(t *testing.T) {
		f := newFakeStore()
		order := seedQueuedRetrieve(f, 10, "LINE-A")
		order.PayloadCode = "PN-FLEET"
		// Source node + dest node resolve; finder returns a found bin.
		f.nodesByDot["LINE-A"] = &nodes.Node{ID: 100, Name: "LINE-A"}
		finder := foundFinderWith(20, "SRC-A")
		s := newScannerWith(t, f, finder, failDispatch, nil)
		s.RunOnce()

		want := queueReasonUpdate{OrderID: 10, Reason: "Robot system not responding — retrying",
			Code: string(protocol.QueueFleetUnavailable), Cause: "fleet-error"}
		assertQueueReason(t, f, want)
	})

	// --- Held-bin path: order already holding a bin, fleet dispatch fails ---
	t.Run("held-bin fleet fail sets fleet_unavailable", func(t *testing.T) {
		f := newFakeStore()
		order := seedQueuedRetrieve(f, 11, "LINE-B")
		binID := int64(30)
		order.BinID = &binID
		order.SourceNode = "SRC-B"
		f.nodesByDot["LINE-B"] = &nodes.Node{ID: 101, Name: "LINE-B"}
		f.nodesByDot["SRC-B"] = &nodes.Node{ID: 102, Name: "SRC-B"}
		// held-bin path does not consult the finder; pass a found stand-in so the
		// constructor is happy.
		s := newScannerWith(t, f, foundFinderWith(30, "SRC-B"), failDispatch, nil)
		s.RunOnce()

		want := queueReasonUpdate{OrderID: 11, Reason: "Robot system not responding — retrying",
			Code: string(protocol.QueueFleetUnavailable), Cause: "fleet-error"}
		assertQueueReason(t, f, want)
	})

	// --- Bin soft-acquire race: ReserveForDispatch fails (another order reserved
	// the bin in the find→reserve window), order requeues waiting on material. ---
	t.Run("bin reserve race sets waiting_for_material", func(t *testing.T) {
		f := newFakeStore()
		order := seedQueuedRetrieve(f, 12, "LINE-C")
		order.PayloadCode = "PN-CLAIM"
		f.nodesByDot["LINE-C"] = &nodes.Node{ID: 103, Name: "LINE-C"}
		f.errReserveBin = errClaim // soft-reserve fails — the Rule-1 analog of the old claim race
		finder := foundFinderWith(40, "SRC-C")
		s := newScannerWith(t, f, finder, &recordingDispatcher{}, nil)
		s.RunOnce()

		// The plain path resolves dest + reserves the slot soft FIRST, then tries to
		// soft-acquire the bin; the race fails and requeues under material (lock-race).
		found := false
		for _, qr := range f.queueReasons {
			if qr.OrderID == 12 && qr.Code == string(protocol.QueueWaitingForMaterial) &&
				qr.Cause == "lock-race" {
				found = true
			}
		}
		if !found {
			t.Fatalf("soft-reserve race did not record waiting_for_material/lock-race; got %v", f.queueReasons)
		}
	})

	// --- Destination slot reserve conflict → waiting_for_slot ---
	t.Run("slot reserve conflict sets waiting_for_slot", func(t *testing.T) {
		f := newFakeStore()
		order := seedQueuedRetrieve(f, 13, "LINE-D")
		order.PayloadCode = "PN-SLOT"
		f.nodesByDot["LINE-D"] = &nodes.Node{ID: 104, Name: "LINE-D"}
		finder := foundFinderWith(50, "SRC-D")
		d := &recordingDispatcher{reserveErr: errReserve}
		s := newScannerWith(t, f, finder, d, nil)
		s.RunOnce()

		found := false
		for _, qr := range f.queueReasons {
			if qr.OrderID == 13 && qr.Code == string(protocol.QueueWaitingForSlot) &&
				qr.Cause == "slot-reserved" {
				found = true
			}
		}
		if !found {
			t.Fatalf("slot-reserve conflict did not record waiting_for_slot/slot-reserved; got %v", f.queueReasons)
		}
	})
}

// assertQueueReason finds the LAST recorded queue-reason write for the order
// (the requeue writes after any earlier gate write) and checks all four fields.
func assertQueueReason(t *testing.T, f *fakeStore, want queueReasonUpdate) {
	t.Helper()
	var last queueReasonUpdate
	found := false
	for _, qr := range f.queueReasons {
		if qr.OrderID == want.OrderID {
			last = qr
			found = true
		}
	}
	if !found {
		t.Fatalf("no queue_reason recorded for order %d; writes were %v", want.OrderID, f.queueReasons)
	}
	if last.Reason != want.Reason || last.Code != want.Code || last.Cause != want.Cause {
		t.Errorf("queue_reason = %+v, want %+v", last, want)
	}
}

// foundFinderWith returns a finder that reports a found bin at the given node.
func foundFinderWith(binID int64, nodeName string) BinFinder {
	return &fakeFinder{result: dispatch.SourceResult{
		Outcome: dispatch.OutcomeFound,
		Bin:     &bins.Bin{ID: binID},
		Node:    &nodes.Node{ID: 900, Name: nodeName},
	}}
}
