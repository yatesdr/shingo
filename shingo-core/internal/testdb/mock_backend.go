package testdb

import (
	"fmt"

	"shingocore/fleet"
)

// MockBackend is a configurable mock implementation of fleet.Backend.
// Use NewFailingBackend() for tests that expect fleet errors, or
// NewSuccessBackend() for tests that need successful fleet operations.
type MockBackend struct {
	fail        bool
	orders      map[string]fleet.TransportOrderResult
	createReqs  []fleet.CreateOrderRequest
	cancelled   []string
	onCreate    func()
	releaseReqs []ReleaseCall
}

// ReleaseCall is one captured append against a live (unsealed) order — the
// /addBlocks half of the staged lifecycle. Complete records whether the append
// SEALED the order.
type ReleaseCall struct {
	VendorOrderID string
	Blocks        []fleet.OrderBlock
	Complete      bool
}

// CancelRequests returns the vendor order ids passed to CancelOrder, in call
// order. Needed to assert the orphan-mission guard actually cancels the fleet
// order it just created when the status write is refused (dispatcher.go).
func (m *MockBackend) CancelRequests() []string { return m.cancelled }

// SetFail flips the backend between refusing and accepting, mid-test.
//
// NewFailingBackend / NewSuccessBackend fix the answer at construction, which is
// enough for a test that asserts a refusal. It is not enough for one that
// asserts RECOVERY from a refusal — "the fleet was busy and then it was not" is
// a sequence, and a backend that can only hold one answer can only prove half of
// it. A refusal test that stops at the refusal is green while the demand is dead
// forever, which is the failure being fixed.
func (m *MockBackend) SetFail(fail bool) { m.fail = fail }

// SetOnCreate installs a hook that runs INSIDE CreateOrder, after the request
// is recorded and before it returns. The fleet call is where real time passes
// in a dispatch, so it is the only faithful place to inject a concurrent
// mutation — e.g. an operator cancel landing while the robot is being
// committed. Tests for the post-create orphan guard need exactly that window:
// the pre-dispatch status re-read must still see a live order, and the status
// write afterwards must not.
func (m *MockBackend) SetOnCreate(fn func()) { m.onCreate = fn }

// CreateRequests returns the CreateOrderRequests seen by CreateOrder, in call
// order. This is the unified capture (the single create primitive) and the one
// differential tests should assert on — it preserves the Complete value that
// distinguishes the no-wait (Complete=true) and staged (Complete=false) lifecycles.
func (m *MockBackend) CreateRequests() []fleet.CreateOrderRequest { return m.createReqs }

// ReleaseCalls returns the appends seen by ReleaseOrder, in call order. The
// create/append PAIR is what a staged lifecycle has to be asserted on: a lane-gate
// order that created unsealed but never appended is a robot dwelling forever, and
// only this capture can tell that apart from one whose tail went out.
func (m *MockBackend) ReleaseCalls() []ReleaseCall { return m.releaseReqs }

// NewFailingBackend returns a MockBackend where all operations return errors.
//
// It allocates the same recording map the success constructor does, so SetFail
// can turn it into a working backend mid-test. Without that, a failing backend
// was permanently failing by accident rather than by choice: CreateOrder's
// success arm writes to `orders`, and a nil map panics on write.
func NewFailingBackend() *MockBackend {
	return &MockBackend{fail: true, orders: make(map[string]fleet.TransportOrderResult)}
}

// NewSuccessBackend returns a MockBackend where all operations succeed
// and record created orders in the internal map.
func NewSuccessBackend() *MockBackend {
	return &MockBackend{orders: make(map[string]fleet.TransportOrderResult)}
}

// Orders returns a copy of the orders created via CreateOrder.
func (m *MockBackend) Orders() map[string]fleet.TransportOrderResult {
	out := make(map[string]fleet.TransportOrderResult, len(m.orders))
	for k, v := range m.orders {
		out[k] = v
	}
	return out
}

func (m *MockBackend) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	if m.fail {
		return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
	}
	result := fleet.TransportOrderResult{VendorOrderID: req.OrderID}
	m.orders[req.OrderID] = result
	m.createReqs = append(m.createReqs, req)
	if m.onCreate != nil {
		m.onCreate()
	}
	return result, nil
}

func (m *MockBackend) CancelOrder(vendorOrderID string) error {
	m.cancelled = append(m.cancelled, vendorOrderID)
	if m.fail {
		return fmt.Errorf("mock: not connected")
	}
	return nil
}

func (m *MockBackend) SetOrderPriority(vendorOrderID string, priority int) error {
	if m.fail {
		return fmt.Errorf("mock: not connected")
	}
	return nil
}

func (m *MockBackend) Ping() error {
	if m.fail {
		return fmt.Errorf("mock: not connected")
	}
	return nil
}

func (m *MockBackend) Name() string { return "mock" }

func (m *MockBackend) MapState(vendorState string) string { return "dispatched" }

func (m *MockBackend) IsTerminalState(vendorState string) bool { return false }

func (m *MockBackend) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock, complete bool) error {
	if m.fail {
		return fmt.Errorf("mock: not connected")
	}
	m.releaseReqs = append(m.releaseReqs, ReleaseCall{
		VendorOrderID: vendorOrderID,
		Blocks:        append([]fleet.OrderBlock(nil), blocks...),
		Complete:      complete,
	})
	return nil
}

func (m *MockBackend) Reconfigure(cfg fleet.ReconfigureParams) {}

// MockTrackingBackend wraps MockBackend and additionally satisfies
// the fleet.TrackingBackend interface (InitTracker + Tracker).
type MockTrackingBackend struct {
	*MockBackend
}

// NewTrackingBackend returns a MockTrackingBackend that succeeds on all
// fleet operations and satisfies fleet.TrackingBackend.
func NewTrackingBackend() *MockTrackingBackend {
	return &MockTrackingBackend{MockBackend: NewSuccessBackend()}
}

func (m *MockTrackingBackend) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {
}

func (m *MockTrackingBackend) Tracker() fleet.OrderTracker { return nil }
