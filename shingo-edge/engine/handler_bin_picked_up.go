// handler_bin_picked_up.go — Edge handler for SubjectBinPickedUp.
//
// HandleBinPickedUp processes Core's notification. It
// fires when Core observes that a robot has physically picked up a
// bin from the source location (driven by the rds.Poller's per-block
// FINISHED transition through wiring_block_completed.go on Core).
//
// SEND PARTIAL BACK is the motivating flow:
//
//   1. Operator clicks RELEASE PARTIAL on a bin that still has UOP
//      remaining. Edge marks the order in_transit, sets the bin's
//      manifest to the partial count, and fires the order to the
//      fleet.
//   2. The cell keeps cycling — PLC ticks continue against the
//      released bin's slot until the robot arrives. Each tick
//      attributes to the released bin (ActiveOrderID still points
//      at the partial-back order, BinID still points at that bin).
//   3. Robot arrives, grabs the bin. Core's rds.Poller sees the
//      pickup-block FINISH and publishes BinPickedUp to Edge.
//   4. Edge flushes the inventory delta accumulator for the released
//      bin so any in-flight ticks ship before the active claim
//      advances. The runtime's ActiveOrderID is then cleared so
//      subsequent ticks attribute to whatever lands next.
//
// SME-accepted small bias: if Edge crashes during the pickup window,
// a tick or two recorded after the physical pickup but before the
// flush may attribute to a bin that's no longer at the slot. The
// reconciler heals the count within the next 60s pass.
package engine

// HandleBinPickedUp processes a Core BinPickedUp notification.
// Best-effort — failures log and continue rather than rejecting the
// envelope; the reconciler heals any miscount on the next pass.
func (e *Engine) HandleBinPickedUp(orderUUID string, binID int64) {
	if e.inventoryDelta == nil {
		// Nothing to flush; reporter not wired (test contexts).
		return
	}

	order, err := e.db.GetOrderByUUID(orderUUID)
	if err != nil || order == nil {
		// Unknown order — Edge may have GC'd a terminal order, or the
		// envelope is for a different station. Log and move on.
		e.logFn("bin_picked_up: order uuid=%s not found", orderUUID)
		return
	}

	// Flush the released-bin's accumulator: any deltas recorded
	// between RELEASE PARTIAL and BinPickedUp must ship before the
	// active claim advances or the runtime clears, otherwise
	// post-flush ticks attribute against a bin that's no longer at
	// the slot.
	e.inventoryDelta.Flush()

	// Advance: clear the runtime's ActiveOrderID for whichever node
	// the order was tied to so the next tick attribution lands cleanly
	// against the next claim. ProcessNodeID may be nil (pure-kanban
	// orders, generic moves); skip in that case.
	if order.ProcessNodeID != nil {
		runtime, err := e.db.GetProcessNodeRuntime(*order.ProcessNodeID)
		if err != nil || runtime == nil {
			return
		}
		// Only clear if the active order is still the one we just
		// picked up — guards against a race where the next bin's
		// delivery already advanced the slot.
		if runtime.ActiveOrderID != nil && *runtime.ActiveOrderID == order.ID {
			if err := e.db.UpdateProcessNodeRuntimeOrders(*order.ProcessNodeID, nil, runtime.StagedOrderID); err != nil {
				e.logFn("bin_picked_up: clear active order node=%d: %v", *order.ProcessNodeID, err)
			}
		}
	}

	e.logFn("bin_picked_up: flushed deltas + cleared active for order=%s bin=%d (status=%s)",
		orderUUID, binID, order.Status)
}
