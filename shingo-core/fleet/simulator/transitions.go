package simulator

import (
	"log"
)

// StateTransition records a single vendor state change for a simulated order.
type StateTransition struct {
	VendorOrderID string
	OldState      string
	NewState      string
	MappedStatus  string // the dispatch status this maps to
	// Deferred is true when the transition did NOT happen: the resolver could
	// not map the vendor id yet, so the state has not advanced and nothing was
	// emitted (Fix A). The driver (or the test) is expected to retry.
	Deferred bool
}

// DriveState transitions a simulated order to a new vendor state and returns
// the old state, the mapped dispatch status, and whether the transition was
// DEFERRED. A deferral (Fix A, sim↔RDS-poller parity, 2026-09-07) means the
// resolver could not map the vendor id to a Core order yet: the state has NOT
// advanced and nothing was emitted — the caller is expected to retry the same
// transition on a later tick, which is exactly what the real RDS poller does
// on the same miss (it keeps the old tracked state and retries next cycle,
// rds/poller.go). The old state is still returned, so the caller can see where
// the order remains; mappedStatus is empty because no status was produced for
// a transition that did not happen.
//
// If the simulator has been wired into an Engine via InitTracker, DriveState
// automatically resolves the vendor order ID and emits an OrderStatusChanged
// event through the engine's event pipeline. Resolution runs BEFORE the state
// commits — committing first made a resolver miss a permanently lost
// transition, the one drop path that left no trace — and the event is emitted
// OUTSIDE the write lock to prevent deadlocks: the EventBus calls subscribers
// synchronously, and any subscriber that reads simulator state (RLock) would
// deadlock if we emitted while holding the WLock.
//
// Returns empty strings if the order is not found.
func (s *SimulatorBackend) DriveState(vendorOrderID, newState string) (oldState, mappedStatus string, deferred bool) {
	s.mu.Lock()
	order, ok := s.orders[vendorOrderID]
	if !ok {
		s.mu.Unlock()
		return "", "", false
	}
	oldState = order.state
	if oldState == newState {
		// Not a transition — nothing to resolve, commit, emit or defer.
		s.mu.Unlock()
		return oldState, mapStateInternal(newState), false
	}
	emitter := s.emitter
	resolver := s.resolver
	if emitter == nil || resolver == nil {
		// Unwired backend (tests drive transitions manually and emit on the
		// bus themselves — the DriveFullLifecycle contract). Legacy behavior:
		// commit in place. There is no resolver that could miss, so nothing
		// can defer on this path.
		order.state = newState
		s.stampTerminalLocked(order, newState)
		s.mu.Unlock()
		return oldState, mapStateInternal(newState), false
	}
	s.mu.Unlock()

	orderID, err := resolver.ResolveVendorOrderID(vendorOrderID)
	if err != nil {
		// HOLD THE TRANSITION, DON'T DROP IT (Fix A). A state change the fleet
		// made and Core never heard about used to be committed here anyway —
		// indistinguishable downstream from a state change that never
		// happened. Now the state does not advance and the caller retries,
		// like the RDS poller does.
		log.Printf("simulator: deferred %s → %s for %s: no Core order resolves to it yet, will retry (%v)",
			oldState, newState, vendorOrderID, err)
		return oldState, "", true
	}

	// Re-validate before committing: the order may have been cancelled, moved
	// on or evicted while the lock was dropped to resolve. A stale attempt
	// must neither commit nor emit — driving a transition at a since-cancelled
	// order would resurrect it on Edge.
	s.mu.Lock()
	order, ok = s.orders[vendorOrderID]
	if !ok || order.state != oldState {
		current := "gone"
		if ok {
			current = order.state
		}
		s.mu.Unlock()
		log.Printf("simulator: voided %s → %s for %s: the order moved to %s while the attempt was in flight",
			oldState, newState, vendorOrderID, current)
		return oldState, "", false
	}
	order.state = newState
	mappedStatus = mapStateInternal(newState)
	s.stampTerminalLocked(order, newState)
	s.mu.Unlock()

	// Emit after the commit, outside the lock. The state is on record before
	// any subscriber runs, and the event never describes a transition the
	// vendor did not take.
	emitter.EmitOrderStatusChanged(orderID, vendorOrderID, oldState, newState, "", "", nil)
	return
}

