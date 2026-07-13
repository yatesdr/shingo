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
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
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
	// terminalAt is when the order first entered a terminal state
	// (FINISHED/STOPPED/FAILED); zero until then. The driver's eviction
	// sweep (T2.3) deletes terminal orders older than a retention window.
	terminalAt time.Time
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
	seq      atomic.Int64               // monotonic order-ID source (F1: IDs are never reused, so eviction is safe)
	opts     Options
	clk      clock.Clock           // stamps terminalAt + (via driver) times transitions
	emitter  fleet.TrackerEmitter  // set by InitTracker
	resolver fleet.OrderIDResolver // set by InitTracker
	// driver holds the *Driver in sim builds. Typed `any` so the package still
	// compiles without the sim tag, where nothing assigns or reads it — which is
	// exactly why `unused` fires here on the default build. CI lints untagged, so
	// the suppression is load-bearing, not cosmetic.
	driver any //nolint:unused // set by NewDriverFromConfig / read by typedDriver, both sim-tagged (driver_lifecycle_sim.go)
}

// New creates a SimulatorBackend with the given options.
func New(opts ...Option) *SimulatorBackend {
	s := &SimulatorBackend{
		orders: make(map[string]*simulatedOrder),
		clk:    clock.Real(),
	}
	for _, o := range opts {
		o(&s.opts)
	}
	if s.opts.clk != nil {
		s.clk = s.opts.clk
	}
	return s
}

// isEvictableTerminal reports whether a vendor state is terminal for eviction
// purposes. Note this intentionally includes FAILED, unlike the public
// IsTerminalState (which the dispatch state machine keys on and which omits
// FAILED) — a failed sim order is just as dead and should be reaped.
func isEvictableTerminal(vendorState string) bool {
	switch vendorState {
	case "FINISHED", "STOPPED", "FAILED":
		return true
	}
	return false
}

// stampTerminalLocked records when an order first becomes terminal. Caller must
// hold s.mu. Idempotent — only the first terminal transition is stamped.
func (s *SimulatorBackend) stampTerminalLocked(o *simulatedOrder, newState string) {
	if isEvictableTerminal(newState) && o.terminalAt.IsZero() {
		o.terminalAt = s.clk.Now()
	}
}

// EvictTerminalBefore removes every order that entered a terminal state before
// the cutoff and returns the count removed. Called by the driver's tick (T2.3)
// so the in-memory order map doesn't grow without bound during long soaks.
// Safe only because IDs come from the monotonic counter (F1): a deleted ID can
// never be re-minted, so a future order can't collide with an evicted one.
func (s *SimulatorBackend) EvictTerminalBefore(cutoff time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.orderSeq[:0:0] // new backing array; don't alias the old slice
	evicted := 0
	for _, id := range s.orderSeq {
		o, ok := s.orders[id]
		if !ok {
			continue
		}
		if !o.terminalAt.IsZero() && o.terminalAt.Before(cutoff) {
			delete(s.orders, id)
			evicted++
			continue
		}
		kept = append(kept, id)
	}
	s.orderSeq = kept
	return evicted
}

// --- fleet.Backend implementation ---

// CreateOrder is the single fleet create primitive (fleet.Backend). It stores an
// order with the request's blocks; req.Complete selects the lifecycle — true =
// the order completes once its blocks finish (no-wait, single-shot, the former
// transport shape); false = staged, with blocks appended later via ReleaseOrder
// (the former staged shape). Models both so the docker race suite exercises the
// same lifecycle split real SEER sees.
func (s *SimulatorBackend) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.opts.failOnCreate {
		return fleet.TransportOrderResult{}, fmt.Errorf("simulator: injected create failure")
	}

	vendorID := req.OrderID
	if vendorID == "" {
		vendorID = fmt.Sprintf("sim-%d", s.seq.Add(1))
	}

	order := &simulatedOrder{
		vendorOrderID: vendorID,
		externalID:    req.ExternalID,
		state:         "CREATED",
		priority:      req.Priority,
		complete:      req.Complete,
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
		// Idempotent: the order already settled (FINISHED/STOPPED/FAILED) and was reaped
		// by the eviction sweep before this release arrived — typically a complex order
		// the downtime model FAILED mid-flight while Core's auto-release was in flight for
		// it. Its terminal status already reached Core (DriveState fired before eviction),
		// so the release is moot. No-op instead of erroring: a hard error cascades a
		// spurious fleet_failed that fails the order a SECOND time on the Edge — the source
		// of the "not found for release" noise. A real fleet tolerates an idempotent
		// release of a settled order.
		return nil
	}
	if isEvictableTerminal(order.state) {
		// Same moot case, order still in the map (not yet evicted): it already reached a
		// terminal state, so released blocks can't apply. No-op.
		return nil
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
	s.stampTerminalLocked(order, "STOPPED")
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
	return vendorState == "FINISHED" || vendorState == "STOPPED"
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
		return "faulted"
	case "STOPPED":
		return "cancelled"
	default:
		return "unknown"
	}
}

// Compile-time interface check: SimulatorBackend must satisfy fleet.TrackingBackend.
// fleet.DriverStarter is checked in driver_lifecycle_sim.go (sim-only).
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
