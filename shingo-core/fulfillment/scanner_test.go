package fulfillment

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Scope note:
//
// These tests exercise the scanner's pre-dispatch control flow — the
// branches of tryFulfill that return false before
// s.dispatcher.DispatchDirect is invoked. That covers every
// "skip / unclaim / re-queue" path the production scanner walks when
// inventory is missing, the order was cancelled, the destination is
// still occupied, the bin claim fails, or an intermediate lookup
// errors out.
//
// The happy-path dispatch, the NGRP structural-error path, and the
// sendToEdge ack/waybill path are intentionally out of scope: both
// *dispatch.Dispatcher and *dispatch.DefaultResolver are concrete
// types holding *store.DB, and stubbing them requires either a
// database or a larger scanner refactor (dependency inversion for
// the dispatcher/resolver fields). The integration-level test
// TestFulfillmentScanner_QueueToDispatch in engine/ already covers
// the green path end-to-end; these unit tests fill in the branches
// that were previously only exercised by production traffic.

// newTestScanner builds a Scanner wired to a fake store and no-op
// callbacks. Dispatcher and resolver are left nil — every test here
// stays on paths that return before invoking them.
func newTestScanner(t *testing.T, db Store) *Scanner {
	t.Helper()
	return NewScanner(
		db,
		nil, // dispatcher — not reached on any tested path
		nil, // lifecycle — not reached on any tested path
		nil, // resolver — not reached on any tested path
		func(string, string, any) error { return nil },
		func(orderID int64, code, detail string) {
			t.Errorf("unexpected failFn call: order=%d code=%s detail=%s",
				orderID, code, detail)
		},
		t.Logf,
		nil,
	)
}

// seedQueuedRetrieve installs a simple retrieve order and the
// delivery node lookup it will perform. Source node lookups default
// to empty so tryFulfill falls through to FindSourceBinFIFO.
func seedQueuedRetrieve(f *fakeStore, orderID int64, deliveryNode string) *orders.Order {
	order := &orders.Order{
		ID:           orderID,
		Status:       protocol.StatusQueued,
		PayloadCode:  "PN-123",
		DeliveryNode: deliveryNode,
	}
	f.queued = append(f.queued, order)
	f.ordersByID[orderID] = order
	f.nodesByDot[deliveryNode] = &nodes.Node{ID: 100, Name: deliveryNode, Zone: "ZONE-A"}
	return order
}

// ── scan() branches ─────────────────────────────────────────────────

func TestScanner_RunOnce_NoQueuedOrders(t *testing.T) {
	f := newFakeStore()
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce with no queued orders: got %d, want 0", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("no bins should be claimed: got %v", f.claimedBins)
	}
}

func TestScanner_ListQueuedOrders_ErrorReturnsZero(t *testing.T) {
	f := newFakeStore()
	f.errListQueued = errors.New("db down")
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce when ListQueuedOrders errors: got %d, want 0", got)
	}
}

// ── tryFulfill() branches ────────────────────────────────────────────

func TestScanner_TryFulfill_OrderCancelledBetweenListAndFetch(t *testing.T) {
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 1, "dest-01")
	// GetOrder returns a fresh copy where the order has since flipped
	// out of queued (cancelled/failed/etc). tryFulfill must skip it.
	f.ordersByID[order.ID] = &orders.Order{
		ID:     order.ID,
		Status: protocol.StatusCancelled,
	}
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (order cancelled mid-scan)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("cancelled order should not claim a bin: %v", f.claimedBins)
	}
}

func TestScanner_TryFulfill_InFlightDeliveryNode_Skipped(t *testing.T) {
	f := newFakeStore()
	seedQueuedRetrieve(f, 2, "dest-02")
	f.inFlightAt["dest-02"] = 1 // another order already heading there
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (in-flight delivery blocks)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("in-flight block should prevent claim: %v", f.claimedBins)
	}
}

