// wiring_completion.go — Order delivery and completion handling.
//
// handleOrderDelivered runs when fleet reports FINISHED. It sends the
// delivered notification and moves the bin(s) to their destination
// immediately so telemetry is accurate. handleOrderCompleted runs when
// Edge confirms receipt and advances compound orders. Both paths use
// applyBinArrivalForOrder / applyMultiBinArrivalForOrder for the bin
// move; the completion path is idempotent (skips bins already at dest).

package engine

import (
	"time"

	"shingo/protocol"
	"shingocore/store/orders"
)

// handleOrderDelivered runs on fleet-reported FINISHED. Notifies Edge
// and moves the bin to its destination immediately so subsequent orders
// see accurate occupancy.
func (e *Engine) handleOrderDelivered(order *orders.Order) {
	// Resolve staged expiry for the delivered message
	var stagedExpireAt *time.Time
	if order.DeliveryNode != "" {
		if destNode, err := e.db.GetNodeByDotName(order.DeliveryNode); err == nil {
			if ea := e.resolveStagingExpiry(destNode); ea != nil {
				stagedExpireAt = ea
			}
		}
	}

	if err := e.sendToEdge(protocol.TypeOrderDelivered, order.StationID, &protocol.OrderDelivered{
		OrderUUID:      order.EdgeUUID,
		DeliveredAt:    time.Now().UTC(),
		StagedExpireAt: stagedExpireAt,
	}); err != nil {
		e.logFn("engine: delivered notification: %v", err)
	}

	// Move the bin to its destination NOW — the robot has physically completed
	// the delivery. Waiting for Edge confirmation (the old path) left a window
	// where telemetry reported the bin at the source node, causing stale
	// occupancy checks and blocking subsequent orders.
	e.applyBinArrivalForOrder(order)
}

// applyBinArrivalForOrder moves the order's bin(s) to the delivery node.
// Called from handleOrderDelivered (on fleet FINISHED) so that telemetry
// is accurate immediately. handleOrderCompleted still runs on confirmation
// but is idempotent — it skips the bin move if already at the destination.
func (e *Engine) applyBinArrivalForOrder(order *orders.Order) {
	if order.SourceNode == "" || order.DeliveryNode == "" {
		return
	}

	// Multi-bin path
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		e.applyMultiBinArrivalForOrder(order, orderBins)
		return
	}

	// Single-bin path
	if order.BinID == nil {
		return
	}

	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for delivery arrival: %v", order.DeliveryNode, err)
		return
	}

	sourceNode, _ := e.db.GetNodeByDotName(order.SourceNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	staged, expiresAt := e.resolveNodeStaging(destNode)

	// Note: previously this path forced staged=false for complex orders with
	// WaitIndex > 0 and for retrieve_empty deliveries. Both overrides removed
	// 2026-04-14 — they bypassed the FindSourceBinFIFO staged exclusion and
	// allowed unloader/loader auto-requests to poach lineside bins. With the
	// overrides gone, lineside deliveries arrive `staged` and stay protected
	// until the next claim or operator action.

	if err := e.binService.ApplyArrival(*order.BinID, destNode.ID, staged, expiresAt); err != nil {
		e.logFn("engine: apply bin arrival on delivery for order %d bin %d: %v", order.ID, *order.BinID, err)
		return
	}

	bin, binErr := e.db.GetBin(*order.BinID)
	if binErr != nil {
		e.logFn("engine: get bin %d for delivery arrival event: %v", *order.BinID, binErr)
	}
	if bin != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  sourceNodeID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
		}})
	}
}

