package fulfillment

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Scope note:
//
// These tests exercise the scanner's pre-dispatch control flow — the branches of
// tryFulfill up to and including dispatch. Source finding itself is NOT tested
// here: it moved to the shared dispatch.SourceFinder (dispatch/source_finder_test.go
// pins the tier cascade). The scanner holds the finder behind the one-method
// BinFinder interface, so these tests drive a fakeFinder returning a canned
// outcome and assert the scanner's orchestration around it — claim, rollback,
// re-queue, dispatch, and the store short-circuit.

// stubLifecycle implements the narrow Lifecycle interface for tests by writing
// transition calls through to the fake store's UpdateOrderStatus (kept on the
// fake though it's no longer a Store method) so test expectations like "expected
// Sourcing then Queued" keep asserting against statusUpdates.
type stubLifecycle struct {
	db *fakeStore
}

func (s stubLifecycle) MoveToSourcing(ord *orders.Order, _, reason string) error {
	return s.db.UpdateOrderStatus(ord.ID, string(protocol.StatusSourcing), reason)
}

func (s stubLifecycle) Queue(ord *orders.Order, _, reason string) error {
	return s.db.UpdateOrderStatus(ord.ID, string(protocol.StatusQueued), reason)
}

// fakeFinder stubs BinFinder: it records each call and returns a canned outcome.
type fakeFinder struct {
	result dispatch.SourceResult
	calls  []fakeFinderCall
}

type fakeFinderCall struct {
	orderID int64
	intent  dispatch.Intent
}

func (f *fakeFinder) FindSource(order *orders.Order, intent dispatch.Intent) dispatch.SourceResult {
	f.calls = append(f.calls, fakeFinderCall{orderID: order.ID, intent: intent})
	return f.result
}

// waitFinder returns a finder whose FindSource reports a wait under the given
// queue code + params. (Older callers passed a free-text reason; the SourceResult
// now carries the structured code + params the sentence is generated from.)
func waitFinder(code protocol.QueueCode, params dispatch.QueueParams) *fakeFinder {
	return &fakeFinder{result: dispatch.SourceResult{
		Outcome:     dispatch.OutcomeWait,
		QueueCode:   code,
		QueueCause:  "finder-test",
		QueueParams: params,
	}}
}

func foundFinder(binID int64, nodeName string) *fakeFinder {
	return &fakeFinder{result: dispatch.SourceResult{
		Outcome: dispatch.OutcomeFound,
		Bin:     &bins.Bin{ID: binID},
		Node:    &nodes.Node{ID: 900 + binID, Name: nodeName},
	}}
}

// recordingDispatcher records DispatchDirect for the paths that reach dispatch.
type recordingDispatcher struct {
	directCalls []directCall
	directErr   error

	// reserveErr, when set, makes ReserveStorageDropoff fail — drives the slot-
	// reserve conflict requeue on both the plain and held-bin paths.
	reserveErr error

	// confirmCalls records each ConfirmForDispatch (the Rule-1 confirm-at-dispatch
	// step); confirmErr drives the confirm-failure requeue.
	confirmCalls []confirmCall
	confirmErr   error

	// reshuffleCalls records orders the scanner asked to reshuffle on replay;
	// reshuffleErr drives the transient-vs-structural disposition.
	reshuffleCalls []int64
	reshuffleErr   error
}

// confirmCall records one Rule-1 confirm-at-dispatch: the order, the bin, and the
// resolved source/dest nodes (so tests can assert slot-vs-line dest routing).
type confirmCall struct {
	orderID int64
	binID   int64
	source  string
	dest    string
}

type directCall struct {
	orderID    int64
	sourceNode string
	destNode   string
}

func (d *recordingDispatcher) DispatchDirect(o *orders.Order, src, dst *nodes.Node) (string, error) {
	d.directCalls = append(d.directCalls, directCall{orderID: o.ID, sourceNode: src.Name, destNode: dst.Name})
	if d.directErr != nil {
		return "", d.directErr
	}
	return "V-1", nil
}

