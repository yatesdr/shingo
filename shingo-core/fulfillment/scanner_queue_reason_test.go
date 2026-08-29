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
//
// THE TWO FLEET ARMS ASSERT THE DOOR, not the store. A fleet refusal goes
// through one place now (Dispatcher.DemoteAfterFleetRefusal), which writes the
// cause in the same breath as it takes the armor off and demotes the paper — the
// scanner's part is to NAME the wait and hand it over. So what these two prove
// is that the scanner still names it correctly; that the name reaches the row is
// the door's own test (dispatch/fleet_demote_docker_test.go).
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

		assertDemoted(t, failDispatch, demoteCall{orderID: 10,
			code: string(protocol.QueueFleetUnavailable), cause: "fleet-error"})
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

		assertDemoted(t, failDispatch, demoteCall{orderID: 11,
			code: string(protocol.QueueFleetUnavailable), cause: "fleet-error"})
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
			t.Fatalf("the store's destination-slot conflict did not record waiting_for_slot/slot-reserved; got %v", f.queueReasons)
		}
	})
}

// foundFinderWith returns a finder that reports a found bin at the given node.
func foundFinderWith(binID int64, nodeName string) BinFinder {
	return &fakeFinder{result: dispatch.SourceResult{
		Outcome: dispatch.OutcomeFound,
		Bin:     &bins.Bin{ID: binID},
		Node:    &nodes.Node{ID: 900, Name: nodeName},
	}}
}

// assertDemoted checks that a fleet refusal reached the one door for this order,
// under the expected name. It reports what the door DID see, because "the wait
// was never named" and "the wait was named wrongly" are different defects and a
// bare "not found" cannot tell them apart.
func assertDemoted(t *testing.T, d *recordingDispatcher, want demoteCall) {
	t.Helper()
	for _, got := range d.demoteCalls {
		if got == want {
			return
		}
	}
	t.Errorf("order %d did not reach the fleet-refusal door as %+v; the door saw %+v",
		want.orderID, want, d.demoteCalls)
}
