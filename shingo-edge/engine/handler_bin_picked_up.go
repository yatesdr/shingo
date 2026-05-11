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

import "shingoedge/domain"

// HandleBinPickedUp processes a Core BinPickedUp notification.
// Best-effort — failures log and continue rather than rejecting the
// envelope; the reconciler heals any miscount on the next pass.
func (e *Engine) HandleBinPickedUp(orderUUID string, binID int64) {
	order, err := e.db.GetOrderByUUID(orderUUID)
	if err != nil || order == nil {
		// Unknown order — Edge may have GC'd a terminal order, or the
		// envelope is for a different station. Log and move on.
		e.logFn("bin_picked_up: order uuid=%s not found", orderUUID)
		return
	}

	// F' Phase 2 — deferred-supply release on evac pickup confirm.
	//
	// When the picked-up order is the evac leg of a changeover node
	// task (matched via task.OldMaterialReleaseOrderID = order.ID),
	// release the paired supply leg (task.NextMaterialOrderID) now
	// that the evac robot has the old bin and is moving away from the
	// slot. The evac order itself is still RUNNING (it has dropoff
	// blocks remaining at outbound) — we trigger on the pickup block's
	// FINISHED transition mid-order, NOT on evac order completion.
	// The pickup-block-done moment is the only one that matters: it
	// means the slot is physically clear for the supply robot to come
	// in. ReleaseChangeoverWait deliberately defers the supply leg at
	// click time precisely so this auto-release closes the loop
	// deterministically — no slot-collision race.
	//
	// Scoped to changeover paths only via the task lookup. Operator-
	// station two-robot paths (operator_stations.go, operator_produce,
	// operator_bin_ops) use SiblingOrderID for their own supply↔evac
	// pairing and continue to rely on operator-click release; we don't
	// touch them here.
	//
	// Runs before the inventoryDelta-nil early-return so the chain
	// works whether or not the delta reporter is wired (the chain is
	// orthogonal to delta flushing). releaseUnlessTerminal is
	// idempotent against terminal supply orders.
	if task, terr := e.db.GetChangeoverNodeTaskByEvacOrderID(order.ID); terr == nil && task != nil {
		if task.NextMaterialOrderID != nil {
			supplyDisp := ReleaseDisposition{CalledBy: "auto-evac-pickup"}
			if rerr := e.releaseUnlessTerminal(*task.NextMaterialOrderID, "deferred-supply-after-evac-pickup", supplyDisp); rerr != nil {
				e.logFn("bin_picked_up: deferred-supply release order %d for evac %s: %v", *task.NextMaterialOrderID, orderUUID, rerr)
			}
		}
		// Drop tasks with the evacuate marker stay non-terminal until the
		// line is physically clear — the operator opted in to "wait for
		// this node to be evacuated before cutover." Pickup is that
		// moment; the bin's onward trip to the supermarket is just a
		// logistics move and doesn't gate cutover. Advance the task to
		// line_cleared (terminal for drop, per IsNodeTaskStateTerminal)
		// so the cutover guard and downstream rollups unblock immediately
		// rather than waiting for order completion at the destination.
		// Non-evac drops were stamped terminal at plan time, so the
		// !terminal guard makes this a no-op for them.
		if task.Situation == "drop" && !domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
			if err := e.db.UpdateChangeoverNodeTaskState(task.ID, "line_cleared"); err != nil {
				e.logFn("bin_picked_up: advance drop task %d to line_cleared: %v", task.ID, err)
			}
		}
	}

	if e.inventoryDelta == nil {
		// Nothing to flush; reporter not wired (test contexts).
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
			// Bin physically left the slot — clear the bin pointer so
			// PLC ticks during the gap before the next delivery don't
			// attribute to a bin that's no longer here. Symmetric with
			// the order-pointer clear above; same race guard applies.
			if err := e.db.SetProcessNodeActiveBinID(*order.ProcessNodeID, nil); err != nil {
				e.logFn("bin_picked_up: clear active bin node=%d: %v", *order.ProcessNodeID, err)
			}
		}
	}

	e.logFn("bin_picked_up: flushed deltas + cleared active for order=%s bin=%d (status=%s)",
		orderUUID, binID, order.Status)
}
