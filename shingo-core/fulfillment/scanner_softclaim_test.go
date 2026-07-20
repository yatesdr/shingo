package fulfillment

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// scanner_softclaim_test.go pins Rule 1 — the simple (single-transport) path is
// SOFT UNTIL COMPLETE: it soft-holds its destination slot and its source bin
// (pending reservations, no hard claimed_by) while it waits, and hard-claims BOTH
// only at dispatch, in one confirm step, immediately before the fleet call.
//
// These characterization tests would FAIL against the pre-Rule-1 scanner, which
// hard-claimed the bin (ClaimForDispatch) BEFORE reserving the slot — so a bin was
// stranded hard the moment the order waited on its slot.

// foundFinderFor returns a finder that reports a found bin at a resolved source node.
func foundFinderFor(binID int64, nodeName string) *fakeFinder {
	return &fakeFinder{result: dispatch.SourceResult{
		Outcome: dispatch.OutcomeFound,
		Bin:     &bins.Bin{ID: binID},
		Node:    &nodes.Node{ID: 900 + binID, Name: nodeName},
	}}
}

// storageDest seeds a node typed as a concrete storage dropoff (a STOR child) so
// isStorageDropoff is true and the slot leg of confirm-at-dispatch actually runs.
func storageDest(f *fakeStore, name string, id int64) *nodes.Node {
	n := &nodes.Node{ID: id, Name: name, NodeTypeCode: protocol.NodeClassSTOR}
	f.nodesByDot[name] = n
	return n
}

// TestRule1_NoHardClaimWhileWaitingForSlot is the headline Rule-1 invariant: a
// simple order waiting on its destination slot holds NO hard bin claim. The bin is
// soft-reserved only; no ConfirmClaim runs until dispatch. At HEAD (pre-Rule-1) the
// bin was hard-claimed before the slot attempt, so a slot wait stranded a hard claim.
func TestRule1_NoHardClaimWhileWaitingForSlot(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 200, "STOR-01")
	order.PayloadCode = "PN-R1"
	storageDest(f, "STOR-01", 2000)
	// Slot reserve fails (another order holds the slot) → the order parks in sourcing.
	dispatcher := &recordingDispatcher{reserveErr: errors.New("slot held by order 999")}
	s := newScannerWith(t, f, foundFinderFor(201, "SRC-01"), dispatcher, func(int64, string, string) {
		t.Errorf("soft-holding must not fail the order")
	})

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (parked on slot wait)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("waiting on a slot must record NO hard bin claim: %v", f.claimedBins)
	}
	if len(f.confirmedBins) != 0 {
		t.Errorf("waiting on a slot must NOT confirm the bin: %v", f.confirmedBins)
	}
	if len(dispatcher.confirmCalls) != 0 {
		t.Errorf("confirm-at-dispatch must not run while waiting on a slot: %+v", dispatcher.confirmCalls)
	}
	// The order parks in SOURCING (never queued), under waiting_for_slot.
	last := f.queueReasons[len(f.queueReasons)-1]
	if last.Code != string(protocol.QueueWaitingForSlot) {
		t.Errorf("queue_code = %q, want waiting_for_slot", last.Code)
	}
	for _, u := range f.statusUpdates {
		if u.Status != string(protocol.StatusSourcing) {
			t.Fatalf("a soft-holding order parks in sourcing, got trail %v", f.statusUpdates)
		}
	}
}

// TestRule1_ConfirmAtDispatchHardClaimsBoth: once dispatch proceeds, the confirm
// step hard-claims BOTH the slot (storage dest) and the bin — exactly once, and
// only at dispatch (not at acquire). Before dispatch the bin is soft only.
func TestRule1_ConfirmAtDispatchHardClaimsBoth(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 201, "STOR-02")
	order.PayloadCode = "PN-R1B"
	storageDest(f, "STOR-02", 2001)
	dispatcher := &recordingDispatcher{}
	s := newScannerWith(t, f, foundFinderFor(202, "SRC-02"), dispatcher, func(int64, string, string) {
		t.Errorf("happy path must not fail the order")
	})

	if got := s.RunOnce(); got != 1 {
		t.Fatalf("RunOnce: got %d, want 1 (dispatched)", got)
	}
	// One confirm-at-dispatch call carrying the bin id + the storage dest.
	if len(dispatcher.confirmCalls) != 1 {
		t.Fatalf("confirm-at-dispatch calls = %d, want 1: %+v", len(dispatcher.confirmCalls), dispatcher.confirmCalls)
	}
	cc := dispatcher.confirmCalls[0]
	if cc.orderID != 201 || cc.binID != 202 || cc.dest != "STOR-02" {
		t.Errorf("confirm call = %+v, want {order 201, bin 202, dest STOR-02}", cc)
	}
	// No legacy hard ClaimForDispatch ran from the scanner path.
	if len(f.claimedBins) != 0 {
		t.Errorf("scanner path must not use the legacy fused hard claim: %v", f.claimedBins)
	}
}