// applyMultiBinArrivalForOrder handles the multi-bin case at delivery time.
//
// Note: previously this path forced staged=false for complex orders with
// WaitIndex > 0 ("operatorConfirmed"). Override removed 2026-04-14 — bins
// arriving at lineside via complex orders now stage like simple orders do.
// See applyBinArrivalForOrder for full context.
func (e *Engine) applyMultiBinArrivalForOrder(order *orders.Order, orderBins []*orders.OrderBin) {
	var instructions []orders.BinArrivalInstruction
	// fromNodeIDs[i] is the source node of instructions[i]. Captured here
	// so the post-arrival BinUpdatedEvent can carry FromNodeID — without it
	// handleKanbanDemand cannot fire produce signals on storage-slot exit.
	var fromNodeIDs []int64

	for _, ob := range orderBins {
		if ob.DestNode == "" {
			continue
		}
		destNode, err := e.db.GetNodeByDotName(ob.DestNode)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found on delivery: %v", order.ID, ob.BinID, ob.DestNode, err)
			continue
		}
		staged, expiresAt := e.resolveNodeStaging(destNode)
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})

		// Resolve the per-bin source node (the OrderBin.NodeName is the dot-path
		// of the pickup step). 0 means "unknown source" — kanban will simply not
		// fire the FROM-side check, which is the correct degradation.
		fromNodeID := int64(0)
		if ob.NodeName != "" {
			if srcNode, err := e.db.GetNodeByDotName(ob.NodeName); err == nil && srcNode != nil {
				fromNodeID = srcNode.ID
			}
		}
		fromNodeIDs = append(fromNodeIDs, fromNodeID)
	}

	if len(instructions) == 0 {
		return
	}

	if err := e.db.ApplyMultiBinArrival(instructions); err != nil {
		e.logFn("engine: multi-bin delivery arrival for order %d: %v", order.ID, err)
		return
	}

	for i, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  fromNodeIDs[i],
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
		}})
	}
}

// handleOrderCompleted runs when Edge confirms receipt. Bin movement already
// happened in handleOrderDelivered, so this is mostly paperwork (compound
// order advancement, cleanup). The bin arrival call is kept as an idempotent
// safety net — if the bin is already at dest, ApplyBinArrival is a no-op.
func (e *Engine) handleOrderCompleted(ev OrderCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for completion: %v", ev.OrderID, err)
		return
	}

	// If this is a child of a compound order, advance the parent
	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	if order.SourceNode == "" || order.DeliveryNode == "" {
		return
	}

	// Check for multi-bin junction table rows (populated by claimComplexBins
	// for orders with 2+ pickup steps). If present, each bin has a per-step
	// destination — use the junction table path instead of the legacy single-bin path.
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		e.handleMultiBinCompleted(order, orderBins)
		return
	}

	// Legacy single-bin path: idempotent safety net — bin should already be at
	// dest from handleOrderDelivered, but re-apply in case delivery arrival failed.
	if order.BinID == nil {
		return
	}

	bin, err := e.db.GetBin(*order.BinID)
	if err != nil {
		e.logFn("engine: get bin %d for completion: %v", *order.BinID, err)
		return
	}
	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for completion: %v", order.DeliveryNode, err)
		return
	}
	sourceNode, _ := e.db.GetNodeByDotName(order.SourceNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	// Safety-net invariant: only re-apply this order's arrival if the bin is
	// STILL at source, meaning handleOrderDelivered's authoritative move never
	// ran. If the bin has moved at all — to dest (normal post-FINISH state),
	// or to a third node (re-claimed by a newer order during the FINISH →
	// CONFIRM window) — we have no business re-applying. The previous guard
	// only checked "bin at dest", so a re-claimed bin at a third node fell
	// through and got teleported back to this stale order's destination
	// (SMN_001 → SMN_002 plant-test failure 2026-04-27). The AutoConfirm
	// split for evac legs collapses the typical race window, but this guard
	// codes the actual safety-net invariant directly.
	if bin.NodeID == nil || sourceNode == nil || *bin.NodeID != sourceNode.ID {
		e.dbg("completion: bin %d not at source %s — skipping safety-net arrival", *order.BinID, order.SourceNode)
		return
	}

	// Bin still at source — apply arrival as recovery from a missed FINISH

	staged, expiresAt := e.resolveNodeStaging(destNode)

	// Note: see applyBinArrivalForOrder for the override-removal context.
	// Same overrides existed here in the safety-net path and were removed
	// for the same reason.

	if err := e.binService.ApplyArrival(*order.BinID, destNode.ID, staged, expiresAt); err != nil {
		e.logFn("engine: apply bin arrival for order %d bin %d: %v", order.ID, *order.BinID, err)
		return
	}

	// Emit bin contents changed
	updatedBin, binErr := e.db.GetBin(*order.BinID)
	if binErr != nil {
		e.logFn("engine: get bin %d for completion event: %v", *order.BinID, binErr)
	}
	if updatedBin != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       updatedBin.ID,
			PayloadCode: updatedBin.PayloadCode,
			FromNodeID:  sourceNodeID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
		}})
	}
}