func TestScanner_TryFulfill_DestNodeHasBin_Skipped(t *testing.T) {
	f := newFakeStore()
	seedQueuedRetrieve(f, 3, "dest-03")
	// Node lookup succeeds; the node still has a bin parked on it
	// (e.g. the previous order's outbound hasn't been unloaded yet).
	f.binsAtNode[100] = 1
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (dest node occupied)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("dest-occupied block should prevent claim: %v", f.claimedBins)
	}
}

func TestScanner_TryFulfill_EmptyPayloadCode_Skipped(t *testing.T) {
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 4, "dest-04")
	order.PayloadCode = "" // no payload = nothing to source
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (empty payload)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("empty-payload order should not claim a bin: %v", f.claimedBins)
	}
}

func TestScanner_TryFulfill_RetrieveEmpty_NoBinAvailable_UsesDestZone(t *testing.T) {
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 5, "dest-05")
	order.PayloadDesc = "retrieve_empty"
	// delivery node has Zone="ZONE-A" already — preferZone should
	// derive from it.
	f.errFindEmptyBin = errors.New("no empties available")
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (no empty bin)", got)
	}
	if len(f.findEmptyPrefZones) != 1 {
		t.Fatalf("FindEmptyCompatibleBin should be called once, got %d", len(f.findEmptyPrefZones))
	}
	got := f.findEmptyPrefZones[0]
	if got.PayloadCode != "PN-123" {
		t.Errorf("payload code: got %q, want %q", got.PayloadCode, "PN-123")
	}
	if got.PreferZone != "ZONE-A" {
		t.Errorf("preferZone derivation: got %q, want %q (dest node's zone)",
			got.PreferZone, "ZONE-A")
	}
}

func TestScanner_TryFulfill_Retrieve_NoSourceBin_Skipped(t *testing.T) {
	f := newFakeStore()
	seedQueuedRetrieve(f, 6, "dest-06")
	f.errFindSourceBinFIFO = errors.New("no source bin")
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (no source bin)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("no-source should not claim a bin: %v", f.claimedBins)
	}
}

func TestScanner_TryFulfill_ClaimBinFails_SkipsSilently(t *testing.T) {
	f := newFakeStore()
	seedQueuedRetrieve(f, 7, "dest-07")
	sourceNodeID := int64(200)
	f.sourceBin = &bins.Bin{ID: 42, NodeID: &sourceNodeID, PayloadCode: "PN-123"}
	f.errClaimBin = errors.New("already claimed by another scanner")
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (claim contention)", got)
	}
	if len(f.unclaimedOrderIDs) != 0 {
		t.Errorf("failed claim should not trigger unclaim: %v", f.unclaimedOrderIDs)
	}
	if len(f.statusUpdates) != 0 {
		t.Errorf("failed claim should not trigger status change: %v", f.statusUpdates)
	}
}

func TestScanner_TryFulfill_GetNodeAfterClaimFails_Unclaims(t *testing.T) {
	f := newFakeStore()
	seedQueuedRetrieve(f, 8, "dest-08")
	sourceNodeID := int64(201)
	f.sourceBin = &bins.Bin{ID: 43, NodeID: &sourceNodeID, PayloadCode: "PN-123"}
	// Bin claim succeeds, but the source-node lookup fails before the
	// order can be updated. The scanner must release the claim so
	// the bin is selectable by the next pass.
	f.errGetNode = errors.New("node disappeared")
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (node lookup failed)", got)
	}
	if len(f.claimedBins) != 1 {
		t.Fatalf("bin should have been claimed before the failure: got %v", f.claimedBins)
	}
	if len(f.unclaimedOrderIDs) != 1 || f.unclaimedOrderIDs[0] != 8 {
		t.Errorf("unclaim should run for order 8: got %v", f.unclaimedOrderIDs)
	}
	if len(f.statusUpdates) != 0 {
		t.Errorf("status should not be touched when source node missing: %v", f.statusUpdates)
	}
}

