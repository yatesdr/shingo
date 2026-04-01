// Package simulator provides an in-memory fleet.Backend for testing.
//
// The simulator stores every order and block the dispatcher sends it, so tests
// can inspect exactly what would have been sent to the real fleet vendor. Tests
// can also drive simulated state transitions (CREATED → RUNNING → FINISHED, etc.)
// to exercise the full order lifecycle without real robots or an RDS server.
//
// Usage:
//
//	sim := simulator.New()
//	d := dispatch.NewDispatcher(db, sim, emitter, "core", "dispatch", nil)
//	// ... submit orders, then inspect:
//	view := sim.GetOrderByIndex(0)
//	for _, b := range view.Blocks {
//	    assert.Equal(t, "JackLoad", b.BinTask)  // or JackUnload
//	}
package simulator

import (
	"fmt"
	"sync"

	"shingo/protocol"
	"shingocore/fleet"
)

// simulatedOrder is the in-memory representation of a fleet order.
type simulatedOrder struct {
	vendorOrderID string
	externalID    string
	state         string // vendor state: CREATED, RUNNING, WAITING, FINISHED, FAILED, STOPPED
	priority      int
	complete      bool // false for staged orders until ReleaseOrder
	blocks        []simulatedBlock
}

type simulatedBlock struct {
	blockID  string
	location string
	binTask  string
}

// SimulatorBackend implements fleet.TrackingBackend with in-memory order tracking.
// All orders, blocks, and bin tasks are stored in memory for test inspection.
// When wired into an Engine via InitTracker, DriveState emits events
// automatically through the event pipeline.
type SimulatorBackend struct {
	mu       sync.RWMutex
	orders   map[string]*simulatedOrder // vendorOrderID → order
	orderSeq []string                   // creation order
	opts     Options
	emitter  fleet.TrackerEmitter  // set by InitTracker
	resolver fleet.OrderIDResolver // set by InitTracker
}

// New creates a SimulatorBackend with the given options.
func New(opts ...Option) *SimulatorBackend {
	s := &SimulatorBackend{
		orders: make(map[string]*simulatedOrder),
	}
	for _, o := range opts {
		o(&s.opts)
	}
	return s
}

// --- fleet.Backend implementation ---

// CreateTransportOrder stores a complete two-block order (JackLoad at source,
// JackUnload at destination), matching the behavior of the real SEER RDS adapter.
func (s *SimulatorBackend) CreateTransportOrder(req fleet.TransportOrderRequest) (fleet.TransportOrderResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.opts.failOnCreate {
		return fleet.TransportOrderResult{}, fmt.Errorf("simulator: injected create failure")
	}

	vendorID := req.OrderID
	if vendorID == "" {
		vendorID = fmt.Sprintf("sim-%d", len(s.orders)+1)
	}

	order := &simulatedOrder{
		vendorOrderID: vendorID,
		externalID:    req.ExternalID,
		state:         "CREATED",
		priority:      req.Priority,
		complete:      true,
		blocks: []simulatedBlock{
			{blockID: vendorID + "_load", location: req.FromLoc, binTask: "JackLoad"},
			{blockID: vendorID + "_unload", location: req.ToLoc, binTask: "JackUnload"},
		},
	}
	s.orders[vendorID] = order
	s.orderSeq = append(s.orderSeq, vendorID)
	return fleet.TransportOrderResult{VendorOrderID: vendorID}, nil
}

// CreateStagedOrder stores an incomplete (staged) order with the blocks from
// the request. The order remains incomplete until ReleaseOrder is called.
func (s *SimulatorBackend) CreateStagedOrder(req fleet.StagedOrderRequest) (fleet.TransportOrderResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.opts.failOnCreate {
		return fleet.TransportOrderResult{}, fmt.Errorf("simulator: injected staged create failure")
	}

	vendorID := req.OrderID
	if vendorID == "" {
		vendorID = fmt.Sprintf("sim-%d", len(s.orders)+1)
	}

	order := &simulatedOrder{
		vendorOrderID: vendorID,
		externalID:    req.ExternalID,
		state:         "CREATED",
		priority:      req.Priority,
		complete:      false, // staged: not yet complete
	}
	for _, b := range req.Blocks {
		order.blocks = append(order.blocks, simulatedBlock{
			blockID:  b.BlockID,
			location: b.Location,
			binTask:  b.BinTask,
		})
	}
	s.orders[vendorID] = order
	s.orderSeq = append(s.orderSeq, vendorID)
	return fleet.TransportOrderResult{VendorOrderID: vendorID}, nil
}