// TestRule1_ReentryReusesOwnBin: a soft-holding order re-entered on a later tick
// (still sourcing, BinID set at soft-acquire) reuses ITS OWN bin — the finder is
// never consulted again, no second bin is reserved. This is the owner-aware keep
// arm: the SQL finders exclude pending-reserved bins owner-blind, so re-finding
// would shop a second bin and double-source.
func TestRule1_ReentryReusesOwnBin(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	heldBin := int64(203)
	order := &orders.Order{
		ID: 202, Status: protocol.StatusSourcing, OrderType: protocol.OrderTypeRetrieve,
		BinID: &heldBin, SourceNode: "SRC-03", DeliveryNode: "STOR-03", PayloadCode: "PN-R1C",
	}
	f.queued = append(f.queued, order)
	f.ordersByID[202] = order
	f.nodesByDot["SRC-03"] = &nodes.Node{ID: 903, Name: "SRC-03"}
	storageDest(f, "STOR-03", 2003)
	// A found finder that, if consulted, would return a DIFFERENT bin — proving the
	// held bin is reused, not re-found.
	finder := foundFinderFor(9999, "WRONG-SRC")
	dispatcher := &recordingDispatcher{}
	s := newScannerWith(t, f, finder, dispatcher, func(int64, string, string) {
		t.Errorf("re-entry must not fail the order")
	})

	if got := s.RunOnce(); got != 1 {
		t.Fatalf("RunOnce: got %d, want 1 (re-entered and dispatched)", got)
	}
	if len(finder.calls) != 0 {
		t.Errorf("re-entry must NOT consult the finder (owner-aware reuse): %+v", finder.calls)
	}
	// The held bin (203) is reused — confirm carries 203, not 9999.
	if len(dispatcher.confirmCalls) != 1 || dispatcher.confirmCalls[0].binID != 203 {
		t.Errorf("confirm = %+v, want the held bin 203 (reused, not re-found)", dispatcher.confirmCalls)
	}
}

// TestRule1_ConfirmFailKeepsSoftHold: a confirm-at-dispatch failure (e.g. the pending
// reservation was reaped) parks the order in sourcing under claim_failed and KEEPS
// the soft hold — next tick re-enters via BinID and re-confirms (owner-idempotent).
// No hard claim is left stranded on a non-dispatched order.
func TestRule1_ConfirmFailKeepsSoftHold(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	heldBin := int64(204)
	order := &orders.Order{
		ID: 203, Status: protocol.StatusSourcing, OrderType: protocol.OrderTypeRetrieve,
		BinID: &heldBin, SourceNode: "SRC-04", DeliveryNode: "STOR-04", PayloadCode: "PN-R1D",
	}
	f.queued = append(f.queued, order)
	f.ordersByID[203] = order
	f.nodesByDot["SRC-04"] = &nodes.Node{ID: 904, Name: "SRC-04"}
	storageDest(f, "STOR-04", 2004)
	dispatcher := &recordingDispatcher{confirmErr: errors.New("pending reservation reaped")}
	s := newScannerWith(t, f, foundFinderFor(204, "SRC-04"), dispatcher, func(int64, string, string) {
		t.Errorf("confirm failure must requeue, not fail the order")
	})

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (confirm failed, requeued)", got)
	}
	if len(dispatcher.confirmCalls) != 1 {
		t.Fatalf("confirm must be attempted once: %+v", dispatcher.confirmCalls)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("a confirm failure must leave NO hard claim stranded: %v", f.claimedBins)
	}
	last := f.queueReasons[len(f.queueReasons)-1]
	if last.Code != string(protocol.QueueWaitingForMaterial) || last.Cause != "claim-failed" {
		t.Errorf("queue on confirm fail = %+v, want waiting_for_material/claim-failed", last)
	}
	for _, u := range f.statusUpdates {
		if u.Status != string(protocol.StatusSourcing) {
			t.Fatalf("a confirm-failed order stays in sourcing, got %v", f.statusUpdates)
		}
	}
}