func TestScanner_TryFulfill_DestNodeLookupFails_UnclaimsAndRequeues(t *testing.T) {
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 9, "dest-09")
	sourceNodeID := int64(202)
	f.sourceBin = &bins.Bin{ID: 44, NodeID: &sourceNodeID, PayloadCode: "PN-123"}
	f.nodesByID[sourceNodeID] = &nodes.Node{ID: sourceNodeID, Name: "src-09"}

	// First GetNodeByDotName succeeds (bin-occupancy check);
	// second call (final destination resolution) fails.
	var calls int32
	dest := f.nodesByDot[order.DeliveryNode]
	f.getNodeByDotNameFn = func(name string) (*nodes.Node, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return dest, nil
		}
		return nil, errors.New("dest vanished")
	}

	s := newTestScanner(t, f)
	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (dest lookup failed after claim)", got)
	}

	if len(f.claimedBins) != 1 {
		t.Fatalf("bin should have been claimed before dest lookup: got %v", f.claimedBins)
	}
	if len(f.unclaimedOrderIDs) != 1 || f.unclaimedOrderIDs[0] != 9 {
		t.Errorf("unclaim should run for order 9: got %v", f.unclaimedOrderIDs)
	}

	// Status trail: StatusSourcing (bin found) → StatusQueued (re-queue)
	if len(f.statusUpdates) != 2 {
		t.Fatalf("expected 2 status updates, got %d: %v", len(f.statusUpdates), f.statusUpdates)
	}
	if f.statusUpdates[0].Status != protocol.StatusSourcing {
		t.Errorf("first status: got %q, want %q", f.statusUpdates[0].Status, protocol.StatusSourcing)
	}
	if f.statusUpdates[1].Status != protocol.StatusQueued {
		t.Errorf("second status: got %q, want %q (re-queue on transient dest miss)",
			f.statusUpdates[1].Status, protocol.StatusQueued)
	}
}

// ── RunOnce coalescing + periodic sweep ─────────────────────────────

func TestScanner_RunOnce_TriggerDuringScan_RerunsOnce(t *testing.T) {
	f := newFakeStore()
	s := newTestScanner(t, f)

	// Simulate an event arriving mid-scan: the fake sets pending=true
	// by calling s.Trigger() when ListQueuedOrders is entered.
	// After the first scan returns, RunOnce sees pending=true and
	// calls scan() a second time; on that second call the hook is
	// already cleared (we unset it below) so it doesn't loop.
	var calls int
	f.onListQueuedOrders = func() {
		calls++
		if calls == 1 {
			s.Trigger()
		}
	}

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce fulfilled unexpectedly: got %d, want 0", got)
	}
	if calls != 2 {
		t.Errorf("RunOnce should coalesce into exactly 2 scans, got %d", calls)
	}
}

func TestScanner_StartPeriodicSweep_StopHaltsLoop(t *testing.T) {
	f := newFakeStore()
	s := newTestScanner(t, f)

	// Counter is installed before the sweep starts so there's no
	// data race with the goroutine's ListQueuedOrders call.
	var scanCount int32
	f.onListQueuedOrders = func() { atomic.AddInt32(&scanCount, 1) }

	s.StartPeriodicSweep(5 * time.Millisecond)
	time.Sleep(25 * time.Millisecond) // allow ~5 ticks
	s.Stop()

	afterStop := atomic.LoadInt32(&scanCount)
	if afterStop == 0 {
		t.Fatalf("sweep should have fired at least once before Stop")
	}

	// A tick firing concurrent with Stop can race through one more
	// RunOnce before the stopChan select wins, so tolerate +1 but
	// no more — otherwise the loop is still running.
	time.Sleep(40 * time.Millisecond)
	final := atomic.LoadInt32(&scanCount)
	if final > afterStop+1 {
		t.Errorf("sweep ran %d extra times after Stop (%d → %d), want ≤ 1",
			final-afterStop, afterStop, final)
	}
}
