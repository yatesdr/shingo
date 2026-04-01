package testdb

import (
	"fmt"

	"shingocore/fleet"
)

// MockBackend is a configurable mock implementation of fleet.Backend.
// Use NewFailingBackend() for tests that expect fleet errors, or
// NewSuccessBackend() for tests that need successful fleet operations.
type MockBackend struct {
	fail   bool
	orders map[string]fleet.TransportOrderResult
}

// NewFailingBackend returns a MockBackend where all operations return errors.
func NewFailingBackend() *MockBackend {
	return &MockBackend{fail: true}
}

// NewSuccessBackend returns a MockBackend where all operations succeed
// and record created orders in the internal map.
func NewSuccessBackend() *MockBackend {
	return &MockBackend{orders: make(map[string]fleet.TransportOrderResult)}
}

// Orders returns a copy of the orders created via CreateTransportOrder or CreateStagedOrder.
func (m *MockBackend) Orders() map[string]fleet.TransportOrderResult {
	out := make(map[string]fleet.TransportOrderResult, len(m.orders))
	for k, v := range m.orders {
		out[k] = v
	}
	return out
}

func (m *MockBackend) CreateTransportOrder(req fleet.TransportOrderRequest) (fleet.TransportOrderResult, error) {
	if m.fail {
		return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
	}
	result := fleet.TransportOrderResult{VendorOrderID: req.OrderID}
	m.orders[req.OrderID] = result
	return result, nil
}

func (m *MockBackend) CancelOrder(vendorOrderID string) error {
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

func (m *MockBackend) CreateStagedOrder(req fleet.StagedOrderRequest) (fleet.TransportOrderResult, error) {
	if m.fail {
		return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
	}
	result := fleet.TransportOrderResult{VendorOrderID: req.OrderID}
	m.orders[req.OrderID] = result
	return result, nil
}

func (m *MockBackend) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock, complete bool) error {
	if m.fail {
		return fmt.Errorf("mock: not connected")
	}
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

func (m *MockTrackingBackend) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {}

func (m *MockTrackingBackend) Tracker() fleet.OrderTracker { return nil }