func (d *recordingDispatcher) DispatchPreparedComplex(*orders.Order) error {
	panic("recordingDispatcher: complex path not expected in this test")
}

// ReserveStorageDropoff honors reserveErr so the slot-reserve conflict requeue
// is exercisable here; the node-driven reserve is also covered end-to-end in the
// dispatch package's docker tests.
func (d *recordingDispatcher) ReserveStorageDropoff(*orders.Order) error {
	return d.reserveErr
}

// ConfirmForDispatch records the Rule-1 confirm-at-dispatch step and honors
// confirmErr so the confirm-failure requeue is exercisable here.
func (d *recordingDispatcher) ConfirmForDispatch(o *orders.Order, binID int64, src, dst *nodes.Node) error {
	d.confirmCalls = append(d.confirmCalls, confirmCall{orderID: o.ID, binID: binID, source: src.Name, dest: dst.Name})
	return d.confirmErr
}
func (d *recordingDispatcher) PostFindHook() {}

func (d *recordingDispatcher) PlanBuriedReshuffle(o *orders.Order, _ *dispatch.BuriedError) error {
	d.reshuffleCalls = append(d.reshuffleCalls, o.ID)
	return d.reshuffleErr
}

// newTestScanner wires a scanner whose finder always waits and whose dispatcher
// is nil — for the pre-finder branches (cancelled, in-flight, dest occupied,
// empty-payload guard) that never reach dispatch. failFn t.Errorf's on any call
// so a guard regression fails the test automatically.
func newTestScanner(t *testing.T, f *fakeStore) *Scanner {
	t.Helper()
	return newScannerWith(t, f, waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Payload: "PN-123"}), nil,
		func(orderID int64, code, detail string) {
			t.Errorf("unexpected failFn call: order=%d code=%s detail=%s", orderID, code, detail)
		})
}

func newScannerWith(t *testing.T, f *fakeStore, finder BinFinder, dispatcher Dispatcher, failFn func(int64, string, string)) *Scanner {
	t.Helper()
	return NewScanner(
		f,
		dispatcher,
		stubLifecycle{db: f},
		finder,
		f, // claimer
		func(string, string, any) error { return nil },
		failFn,
		t.Logf,
		nil,
	)
}

// seedQueuedRetrieve installs a simple retrieve order and its delivery node.
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
	t.Parallel()
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
	t.Parallel()
	f := newFakeStore()
	f.errListQueued = errors.New("db down")
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce when ListQueuedOrders errors: got %d, want 0", got)
	}
}

// ── tryFulfill() pre-finder branches ─────────────────────────────────

func TestScanner_TryFulfill_OrderCancelledBetweenListAndFetch(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 1, "dest-01")
	f.ordersByID[order.ID] = &orders.Order{ID: order.ID, Status: protocol.StatusCancelled}
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (order cancelled mid-scan)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("cancelled order should not claim a bin: %v", f.claimedBins)
	}
}

func TestScanner_TryFulfill_InFlightDeliveryNode_Skipped(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f := newFakeStore()
	seedQueuedRetrieve(f, 3, "dest-03")
	f.binsAtNode[100] = 1
	s := newTestScanner(t, f)

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (dest node occupied)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("dest-occupied block should prevent claim: %v", f.claimedBins)
	}
}

// A blank payload on a plain retrieve is a construction bug — route through failFn.
func TestScannerScanForRetrieve_FailsCleanlyOnEmptyPayload(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 4, "dest-04")
	order.PayloadCode = "" // no payload = nothing to source

	var failCalls []struct {
		orderID int64
		code    string
		detail  string
	}
	s := newScannerWith(t, f, waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Payload: "PN-123"}), nil, func(orderID int64, code, detail string) {
		failCalls = append(failCalls, struct {
			orderID int64
			code    string
			detail  string
		}{orderID, code, detail})
	})

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (empty payload)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("empty-payload order should not claim a bin: %v", f.claimedBins)
	}
	if len(failCalls) != 1 {
		t.Fatalf("failFn should be called exactly once for empty payload, got %d", len(failCalls))
	}
	if fc := failCalls[0]; fc.orderID != order.ID || fc.code != "structural" || !strings.Contains(fc.detail, "empty payload_code") {
		t.Errorf("failFn call = %+v, want order %d code=structural detail~empty payload_code", fc, order.ID)
	}
}