// ReleaseOrder appends additional blocks to a staged order. When complete is
// true the order is marked finished; when false it stays staged so the robot
// can dwell at the next wait point (multi-wait complex orders).
func (s *SimulatorBackend) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock, complete bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	order, ok := s.orders[vendorOrderID]
	if !ok {
		return fmt.Errorf("simulator: order %s not found for release", vendorOrderID)
	}
	for _, b := range blocks {
		order.blocks = append(order.blocks, simulatedBlock{
			blockID:  b.BlockID,
			location: b.Location,
			binTask:  b.BinTask,
		})
	}
	order.complete = complete
	return nil
}

// CancelOrder sets the order state to STOPPED.
func (s *SimulatorBackend) CancelOrder(vendorOrderID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[vendorOrderID]
	if !ok {
		return fmt.Errorf("simulator: order %s not found", vendorOrderID)
	}
	order.state = "STOPPED"
	return nil
}

// SetOrderPriority updates the order's priority value.
func (s *SimulatorBackend) SetOrderPriority(vendorOrderID string, priority int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if order, ok := s.orders[vendorOrderID]; ok {
		order.priority = priority
		return nil
	}
	return fmt.Errorf("simulator: order %s not found", vendorOrderID)
}

// Ping returns nil unless WithPingFailure was set.
func (s *SimulatorBackend) Ping() error {
	if s.opts.failOnPing {
		return fmt.Errorf("simulator: ping failure injected")
	}
	return nil
}

// Name returns "Simulator".
func (s *SimulatorBackend) Name() string { return "Simulator" }

// MapState translates vendor states to dispatch status strings.
// This replicates the same mapping as the SEER RDS adapter's MapState.
func (s *SimulatorBackend) MapState(vendorState string) string {
	return mapStateInternal(vendorState)
}

// IsTerminalState returns true for FINISHED, FAILED, STOPPED.
func (s *SimulatorBackend) IsTerminalState(vendorState string) bool {
	return vendorState == "FINISHED" || vendorState == "FAILED" || vendorState == "STOPPED"
}

// Reconfigure is a no-op for the simulator.
func (s *SimulatorBackend) Reconfigure(_ fleet.ReconfigureParams) {}

// mapStateInternal is a pure function that translates vendor states to dispatch
// status strings. Extracted from MapState so that DriveState can call it while
// holding the write lock without risk of lock reentrancy.
func mapStateInternal(vendorState string) string {
	switch vendorState {
	case "CREATED", "TOBEDISPATCHED":
		return "dispatched"
	case "RUNNING":
		return "in_transit"
	case "WAITING":
		return "staged"
	case "FINISHED":
		return "delivered"
	case "FAILED":
		return "failed"
	case "STOPPED":
		return "cancelled"
	default:
		return "unknown"
	}
}

// Compile-time interface check: SimulatorBackend must satisfy fleet.TrackingBackend.
var _ fleet.TrackingBackend = (*SimulatorBackend)(nil)

// --- fleet.TrackingBackend implementation ---

// InitTracker stores the emitter and resolver so that DriveState can emit
// events automatically through the engine's event pipeline.
func (s *SimulatorBackend) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitter = emitter
	s.resolver = resolver
}

// Tracker returns a no-op OrderTracker. The simulator does not need a poller —
// DriveState emits events directly — but the engine calls Tracker().Start()
// and Tracker().Stop() during lifecycle, so we return a safe stub.
func (s *SimulatorBackend) Tracker() fleet.OrderTracker {
	return &simTracker{}
}

// simTracker is a no-op OrderTracker for the simulator. The simulator emits
// events through DriveState rather than a background poller, so tracking
// operations are intentional no-ops.
type simTracker struct{}

func (t *simTracker) Track(vendorOrderID string)   {}
func (t *simTracker) Untrack(vendorOrderID string) {}
func (t *simTracker) ActiveCount() int             { return 0 }
func (t *simTracker) Start()                       {}
func (t *simTracker) Stop()                        {}

// --- Convenience: expose protocol.Envelope for tests ---

// TestEnvelope creates a minimal protocol.Envelope suitable for test dispatch calls.
func TestEnvelope() *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "sim-edge"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
}