// handleMultiBinCompleted processes completion for orders with multiple claimed bins.
// Each bin is moved to its per-step destination (from the order_bins junction table)
// in a single atomic transaction. Idempotent — skips bins already at their destination
// (normal case when applyMultiBinArrivalForOrder already ran at delivery time).
func (e *Engine) handleMultiBinCompleted(order *orders.Order, orderBins []*orders.OrderBin) {
	var instructions []orders.BinArrivalInstruction
	// fromNodeIDs[i] is the source node of instructions[i] — same purpose as
	// in applyMultiBinArrivalForOrder: keep FromNodeID intact so kanban can
	// fire on storage-slot exit when this safety-net path actually moves a bin.
	var fromNodeIDs []int64

	// Note: previously had an "operatorConfirmed" override forcing staged=false
	// for complex orders with WaitIndex > 0. Removed 2026-04-14 — see
	// applyBinArrivalForOrder for context.

	for _, ob := range orderBins {
		if ob.DestNode == "" {
			e.logFn("engine: order %d bin %d has no dest_node in order_bins — skipping", order.ID, ob.BinID)
			continue
		}
		destNode, err := e.db.GetNodeByDotName(ob.DestNode)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found: %v", order.ID, ob.BinID, ob.DestNode, err)
			continue
		}

		// Safety-net invariant: only re-apply this leg's arrival if the bin
		// is STILL at this leg's source. If the bin has moved at all (to dest,
		// or to a third node because a newer order re-claimed it during the
		// FINISH → CONFIRM window), skip — re-applying would clobber
		// legitimate state. The previous guard only checked "bin at dest" and
		// teleported re-claimed bins back. See single-bin path comment for the
		// SMN_001 / SMN_002 plant-test context.
		bin, err := e.db.GetBin(ob.BinID)
		if err != nil {
			e.logFn("engine: order %d bin %d get for safety-net guard: %v", order.ID, ob.BinID, err)
			continue
		}
		var legSourceNodeID int64
		if ob.NodeName != "" {
			if srcNode, srcErr := e.db.GetNodeByDotName(ob.NodeName); srcErr == nil && srcNode != nil {
				legSourceNodeID = srcNode.ID
			}
		}
		if bin.NodeID == nil || legSourceNodeID == 0 || *bin.NodeID != legSourceNodeID {
			e.dbg("multi-bin completion: bin %d not at source %s — skipping safety-net arrival", ob.BinID, ob.NodeName)
			continue
		}

		staged, expiresAt := e.resolveNodeStaging(destNode)
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})

		// Capture the per-bin source node before we move it so the post-arrival
		// event still has it. The OrderBin.NodeName is the pickup step's dot-path.
		fromNodeID := int64(0)
		if ob.NodeName != "" {
			if srcNode, err := e.db.GetNodeByDotName(ob.NodeName); err == nil && srcNode != nil {
				fromNodeID = srcNode.ID
			}
		}
		fromNodeIDs = append(fromNodeIDs, fromNodeID)
	}

	// Junction rows are deleted only when the order has reached a terminal
	// status (confirmed / failed / cancelled). The Stage 10 action map
	// fires fireCompleted on (X, delivered) AND on (delivered, confirmed),
	// so this handler runs twice per order. Deleting on the first
	// (delivered) call would lose the per-bin destination data the
	// sibling handleOrderDelivered path needs on the same status change;
	// keeping the rows alive until terminal lets every completion firing
	// take the multi-bin idempotent path consistently. The terminal
	// transition (handled by HandleOrderReceipt's MarkConfirmed) is the
	// natural cleanup point — by then no more re-runs of this handler
	// will fire for the order.
	if protocol.IsTerminal(order.Status) {
		defer e.db.DeleteOrderBins(order.ID)
	}

	if len(instructions) == 0 {
		e.dbg("multi-bin completion: order %d all bins already at dest — skipping arrival", order.ID)
		return
	}

	if err := e.db.ApplyMultiBinArrival(instructions); err != nil {
		e.logFn("engine: multi-bin arrival for order %d: %v", order.ID, err)
		return
	}

	// Emit BinUpdatedEvent only for bins that actually moved
	for i, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			e.logFn("engine: get bin %d for multi-bin event: %v", inst.BinID, err)
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  fromNodeIDs[i],
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
		}})
	}

	e.logFn("engine: order %d multi-bin completion: %d bins moved", order.ID, len(instructions))
}