// ── tryFulfill() finder-driven branches ──────────────────────────────

// retrieve_empty routes to the finder with IntentEmpty; a Wait leaves the order
// queued (no claim, no failFn).
func TestScanner_TryFulfill_RetrieveEmpty_FinderWaits_StaysQueued(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 5, "dest-05")
	order.OrderType = protocol.OrderTypeRetrieveEmpty
	order.SourceIntent = dispatch.SourceIntentEmpty // Stage 4: intent now data, set at intake
	finder := waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Kind: "empty", Payload: "PN-123"})
	s := newTestScanner(t, f)
	s.finder = finder // finder is white-box accessible in-package

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (no empty bin)", got)
	}
	if len(finder.calls) != 1 || finder.calls[0].intent != dispatch.IntentEmpty {
		t.Fatalf("finder should be called once with IntentEmpty, got %+v", finder.calls)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("no empty available should not claim a bin: %v", f.claimedBins)
	}
}

// A retrieve_empty carries a blank payload legitimately (agnostic empty request);
// the empty-payload guard must NOT fire — the order must reach the finder.
func TestScanner_TryFulfill_RetrieveEmpty_BlankPayload_ReachesFinder(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 9, "dest-09")
	order.OrderType = protocol.OrderTypeRetrieveEmpty
	order.SourceIntent = dispatch.SourceIntentEmpty // Stage 4: intent now data, set at intake
	order.PayloadCode = ""                          // blank is legitimate here
	finder := waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Kind: "empty", Payload: "PN-123"})
	s := newTestScanner(t, f)
	s.finder = finder

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (no empty → stays queued)", got)
	}
	if len(finder.calls) != 1 || finder.calls[0].intent != dispatch.IntentEmpty {
		t.Fatalf("blank retrieve_empty must reach the finder with IntentEmpty (guard must not fire), got %+v", finder.calls)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("no empty available should not claim a bin: %v", f.claimedBins)
	}
}

// A payload-less MOVE must NOT trip the empty-payload guard — it reaches the
// finder (IntentFull) and no failFn fires. [A6]
func TestScanner_TryFulfill_PayloadlessMove_ReachesFinder(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 15, "dest-15")
	order.OrderType = protocol.OrderTypeMove
	order.SourceIntent = dispatch.SourceIntentLocal // Stage 4: intent now data, set at intake
	order.PayloadCode = ""
	order.SourceNode = "MOVE-SRC"
	finder := waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Payload: "PN-123", Destination: "MOVE-SRC"})
	s := newTestScanner(t, f) // its failFn t.Errorf's on any call
	s.finder = finder

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (finder waits)", got)
	}
	if len(finder.calls) != 1 || finder.calls[0].intent != dispatch.IntentFull {
		t.Fatalf("payload-less move must reach the finder with IntentFull (guard exempt), got %+v", finder.calls)
	}
}

func TestScanner_TryFulfill_Retrieve_FinderWaits_Skipped(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	seedQueuedRetrieve(f, 6, "dest-06")
	finder := waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Payload: "PN-123"})
	s := newTestScanner(t, f)
	s.finder = finder

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (no source bin)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("no-source should not claim a bin: %v", f.claimedBins)
	}
	if len(f.queueReasons) != 1 {
		t.Fatalf("queue_reason should record the finder's wait, got %v", f.queueReasons)
	}
	qr := f.queueReasons[0]
	if qr.Code != string(protocol.QueueWaitingForMaterial) {
		t.Errorf("queue_code = %q, want waiting_for_material", qr.Code)
	}
	if qr.Reason != "Waiting for material: PN-123" {
		t.Errorf("queue_reason sentence = %q, want the generated material sentence", qr.Reason)
	}
}

