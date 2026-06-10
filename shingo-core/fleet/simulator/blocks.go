package simulator

// CompleteBlock emits an EmitBlockCompleted event for one block of an in-flight
// order (brief T2.2 / blocker S2). It mirrors what the real backend's poller
// emits when a per-block transition reaches FINISHED while the parent order is
// still mid-flight (see fleet/tracker.go): pickup blocks free the source slot
// to _TRANSIT, intermediate dropoffs record the bin at its slot immediately.
// The binTask is carried through verbatim so the engine's pickup/dropoff
// classifier (engine/wiring_block_completed.go) routes correctly.
//
// The driver (T2.3) calls this for each block whose simulated transit has
// elapsed. The FINAL delivery is represented by the order reaching FINISHED —
// the engine records it via handleOrderDelivered, and handleStoreBlockCompleted
// independently skips any dropoff whose location is the order's delivery node,
// so emitting the last block here would be a harmless no-op either way.
//
// Resolve + emit happen outside s.mu — the EventBus dispatches synchronously and
// a subscriber that reads simulator state would deadlock if we held the lock
// (same discipline as DriveState). Returns true if an event was emitted; false
// if the order, emitter, or resolver is absent (e.g. before InitTracker).
func (s *SimulatorBackend) CompleteBlock(vendorOrderID, blockID, location, binTask string) bool {
	s.mu.RLock()
	emitter := s.emitter
	resolver := s.resolver
	_, exists := s.orders[vendorOrderID]
	s.mu.RUnlock()

	if !exists || emitter == nil || resolver == nil {
		return false
	}
	orderID, err := resolver.ResolveVendorOrderID(vendorOrderID)
	if err != nil {
		return false
	}
	emitter.EmitBlockCompleted(orderID, vendorOrderID, blockID, location, binTask)
	return true
}
