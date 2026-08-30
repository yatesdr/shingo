package fulfillment

import (
	"errors"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// Sentinel errors injected into the fakes to drive each requeue path.
var (
	errFleet = errors.New("fleet create failed")
	errClaim = errors.New("claim race")
	// errReserve is a GENUINE slot-contention refusal, wrapping the sentinel the
	// reserve arm tags contention with. It used to be a bare errors.New, which is
	// what let a hard database error read as contention: every failure classified
	// the same way, so a test that injected "anything at all" and asserted
	// "slot-reserved" was passing on the defect.
	errReserve = fmt.Errorf("%w: another order holds it", dispatch.ErrSlotContended)
	// errReserveRead is the OTHER shape at the same door: the reserve could not be
	// evaluated at all. Nothing is known about the slot.
	errReserveRead = fmt.Errorf("count bins at LINE-D: %w", dispatch.ErrSlotUnreadable)
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

	// --- An UNREADABLE destination is not a contended one ---
	//
	// THE LIVE MIS-CLASSIFICATION. Both scanner sites derived this cause
	// themselves, from the same four lines: unresolved-group if
	// IsSyntheticUnresolved, otherwise slot-contended. So a failed database read
	// — the count, the reserve write, the settle read — parked as "the
	// destination slot is contended", which sends an operator to a slot that is
	// probably empty, to wait for a carrier that is not there, on a wait that
	// will not clear until a database does.
	//
	// TEST-CERTIFIED, NOT RUN-CERTIFIED: a hard DB error is not producible by any
	// fixture, which is why this seam exists at all.
	t.Run("unreadable destination is not contention", func(t *testing.T) {
		f := newFakeStore()
		order := seedQueuedRetrieve(f, 14, "LINE-E")
		order.PayloadCode = "PN-READ"
		f.nodesByDot["LINE-E"] = &nodes.Node{ID: 105, Name: "LINE-E"}
		finder := foundFinderWith(51, "SRC-E")
		d := &recordingDispatcher{reserveErr: errReserveRead}
		s := newScannerWith(t, f, finder, d, nil)
		s.RunOnce()

		for _, qr := range f.queueReasons {
			if qr.OrderID != 14 {
				continue
			}
			if qr.Cause == string(dispatch.CauseStoreSlotContended) {
				t.Fatalf("a failed READ parked as %q. Nothing about the slot was established — "+
					"telling an operator it is contended invents a fact and points them at the "+
					"wrong end of the plant", qr.Cause)
			}
			if qr.Cause != string(dispatch.CauseCapacityCheckFailed) {
				t.Fatalf("an unreadable destination parked under %q, want %q — the undetermined "+
					"family", qr.Cause, dispatch.CauseCapacityCheckFailed)
			}
			return
		}
		t.Fatalf("order 14 recorded no queue reason at all; got %v", f.queueReasons)
	})

	// --- An UNRESOLVED GROUP is not a contended slot either ---
	t.Run("unresolved group names the resolution, not the slot", func(t *testing.T) {
		f := newFakeStore()
		order := seedQueuedRetrieve(f, 15, "SYN-GRP")
		order.PayloadCode = "PN-GRP"
		f.nodesByDot["SYN-GRP"] = &nodes.Node{ID: 106, Name: "SYN-GRP", IsSynthetic: true}
		finder := foundFinderWith(52, "SRC-G")
		unresolved := dispatch.SyntheticUnresolved{OrderID: 15, Group: "SYN-GRP",
			Err: errors.New("no available slot in node group SYN-GRP")}
		d := &recordingDispatcher{reserveErr: unresolved}
		s := newScannerWith(t, f, finder, d, nil)
		s.RunOnce()

		for _, qr := range f.queueReasons {
			if qr.OrderID != 15 {
				continue
			}
			if qr.Cause != string(dispatch.CauseNGRPResolve) {
				t.Fatalf("a destination that was never narrowed to one position parked under %q, "+
					"want %q — the row blames the slot layer for a resolution that never ran",
					qr.Cause, dispatch.CauseNGRPResolve)
			}
			return
		}
		t.Fatalf("order 15 recorded no queue reason at all; got %v", f.queueReasons)
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
