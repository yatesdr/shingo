package fulfillment

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// stubDispatcher records DispatchPreparedComplex calls and lets tests
// drive return values per-call. DispatchDirect is a panic-on-call stub
// since the complex-order branch never reaches it.
type stubDispatcher struct {
	preparedCalls []int64 // order IDs DispatchPreparedComplex was called with
	preparedErr   error   // returned from every DispatchPreparedComplex call
}

func (s *stubDispatcher) DispatchDirect(*orders.Order, *nodes.Node, *nodes.Node) (string, error) {
	panic("scanner complex-order branch should not call DispatchDirect")
}
func (s *stubDispatcher) DispatchPreparedComplex(o *orders.Order) error {
	s.preparedCalls = append(s.preparedCalls, o.ID)
	return s.preparedErr
}

func newTestScannerWithDispatcher(t *testing.T, db Store, d Dispatcher) *Scanner {
	t.Helper()
	return NewScanner(
		db, d, stubLifecycle{db: db}, nil,
		func(string, string, any) error { return nil },
		func(int64, string, string) {},
		t.Logf, nil,
	)
}

// TestScanner_ComplexOrder_DispatchedWhenCapacityGreen locks down the
// happy-path Phase 4 wiring: a queued complex order with an empty
// dropoff node hits the type-switch, calls
// dispatcher.DispatchPreparedComplex exactly once, and returns true.
func TestScanner_ComplexOrder_DispatchedWhenCapacityGreen(t *testing.T) {
	f := newFakeStore()
	dispatcher := &stubDispatcher{}
	s := newTestScannerWithDispatcher(t, f, dispatcher)

	// Concrete delivery node, empty (zero bins, zero in-flight).
	dest := &nodes.Node{ID: 7, Name: "LINE_01"}
	f.nodesByDot["LINE_01"] = dest

	order := &orders.Order{
		ID:           42,
		Status:       protocol.StatusQueued,
		OrderType:    protocol.OrderTypeComplex,
		DeliveryNode: "LINE_01",
		PayloadCode:  "PN-X",
	}
	f.queued = append(f.queued, order)
	f.ordersByID[42] = order

	got := s.RunOnce()

	if got != 1 {
		t.Errorf("RunOnce returned %d, want 1 (one complex order dispatched)", got)
	}
	if len(dispatcher.preparedCalls) != 1 || dispatcher.preparedCalls[0] != 42 {
		t.Errorf("DispatchPreparedComplex calls = %v, want [42]", dispatcher.preparedCalls)
	}
}

// TestScanner_ComplexOrder_QueuedWhenCapacityBlocked is the negative
// test for the same wiring: when CheckDropoffCapacity blocks, the
// scanner skips DispatchPreparedComplex entirely and writes
// queue_reason via SetOrderQueueReason. The order stays in the queue
// for the next replay.
//
// Per the regression-test rigor pillar this is the negative twin of
// the test above — predicate flips need both directions exercised.
func TestScanner_ComplexOrder_QueuedWhenCapacityBlocked(t *testing.T) {
	f := newFakeStore()
	dispatcher := &stubDispatcher{}
	s := newTestScannerWithDispatcher(t, f, dispatcher)

	// Concrete delivery node OCCUPIED — fakeStore.CountBinsByNode
	// returns the count from binCountsByNode.
	dest := &nodes.Node{ID: 7, Name: "LINE_01"}
	f.nodesByDot["LINE_01"] = dest
	f.binsAtNode = map[int64]int{7: 1}

	order := &orders.Order{
		ID:           99,
		Status:       protocol.StatusQueued,
		OrderType:    protocol.OrderTypeComplex,
		DeliveryNode: "LINE_01",
		PayloadCode:  "PN-X",
	}
	f.queued = append(f.queued, order)
	f.ordersByID[99] = order

	got := s.RunOnce()

	if got != 0 {
		t.Errorf("RunOnce returned %d, want 0 (capacity blocked, no dispatch)", got)
	}
	if len(dispatcher.preparedCalls) != 0 {
		t.Errorf("DispatchPreparedComplex calls = %v, want none (blocked)", dispatcher.preparedCalls)
	}
	// queue_reason should have been written.
	if len(f.queueReasons) != 1 {
		t.Fatalf("queueReasons = %v, want 1 entry", f.queueReasons)
	}
	got0 := f.queueReasons[0]
	if got0.OrderID != 99 {
		t.Errorf("queueReason order = %d, want 99", got0.OrderID)
	}
	if got0.Reason == "" {
		t.Errorf("queueReason reason is empty — operators rely on this to see WHY an order is queued")
	}
}

// TestScanner_ComplexOrder_QueueReasonNotRewrittenWhenSame avoids
// noise on the audit trail: if a queued order is re-evaluated and the
// reason hasn't changed (e.g., scanner periodic sweep), don't write
// the same reason again.
func TestScanner_ComplexOrder_QueueReasonNotRewrittenWhenSame(t *testing.T) {
	f := newFakeStore()
	dispatcher := &stubDispatcher{}
	s := newTestScannerWithDispatcher(t, f, dispatcher)

	dest := &nodes.Node{ID: 7, Name: "LINE_01"}
	f.nodesByDot["LINE_01"] = dest
	f.binsAtNode = map[int64]int{7: 1}

	const existing = "destination LINE_01 occupied (1 bin(s))"
	order := &orders.Order{
		ID:           101,
		Status:       protocol.StatusQueued,
		OrderType:    protocol.OrderTypeComplex,
		DeliveryNode: "LINE_01",
		PayloadCode:  "PN-X",
		QueueReason:  existing,
	}
	f.queued = append(f.queued, order)
	f.ordersByID[101] = order

	s.RunOnce()

	if len(f.queueReasons) != 0 {
		t.Errorf("queueReasons = %v, want no rewrites (reason unchanged)", f.queueReasons)
	}
}