// Under Rule 1 (soft until complete), a simple order whose bin SOFT-ACQUIRE fails
// (a lost reservation race) requeues to SOURCING — never queued — and records NO
// hard claim. Status trail: Sourcing (before acquire) then Sourcing (race requeue,
// idempotent self-transition recorded). The queue code is waiting_for_material
// (lock-race). This is the soft-reserve analog of the old claim-fail requeue.
func TestScannerSimpleSoftReserveFailRequeuesToSourcing(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	seedQueuedRetrieve(f, 7, "dest-07")
	f.errReserveBin = errors.New("reservation conflict: bin held by another order")
	dispatcher := &recordingDispatcher{}
	s := newScannerWith(t, f, foundFinder(42, "src-07"), dispatcher, func(int64, string, string) {})

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (soft-reserve race)", got)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("a failed soft-reserve must not record a hard claim: %v", f.claimedBins)
	}
	if len(f.confirmedBins) != 0 {
		t.Errorf("a failed soft-reserve must not confirm: %v", f.confirmedBins)
	}
	if len(dispatcher.confirmCalls) != 0 {
		t.Errorf("confirm-at-dispatch must not run when the soft reserve failed: %+v", dispatcher.confirmCalls)
	}
	if len(f.queueReasons) == 0 || f.queueReasons[len(f.queueReasons)-1].Code != string(protocol.QueueWaitingForMaterial) {
		t.Fatalf("queue_code on soft-reserve fail = %v, want waiting_for_material (lock-race)", f.queueReasons)
	}
	for _, u := range f.statusUpdates {
		if u.Status != string(protocol.StatusSourcing) {
			t.Fatalf("status trail on soft-reserve fail must stay in sourcing, got %v", f.statusUpdates)
		}
	}
}

// Under Rule 1, destination is resolved BEFORE the bin is acquired (slot-first).
// So an unresolvable destination requeues WITHOUT acquiring or claiming a bin — no
// hard claim is stranded while the order waits. Status trail: Sourcing (entry) then
// Sourcing (requeue). Queue code: waiting_for_material (dest-node-unresolved).
func TestScanner_TryFulfill_DestNodeLookupFails_RequeuesNoBinAcquired(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	// dest-09 is intentionally NOT registered: the capacity gate treats the lookup
	// miss as "not blocked" (passes), then the dest resolve fails and the scanner
	// requeues BEFORE acquiring a bin.
	order := &orders.Order{ID: 9, Status: protocol.StatusQueued, PayloadCode: "PN-123", DeliveryNode: "dest-09"}
	f.queued = append(f.queued, order)
	f.ordersByID[9] = order
	dispatcher := &recordingDispatcher{}
	s := newScannerWith(t, f, foundFinder(44, "src-09"), dispatcher, func(int64, string, string) {})

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0 (dest lookup failed before acquire)", got)
	}
	if len(f.reservedBins) != 0 {
		t.Errorf("dest-fail must happen BEFORE any bin acquire: reservedBins=%v", f.reservedBins)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("dest-fail must record no hard claim: %v", f.claimedBins)
	}
	if len(f.unclaimedOrderIDs) != 0 {
		t.Errorf("dest-fail before acquire has no claim to release: %v", f.unclaimedOrderIDs)
	}
	if len(f.queueReasons) == 0 || f.queueReasons[len(f.queueReasons)-1].Code != string(protocol.QueueWaitingForMaterial) {
		t.Fatalf("queue_code on dest-fail = %v, want waiting_for_material", f.queueReasons)
	}
	for _, u := range f.statusUpdates {
		if u.Status != string(protocol.StatusSourcing) {
			t.Fatalf("status trail on dest-fail must stay in sourcing, got %v", f.statusUpdates)
		}
	}
}