// DriveStateWithRobot transitions a simulated order to a new vendor state with a
// robot ID, simulating the real fleet backend's behavior where every status event
// carries the vehicle identifier. The robot ID flows through effectiveRobotID in
// handleVendorStatusChange, exercising the robot persistence, preservation, and
// clobber-prevention paths that DriveState (which passes robotID=") cannot test.
//
// Deferral semantics are DriveState's (see there): on a resolver miss the
// state does not advance and the caller retries. The sim does not stash the
// robot ID across the retry — the caller keeps it in its own per-order
// progress and re-supplies it, exactly as the driver holds p.robotID across a
// fleet-full wait (G16).
//
// Returns empty strings if the order is not found.
func (s *SimulatorBackend) DriveStateWithRobot(vendorOrderID, newState, robotID string) (oldState, mappedStatus string, deferred bool) {
	s.mu.Lock()
	order, ok := s.orders[vendorOrderID]
	if !ok {
		s.mu.Unlock()
		return "", "", false
	}
	oldState = order.state
	if oldState == newState {
		// Not a transition — nothing to resolve, commit, emit or defer.
		s.mu.Unlock()
		return oldState, mapStateInternal(newState), false
	}
	emitter := s.emitter
	resolver := s.resolver
	if emitter == nil || resolver == nil {
		// Unwired backend: commit in place, same as DriveState's legacy arm.
		order.state = newState
		s.stampTerminalLocked(order, newState)
		s.mu.Unlock()
		return oldState, mapStateInternal(newState), false
	}
	s.mu.Unlock()

	orderID, err := resolver.ResolveVendorOrderID(vendorOrderID)
	if err != nil {
		// Same hold-don't-drop as DriveState: the state does not advance, the
		// caller retries. Retrying re-supplies the robot ID from the caller's
		// own progress bookkeeping.
		log.Printf("simulator: deferred %s → %s for %s (robot %s): no Core order resolves to it yet, will retry (%v)",
			oldState, newState, vendorOrderID, robotID, err)
		return oldState, "", true
	}

	// Re-validate: same supersede window as DriveState.
	s.mu.Lock()
	order, ok = s.orders[vendorOrderID]
	if !ok || order.state != oldState {
		current := "gone"
		if ok {
			current = order.state
		}
		s.mu.Unlock()
		log.Printf("simulator: voided %s → %s for %s (robot %s): the order moved to %s while the attempt was in flight",
			oldState, newState, vendorOrderID, robotID, current)
		return oldState, "", false
	}
	order.state = newState
	mappedStatus = mapStateInternal(newState)
	s.stampTerminalLocked(order, newState)
	s.mu.Unlock()

	// Emit after the commit, outside the lock — same discipline as DriveState:
	// the state is on record before any subscriber runs.
	emitter.EmitOrderStatusChanged(orderID, vendorOrderID, oldState, newState, robotID, "", nil)
	return
}

// DriveFullLifecycle advances a simulated order through the standard
// CREATED → RUNNING → WAITING → FINISHED lifecycle and returns the
// sequence of state transitions. Tests iterate over these and emit
// OrderStatusChanged events on the EventBus.
//
// A deferred step is recorded with Deferred=true and OldState/MappedStatus
// describing what did NOT happen, and the lifecycle moves on — this helper is
// a synchronous driver with no clock, so it cannot wait out a deferral; the
// caller reads Deferred and retries if its scenario needs to.
//
// Returns nil if the order is not found.
func (s *SimulatorBackend) DriveFullLifecycle(vendorOrderID string) []StateTransition {
	s.mu.RLock()
	_, ok := s.orders[vendorOrderID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	steps := []string{"RUNNING", "WAITING", "FINISHED"}
	var transitions []StateTransition
	for _, newState := range steps {
		oldState, mappedStatus, deferred := s.DriveState(vendorOrderID, newState)
		transitions = append(transitions, StateTransition{
			VendorOrderID: vendorOrderID,
			OldState:      oldState,
			NewState:      newState,
			MappedStatus:  mappedStatus,
			Deferred:      deferred,
		})
	}
	return transitions
}

// DriveSimpleLifecycle advances a simulated order through a simple
// CREATED → RUNNING → FINISHED lifecycle (no WAITING step). This matches
// the behavior of simple retrieve orders that go directly to completion.
//
// A deferred step is recorded and carried past, same as DriveFullLifecycle.
//
// Returns nil if the order is not found.
func (s *SimulatorBackend) DriveSimpleLifecycle(vendorOrderID string) []StateTransition {
	s.mu.RLock()
	_, ok := s.orders[vendorOrderID]
	s.mu.RUnlock()
	if !ok {
		return nil
	}

	steps := []string{"RUNNING", "FINISHED"}
	var transitions []StateTransition
	for _, newState := range steps {
		oldState, mappedStatus, deferred := s.DriveState(vendorOrderID, newState)
		transitions = append(transitions, StateTransition{
			VendorOrderID: vendorOrderID,
			OldState:      oldState,
			NewState:      newState,
			MappedStatus:  mappedStatus,
			Deferred:      deferred,
		})
	}
	return transitions
}

// DriveToFailed transitions a simulated order to the FAILED state.
// Useful for testing error handling and recovery paths.
func (s *SimulatorBackend) DriveToFailed(vendorOrderID string) (oldState, mappedStatus string, deferred bool) {
	return s.DriveState(vendorOrderID, "FAILED")
}

// DriveToStopped transitions a simulated order to the STOPPED state.
// Useful for testing cancellation scenarios.
func (s *SimulatorBackend) DriveToStopped(vendorOrderID string) (oldState, mappedStatus string, deferred bool) {
	return s.DriveState(vendorOrderID, "STOPPED")
}
