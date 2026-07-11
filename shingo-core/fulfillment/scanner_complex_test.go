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
func (s *stubDispatcher) ReserveStorageDropoff(*orders.Order) error { return nil }
func (s *stubDispatcher) PostFindHook()                             {}

func newTestScannerWithDispatcher(t *testing.T, f *fakeStore, d Dispatcher) *Scanner {
	t.Helper()
	// finder is nil: the complex branch dispatches via DispatchPreparedComplex
	// and returns before the source finder is consulted.
	return NewScanner(
		f, d, stubLifecycle{db: f}, nil, f,
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
	t.Parallel()
	f := newFakeStore()
	dispatcher := &stubDispatcher{}
	s := newTestScannerWithDispatcher(t, f, dispatcher)

	// Concrete delivery node, empty (zero bins, zero in-flight).
	dest := &nodes.Node{ID: 7, Name: "LINE_01"}
	f.nodesByDot["LINE_01"] = dest

	order := &orders.Order{
		ID:        42,
		Status:    protocol.StatusQueued,
		OrderType: protocol.OrderTypeComplex,
		// Production complex orders are stamped coordinated at intake; the Stage-3
		// discriminator (IsCoordinated) reads the Coordinated column, not StepsJSON.
		Coordinated:  true,
		StepsJSON:    `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"LINE_01"}]`,
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

// TestScanner_ComplexOrder_BypassesCapacityGate is the negative twin
// of TestScanner_ComplexOrder_DispatchedWhenCapacityGreen and the
// regression that pins the 2026-05 gate-removal: complex orders now
// dispatch even when the delivery node has bins sitting on it. The
// step planner already accounts for destination state during
// resolution (two-robot supply legs deliver to nodes their evac
// siblings are about to clear; press-index, single-robot swap, etc.
// follow the same pattern), so a blanket capacity gate at the
// scanner layer wrongly blocked them.
//
// Pre-fix this test asserted the opposite: capacity blocked → queued
// with queue_reason set. The test was rewritten when the gate was
// scoped to simple orders only. See scanner.go:181-190 for the
// invariant.
func TestScanner_ComplexOrder_BypassesCapacityGate(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	dispatcher := &stubDispatcher{}
	s := newTestScannerWithDispatcher(t, f, dispatcher)

	// Concrete delivery node OCCUPIED. A simple order would queue
	// here; a complex order must dispatch — the step planner owns
	// the choreography decision.
	dest := &nodes.Node{ID: 7, Name: "LINE_01"}
	f.nodesByDot["LINE_01"] = dest
	f.binsAtNode = map[int64]int{7: 1}

	order := &orders.Order{
		ID:        99,
		Status:    protocol.StatusQueued,
		OrderType: protocol.OrderTypeComplex,
		// Production complex orders are stamped coordinated at intake; the Stage-3
		// discriminator (IsCoordinated) reads the Coordinated column, not StepsJSON.
		Coordinated:  true,
		StepsJSON:    `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"LINE_01"}]`,
		DeliveryNode: "LINE_01",
		PayloadCode:  "PN-X",
	}
	f.queued = append(f.queued, order)
	f.ordersByID[99] = order

	got := s.RunOnce()

	if got != 1 {
		t.Errorf("RunOnce returned %d, want 1 (complex order dispatches even with bin at dest)", got)
	}
	if len(dispatcher.preparedCalls) != 1 || dispatcher.preparedCalls[0] != 99 {
		t.Errorf("DispatchPreparedComplex calls = %v, want [99]", dispatcher.preparedCalls)
	}
	// No queue_reason write — the scanner's gate doesn't apply to
	// complex orders so it has no opinion to record. Operators don't
	// see a "destination occupied" chip on a complex order that's
	// in flight.
	if len(f.queueReasons) != 0 {
		t.Errorf("queueReasons = %v, want none (complex orders bypass the scanner's capacity gate)", f.queueReasons)
	}
}