// A queued store dispatches the bin it already holds — no finder call, no second
// claim. [A5]
func TestStoreReplayDispatchesOwnClaimedBin(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	binID := int64(77)
	order := &orders.Order{
		ID:           4,
		Status:       protocol.StatusQueued,
		OrderType:    protocol.OrderTypeStore,
		BinID:        &binID,
		SourceNode:   "STORE-SRC",
		DeliveryNode: "STORE-DEST",
	}
	f.queued = append(f.queued, order)
	f.ordersByID[4] = order
	f.nodesByDot["STORE-SRC"] = &nodes.Node{ID: 500, Name: "STORE-SRC"}
	f.nodesByDot["STORE-DEST"] = &nodes.Node{ID: 501, Name: "STORE-DEST"} // empty → gate passes

	finder := waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Payload: "PN-123"})
	dispatcher := &recordingDispatcher{}
	s := newScannerWith(t, f, finder, dispatcher, func(int64, string, string) {})

	if got := s.RunOnce(); got != 1 {
		t.Fatalf("RunOnce: got %d, want 1 (store dispatched)", got)
	}
	if len(finder.calls) != 0 {
		t.Errorf("store must NOT consult the finder (it owns its bin): %+v", finder.calls)
	}
	if len(f.claimedBins) != 0 {
		t.Errorf("store must NOT re-claim a bin: %v", f.claimedBins)
	}
	if len(dispatcher.directCalls) != 1 || dispatcher.directCalls[0].orderID != 4 ||
		dispatcher.directCalls[0].sourceNode != "STORE-SRC" || dispatcher.directCalls[0].destNode != "STORE-DEST" {
		t.Errorf("store dispatch: got %+v, want one DispatchDirect(order 4, STORE-SRC → STORE-DEST)", dispatcher.directCalls)
	}
}

// ── widened scan set {queued, sourcing} ──────────────────

// A complex order sitting in `sourcing` (the widened scan set) is re-attempted —
// DispatchPreparedComplex's entry guard accepts IsAcquiring. A SIMPLE order in
// sourcing is scoped out (not re-sourced) to avoid a double-claim / the intake
// race until commit 4's reserve-reconcile lands.
func TestScannerRetriesSourcingOrder(t *testing.T) {
	t.Parallel()

	t.Run("complex sourcing order is retried", func(t *testing.T) {
		f := newFakeStore()
		dispatcher := &stubDispatcher{}
		s := newTestScannerWithDispatcher(t, f, dispatcher)
		f.nodesByDot["LINE_01"] = &nodes.Node{ID: 7, Name: "LINE_01"}
		order := &orders.Order{
			ID: 42, Status: protocol.StatusSourcing, OrderType: protocol.OrderTypeComplex,
			// Production complex orders are stamped coordinated at intake (IsCoordinated
			// reads the Coordinated column, not StepsJSON).
			Coordinated:  true,
			StepsJSON:    `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"LINE_01"}]`,
			DeliveryNode: "LINE_01", PayloadCode: "PN-X",
		}
		f.queued = append(f.queued, order)
		f.ordersByID[42] = order

		if got := s.RunOnce(); got != 1 {
			t.Fatalf("RunOnce: got %d, want 1 (sourcing complex order retried)", got)
		}
		if len(dispatcher.preparedCalls) != 1 || dispatcher.preparedCalls[0] != 42 {
			t.Errorf("DispatchPreparedComplex calls = %v, want [42]", dispatcher.preparedCalls)
		}
	})

	// Stage 3: the old :175 guard scoped simple `sourcing` orders OUT (they could
	// double-claim on re-source). That guard is gone; a simple order re-entered
	// from `sourcing` while HOLDING its bin now reuses it — the finder is never
	// consulted, no second bin is claimed (the length-1 idempotency). This is the
	// fake-level companion to the docker length-1 test.
	t.Run("simple sourcing order with a held bin reuses it (idempotent, no re-find)", func(t *testing.T) {
		f := newFakeStore()
		finder := foundFinder(88, "src-x") // must NOT be consulted — the bin is already held
		dispatcher := &recordingDispatcher{}
		s := newScannerWith(t, f, finder, dispatcher, nil)
		f.nodesByDot["dest-x"] = &nodes.Node{ID: 100, Name: "dest-x"}
		f.nodesByDot["src-held"] = &nodes.Node{ID: 101, Name: "src-held"}
		heldBin := int64(55)
		order := &orders.Order{
			ID: 43, Status: protocol.StatusSourcing, OrderType: protocol.OrderTypeRetrieve,
			BinID:      &heldBin, // already holds its bin — must be reused, never re-found
			SourceNode: "src-held", DeliveryNode: "dest-x", PayloadCode: "PN-X",
		}
		f.queued = append(f.queued, order)
		f.ordersByID[43] = order

		if got := s.RunOnce(); got != 1 {
			t.Fatalf("RunOnce: got %d, want 1 (held-bin sourcing order dispatches, no re-find)", got)
		}
		if len(finder.calls) != 0 {
			t.Errorf("a held-bin order must NOT consult the finder (length-1 idempotency): %+v", finder.calls)
		}
		if len(f.claimedBins) != 0 {
			t.Errorf("a held-bin order must not claim a second bin: %v", f.claimedBins)
		}
		if len(dispatcher.directCalls) != 1 || dispatcher.directCalls[0].orderID != 43 {
			t.Errorf("DispatchDirect calls = %+v, want one for order 43", dispatcher.directCalls)
		}
	})
}

