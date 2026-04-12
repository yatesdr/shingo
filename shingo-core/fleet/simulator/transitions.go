package simulator

import "shingocore/fleet"

// StateTransition records a single vendor state change for a simulated order.
type StateTransition struct {
	VendorOrderID string
	OldState      string
	NewState      string
	MappedStatus  string // the dispatch status this maps to
}

// DriveState transitions a simulated order to a new vendor state and returns
// the old state and the mapped dispatch status.
//
// If the simulator has been wired into an Engine via InitTracker, DriveState
// automatically resolves the vendor order ID and emits an OrderStatusChanged
// event through the engine's event pipeline. The event is emitted OUTSIDE the
// write lock to prevent deadlocks — the EventBus calls subscribers
// synchronously, and any subscriber that reads simulator state (RLock) would
// deadlock if we emitted while holding the WLock.
//
// Returns empty strings if the order is not found.
func (s *SimulatorBackend) DriveState(vendorOrderID, newState string) (oldState, mappedStatus string) {
	var emitter fleet.TrackerEmitter
	var resolver fleet.OrderIDResolver

	s.mu.Lock()
	order, ok := s.orders[vendorOrderID]
	if !ok {
		s.mu.Unlock()
		return "", ""
	}
	oldState = order.state
	order.state = newState
	mappedStatus = mapStateInternal(newState)
	emitter = s.emitter
	resolver = s.resolver
	s.mu.Unlock()

	// Emit outside the lock. Only emit on actual state changes.
	if emitter != nil && resolver != nil && oldState != newState {
		if orderID, err := resolver.ResolveVendorOrderID(vendorOrderID); err == nil {
			emitter.EmitOrderStatusChanged(orderID, vendorOrderID, oldState, newState, "", "", nil)
		}
	}

	return
}



// DriveStateWithRobot transitions a simulated order to a new vendor state with a
// robot ID, simulating the real fleet backend's behavior where every status event
// carries the vehicle identifier. The robot ID flows through effectiveRobotID in
// handleVendorStatusChange, exercising the robot persistence, preservation, and
// clobber-prevention paths that DriveState (which passes robotID=") cannot test.
//
// Returns empty strings if the order is not found.
func (s *SimulatorBackend) DriveStateWithRobot(vendorOrderID, newState, robotID string) (oldState, mappedStatus string) {
	var emitter fleet.TrackerEmitter
	var resolver fleet.OrderIDResolver

	s.mu.Lock()
	order, ok := s.orders[vendorOrderID]
	if !ok {
		s.mu.Unlock()
		return "", ""
	}
	oldState = order.state
	order.state = newState
	mappedStatus = mapStateInternal(newState)
	emitter = s.emitter
	resolver = s.resolver
	s.mu.Unlock()

	// Emit outside the lock. Only emit on actual state changes.
	if emitter != nil && resolver != nil && oldState != newState {
		if orderID, err := resolver.ResolveVendorOrderID(vendorOrderID); err == nil {
			emitter.EmitOrderStatusChanged(orderID, vendorOrderID, oldState, newState, robotID, "", nil)
		}
	}

	return
}
// DriveFullLifecycle advances a simulated order through the standard
// CREATED → RUNNING → WAITING → FINISHED lifecycle and returns the
// sequence of state transitions. Tests iterate over these and emit
// OrderStatusChanged events on the EventBus.
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
		oldState, mappedStatus := s.DriveState(vendorOrderID, newState)
		transitions = append(transitions, StateTransition{
			VendorOrderID: vendorOrderID,
			OldState:      oldState,
			NewState:      newState,
			MappedStatus:  mappedStatus,
		})
	}
	return transitions
}

// DriveSimpleLifecycle advances a simulated order through a simple
// CREATED → RUNNING → FINISHED lifecycle (no WAITING step). This matches
// the behavior of simple retrieve orders that go directly to completion.
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
		oldState, mappedStatus := s.DriveState(vendorOrderID, newState)
		transitions = append(transitions, StateTransition{
			VendorOrderID: vendorOrderID,
			OldState:      oldState,
			NewState:      newState,
			MappedStatus:  mappedStatus,
		})
	}
	return transitions
}

// DriveToFailed transitions a simulated order to the FAILED state.
// Useful for testing error handling and recovery paths.
func (s *SimulatorBackend) DriveToFailed(vendorOrderID string) (oldState, mappedStatus string) {
	return s.DriveState(vendorOrderID, "FAILED")
}

// DriveToStopped transitions a simulated order to the STOPPED state.
// Useful for testing cancellation scenarios.
func (s *SimulatorBackend) DriveToStopped(vendorOrderID string) (oldState, mappedStatus string) {
	return s.DriveState(vendorOrderID, "STOPPED")
}