// A7: the scanner's capacity gate self-excludes — it passes order.ID, not 0, so
// the in-flight tally (which counts `sourcing`) can't count the order's own row.
// With the widened set, simple sourcing orders are scoped out before the gate,
// so this pins the mechanism a QUEUED order threads; the reserve/confirm split
// makes it load-bearing once sourcing
// orders reach the gate.
func TestScannerCapacityGatePassesOrderID(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	order := seedQueuedRetrieve(f, 55, "dest-55") // concrete dest, 0 bins, 0 in-flight
	s := newTestScanner(t, f)
	s.finder = waitFinder(protocol.QueueWaitingForMaterial, dispatch.QueueParams{Payload: "PN-123"}) // stop after the gate

	if got := s.RunOnce(); got != 0 {
		t.Fatalf("RunOnce: got %d, want 0", got)
	}
	if len(f.capacityExcludeIDs) == 0 {
		t.Fatalf("capacity gate in-flight count was never called")
	}
	for _, id := range f.capacityExcludeIDs {
		if id != order.ID {
			t.Errorf("capacity gate excludeID = %d, want order.ID %d (A7: scanner must self-exclude)", id, order.ID)
		}
	}
}

// ── RunOnce coalescing + periodic sweep ─────────────────────────────

func TestScanner_RunOnce_TriggerDuringScan_RerunsOnce(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	s := newTestScanner(t, f)

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
	t.Parallel()
	f := newFakeStore()
	s := newTestScanner(t, f)

	var scanCount int32
	f.onListQueuedOrders = func() { atomic.AddInt32(&scanCount, 1) }

	s.StartPeriodicSweep(5 * time.Millisecond)
	testutil.Eventually(t, 2*time.Second, func() bool {
		return atomic.LoadInt32(&scanCount) >= 1
	})
	s.Stop()

	afterStop := atomic.LoadInt32(&scanCount)
	if afterStop == 0 {
		t.Fatalf("sweep should have fired at least once before Stop")
	}

	time.Sleep(40 * time.Millisecond) // negative assertion: verify no further sweeps
	final := atomic.LoadInt32(&scanCount)
	if final > afterStop+1 {
		t.Errorf("sweep ran %d extra times after Stop (%d → %d), want ≤ 1",
			final-afterStop, afterStop, final)
	}
}
