// wiring.go — Core event handler wiring and order lifecycle processing.
//
// This is the reactive heart of ShinGo Core. wireEventHandlers() subscribes
// to EventBus events and dispatches to handlers in this file.
//
// Layout:
//   sendToEdge                    – outbound envelope helper
//   wireEventHandlers             – all EventBus subscriptions in one place
//   handleVendorStatusChange      – fleet status → order status mapping,
//                                   waybill, staged notification, terminal dispatch
//   handleOrderDelivered/Completed – bin arrival, multi-bin, compound order advancement
//   handleOrderCompleted chain    – staged delivery → Order B → changeover release →
//                                   manual swap → produce ingest → normal replenishment
//   handleNodeOrderFailed         – changeover error marking
//   handleSequentialBackfill      – auto-create Order B on in_transit
//
// The completion chain (handleNodeOrderCompleted) uses early-return pattern:
// each handler returns true if it matched, false to fall through to the next.

package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store"
)

// ── Outbound messaging ──────────────────────────────────────────────

// sendToEdge builds a protocol envelope and enqueues it for dispatch to an edge station.
func (e *Engine) sendToEdge(msgType string, stationID string, payload any) error {
	coreAddr := protocol.Address{Role: protocol.RoleCore, Station: e.cfg.Messaging.StationID}
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	env, err := protocol.NewEnvelope(msgType, coreAddr, edgeAddr, payload)
	if err != nil {
		return fmt.Errorf("build %s: %w", msgType, err)
	}
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode %s: %w", msgType, err)
	}
	if err := e.db.EnqueueOutbox(e.cfg.Messaging.DispatchTopic, data, msgType, stationID); err != nil {
		e.logFn("engine: outbox enqueue %s to %s failed: %v", msgType, stationID, err)
		return fmt.Errorf("enqueue %s: %w", msgType, err)
	}
	return nil
}

// ── Event subscriptions ─────────────────────────────────────────────

func (e *Engine) wireEventHandlers() {
	// ── Dispatch tracking ───────────────────────────────────────────
	// When an order is dispatched, track it in the tracker
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderDispatchedEvent)
		if e.tracker == nil {
			return
		}
		// On redirect, the order may already have an old vendor order ID tracked.
		// Look up the order and untrack the old ID if it differs from the new one.
		if order, err := e.db.GetOrder(ev.OrderID); err == nil && order.VendorOrderID != "" && order.VendorOrderID != ev.VendorOrderID {
			e.tracker.Untrack(order.VendorOrderID)
			e.logFn("engine: untracked old vendor order %s for order %d (redirect)", order.VendorOrderID, ev.OrderID)
		}
		e.tracker.Track(ev.VendorOrderID)
		e.logFn("engine: tracking vendor order %s for order %d", ev.VendorOrderID, ev.OrderID)
	}, EventOrderDispatched)

	// ── Vendor status changes ───────────────────────────────────────
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderStatusChangedEvent)
		e.dbg("vendor status change: order=%d vendor=%s %s->%s robot=%s", ev.OrderID, ev.VendorOrderID, ev.OldStatus, ev.NewStatus, ev.RobotID)
		e.handleVendorStatusChange(ev)
	}, EventOrderStatusChanged)

	// Record mission telemetry on every vendor status change
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderStatusChangedEvent)
		e.recordMissionEvent(ev)
	}, EventOrderStatusChanged)

	// ── Order failure ───────────────────────────────────────────────
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderFailedEvent)
		e.logFn("engine: order %d failed: %s - %s", ev.OrderID, ev.ErrorCode, ev.Detail)
		e.db.AppendAudit("order", ev.OrderID, "failed", "", ev.Detail, "system")

		if order, err := e.db.GetOrder(ev.OrderID); err == nil {
			// If child of a compound order, handle parent failure
			if order.ParentOrderID != nil && e.dispatcher != nil {
				e.dispatcher.HandleChildOrderFailure(*order.ParentOrderID, ev.OrderID)
			}
			e.maybeCreateReturnOrder(order, "failed")
		}
	}, EventOrderFailed)

	// ── Order completion ────────────────────────────────────────────
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCompletedEvent)
		e.logFn("engine: order %d completed", ev.OrderID)
		e.db.AppendAudit("order", ev.OrderID, "completed", "", "", "system")
		e.handleOrderCompleted(ev)
	}, EventOrderCompleted)

	// ── Order cancellation ─────────────────────────────────────────
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCancelledEvent)
		e.logFn("engine: order %d cancelled: %s", ev.OrderID, ev.Reason)
		e.db.AppendAudit("order", ev.OrderID, "cancelled", "", ev.Reason, "system")

		// Notify ShinGo Edge so it can transition the order locally.
		// The dispatcher path (edge-initiated cancel) sends its own reply via
		// ReplySender.SendCancelled, but engine-initiated cancellations (web UI
		// terminate, fleet status change, recovery) go through this event handler.
		// The edge handler (HandleOrderCancelled) is idempotent — a duplicate
		// cancellation for an already-cancelled order is harmless.
		if ev.StationID != "" && ev.EdgeUUID != "" {
			if err := e.sendToEdge(protocol.TypeOrderCancelled, ev.StationID,
				&protocol.OrderCancelled{
					OrderUUID: ev.EdgeUUID,
					Reason:    ev.Reason,
				}); err != nil {
				e.logFn("engine: cancel notification to edge: %v", err)
			} else {
				e.dbg("cancel notification sent to edge: station=%s uuid=%s", ev.StationID, ev.EdgeUUID)
			}
		}

		// Skip auto-return for orders that were already delivered/confirmed.
		// The bin is at the destination, not at the pickup node.
		if ev.PreviousStatus == dispatch.StatusDelivered || ev.PreviousStatus == dispatch.StatusConfirmed {
			e.logFn("engine: order %d was %s before cancel, skipping auto-return (bin at destination)", ev.OrderID, ev.PreviousStatus)
		} else if order, err := e.db.GetOrder(ev.OrderID); err == nil {
			e.maybeCreateReturnOrder(order, "cancelled")
		}
	}, EventOrderCancelled)

	// ── Audit-only subscriptions ────────────────────────────────────
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderReceivedEvent)
		e.logFn("engine: order %d received from %s: %s %s -> %s", ev.OrderID, ev.StationID, ev.OrderType, ev.PayloadCode, ev.DeliveryNode)
		e.db.AppendAudit("order", ev.OrderID, "received", "", fmt.Sprintf("%s %s from %s", ev.OrderType, ev.PayloadCode, ev.StationID), "system")
	}, EventOrderReceived)

	// Bin contents changes: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BinUpdatedEvent)
		e.db.AppendAudit("bin", ev.BinID, ev.Action, "", fmt.Sprintf("payload=%s node=%d", ev.PayloadCode, ev.NodeID), "system")
	}, EventBinUpdated)

	// Node updates: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(NodeUpdatedEvent)
		e.db.AppendAudit("node", ev.NodeID, ev.Action, "", ev.NodeName, "system")
	}, EventNodeUpdated)

	// Corrections: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(CorrectionAppliedEvent)
		e.db.AppendAudit("correction", ev.CorrectionID, ev.CorrectionType, "", ev.Reason, ev.Actor)
	}, EventCorrectionApplied)

	// ── CMS transaction logging ────────────────────────────────────
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BinUpdatedEvent)
		if ev.Action == "moved" && ev.FromNodeID != 0 && ev.ToNodeID != 0 {
			e.RecordMovementTransactions(ev)
		}
	}, EventBinUpdated)

	// ── Fulfillment scanner triggers ────────────────────────────────
	triggerFulfillment := func(Event) {
		if e.fulfillment != nil {
			e.fulfillment.Trigger()
			go e.fulfillment.RunOnce()
		}
	}
	e.Events.SubscribeTypes(triggerFulfillment, EventBinUpdated)
	e.Events.SubscribeTypes(triggerFulfillment, EventOrderCompleted)
	e.Events.SubscribeTypes(triggerFulfillment, EventOrderCancelled)
	e.Events.SubscribeTypes(triggerFulfillment, EventOrderFailed)

	// ── Queued order audit ─────────────────────────────────────────
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderQueuedEvent)
		e.logFn("engine: order %d queued for payload %s", ev.OrderID, ev.PayloadCode)
		e.db.AppendAudit("order", ev.OrderID, "queued", "", fmt.Sprintf("payload=%s from %s", ev.PayloadCode, ev.StationID), "system")
	}, EventOrderQueued)

	// ── Kanban demand ──────────────────────────────────────────────
	// look up the demand registry and send a demand signal to Edge.
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BinUpdatedEvent)
		e.handleKanbanDemand(ev)
	}, EventBinUpdated)
}

// ── Vendor status change → order update pipeline ────────────────────

func (e *Engine) handleVendorStatusChange(ev OrderStatusChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for status change: %v", ev.OrderID, err)
		return
	}

	// Compute effective robot ID (handles Case D: preserves existing robot when event has empty robotID)
	effectiveRobotID := order.RobotID
	if ev.RobotID != "" {
		effectiveRobotID = ev.RobotID
	}

	// First robot assignment: send waybill only (no DB write here - Option C)
	if ev.RobotID != "" && order.RobotID == "" {
		if err := e.sendToEdge(protocol.TypeOrderWaybill, order.StationID, &protocol.OrderWaybill{
			OrderUUID: order.EdgeUUID,
			WaybillID: order.VendorOrderID,
			RobotID:   ev.RobotID,
		}); err != nil {
			e.logFn("engine: waybill: %v", err)
		}
	}

	newStatus := e.fleet.MapState(ev.NewStatus)
	if newStatus == order.Status {
		// Idempotent path: status unchanged, check if robot ID changed
		if effectiveRobotID != order.RobotID {
			if err := e.db.UpdateOrderRobotID(order.ID, effectiveRobotID); err != nil {
				e.logFn("engine: update order %d robot: %v", order.ID, err)
			}
		}
		return
	}

	if err := e.db.UpdateOrderStatus(order.ID, newStatus, fmt.Sprintf("fleet: %s -> %s", ev.OldStatus, ev.NewStatus)); err != nil {
		e.logFn("engine: update order %d status to %s: %v", order.ID, newStatus, err)
	}
	if err := e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, effectiveRobotID); err != nil {
		e.logFn("engine: update order %d vendor state: %v", order.ID, err)
	}

	// Send status update to ShinGo Edge
	if err := e.sendToEdge(protocol.TypeOrderUpdate, order.StationID, &protocol.OrderUpdate{
		OrderUUID: order.EdgeUUID,
		Status:    newStatus,
		Detail:    fmt.Sprintf("fleet state: %s", ev.NewStatus),
	}); err != nil {
		e.logFn("engine: status update: %v", err)
	}

	// Send dedicated staged notification when robot is dwelling
	if newStatus == dispatch.StatusStaged {
		if err := e.sendToEdge(protocol.TypeOrderStaged, order.StationID, &protocol.OrderStaged{
			OrderUUID: order.EdgeUUID,
			Detail:    "robot dwelling at staging node",
		}); err != nil {
			e.logFn("engine: staged notification: %v", err)
		}
	}

	// Non-terminal states are fully handled above — exit early.
	if !e.fleet.IsTerminalState(ev.NewStatus) {
		return
	}

	switch newStatus {
	case dispatch.StatusDelivered:
		e.handleOrderDelivered(order)
	case dispatch.StatusFailed:
		e.handleFleetOrderFailed(order)
	case dispatch.StatusCancelled:
		e.handleFleetOrderCancelled(order)
	}
}

func (e *Engine) handleFleetOrderFailed(order *store.Order) {
	if err := e.db.FailOrderAtomic(order.ID, "fleet order failed"); err != nil {
		e.logFn("engine: atomic fail order %d: %v", order.ID, err)
	}
	e.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
		ErrorCode: "fleet_failed",
		Detail:    "fleet order failed",
	}})
}

func (e *Engine) handleFleetOrderCancelled(order *store.Order) {
	// order.Status is the in-memory value loaded before UpdateOrderStatus ran,
	// so it reflects the status prior to the cancellation update.
	previousStatus := order.Status
	if err := e.db.CancelOrderAtomic(order.ID, "fleet order stopped"); err != nil {
		e.logFn("engine: atomic cancel order %d: %v", order.ID, err)
	}
	e.Events.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:        order.ID,
		EdgeUUID:       order.EdgeUUID,
		StationID:      order.StationID,
		Reason:         "fleet order stopped",
		PreviousStatus: previousStatus,
	}})
}

// ── Delivery & bin arrival ───────────────────────────────────────────

func (e *Engine) handleOrderDelivered(order *store.Order) {
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
func (e *Engine) applyBinArrivalForOrder(order *store.Order) {
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

	if order.OrderType == dispatch.OrderTypeComplex && order.WaitIndex > 0 {
		staged = false
		expiresAt = nil
	}
	if order.PayloadDesc == "retrieve_empty" {
		staged = false
		expiresAt = nil
	}

	if err := e.db.ApplyBinArrival(*order.BinID, destNode.ID, staged, expiresAt); err != nil {
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
func (e *Engine) applyMultiBinArrivalForOrder(order *store.Order, orderBins []*store.OrderBin) {
	var instructions []store.BinArrivalInstruction
	operatorConfirmed := order.OrderType == dispatch.OrderTypeComplex && order.WaitIndex > 0

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
		if operatorConfirmed {
			staged = false
			expiresAt = nil
		}
		instructions = append(instructions, store.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})
	}

	if len(instructions) == 0 {
		return
	}

	if err := e.db.ApplyMultiBinArrival(instructions); err != nil {
		e.logFn("engine: multi-bin delivery arrival for order %d: %v", order.ID, err)
		return
	}

	for _, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
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

	// Skip if bin is already at the destination (normal case after delivery arrival)
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
	if bin.NodeID != nil && *bin.NodeID == destNode.ID {
		e.dbg("completion: bin %d already at dest %s — skipping arrival", *order.BinID, order.DeliveryNode)
		return
	}

	// Bin not yet at destination — apply arrival as safety net
	sourceNode, _ := e.db.GetNodeByDotName(order.SourceNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	staged, expiresAt := e.resolveNodeStaging(destNode)

	if order.OrderType == dispatch.OrderTypeComplex && order.WaitIndex > 0 {
		staged = false
		expiresAt = nil
	}
	if order.PayloadDesc == "retrieve_empty" {
		staged = false
		expiresAt = nil
	}

	if err := e.db.ApplyBinArrival(*order.BinID, destNode.ID, staged, expiresAt); err != nil {
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
func (e *Engine) handleMultiBinCompleted(order *store.Order, orderBins []*store.OrderBin) {
	var instructions []store.BinArrivalInstruction

	operatorConfirmed := order.OrderType == dispatch.OrderTypeComplex && order.WaitIndex > 0

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

		// Idempotency: skip bins already at destination (moved by delivery arrival)
		bin, err := e.db.GetBin(ob.BinID)
		if err == nil && bin.NodeID != nil && *bin.NodeID == destNode.ID {
			e.dbg("multi-bin completion: bin %d already at dest %s — skipping", ob.BinID, ob.DestNode)
			continue
		}

		staged, expiresAt := e.resolveNodeStaging(destNode)
		if operatorConfirmed {
			staged = false
			expiresAt = nil
		}
		instructions = append(instructions, store.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})
	}

	// Clean up junction table rows regardless of whether bins needed moving
	defer e.db.DeleteOrderBins(order.ID)

	if len(instructions) == 0 {
		e.dbg("multi-bin completion: order %d all bins already at dest — skipping arrival", order.ID)
		return
	}

	if err := e.db.ApplyMultiBinArrival(instructions); err != nil {
		e.logFn("engine: multi-bin arrival for order %d: %v", order.ID, err)
		return
	}

	// Emit BinUpdatedEvent only for bins that actually moved
	for _, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			e.logFn("engine: get bin %d for multi-bin event: %v", inst.BinID, err)
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
		}})
	}

	e.logFn("engine: order %d multi-bin completion: %d bins moved", order.ID, len(instructions))
}

// resolveNodeStaging determines if a destination node should receive bins
// as "staged" (lineside nodes) or "available" (storage slots under LANEs).
func (e *Engine) resolveNodeStaging(destNode *store.Node) (staged bool, expiresAt *time.Time) {
	isStorageSlot := false
	if destNode.ParentID != nil {
		if parent, err := e.db.GetNode(*destNode.ParentID); err == nil && parent.NodeTypeCode == "LANE" {
			isStorageSlot = true
		}
	}
	if !isStorageSlot {
		expiresAt = e.resolveStagingExpiry(destNode)
	}
	return !isStorageSlot, expiresAt
}

// maybeCreateReturnOrder creates STORE orders to return bins to their origins
// when an in-flight order is cancelled or fails. Each bin is routed to the
// root parent of its pickup node so the group resolver can pick the best slot.
//
// For multi-bin orders (junction table populated), a separate return order is
// created for each bin. For single-bin orders, the legacy path creates one.
func (e *Engine) maybeCreateReturnOrder(order *store.Order, reason string) {
	// If the fleet never accepted the order (no vendor order ID), the bin
	// never left its origin — no return needed. This prevents spurious
	// auto_return orders when dispatch fails at the fleet API level.
	if order.VendorOrderID == "" {
		e.logFn("engine: order %d failed before fleet accepted it, skipping auto-return", order.ID)
		return
	}

	switch order.Status {
	case dispatch.StatusDispatched, dispatch.StatusInTransit, dispatch.StatusStaged,
		dispatch.StatusFailed, dispatch.StatusCancelled:
		// These are states where the bin may have left its origin
	default:
		return
	}

	// Don't create return orders for return orders (prevent infinite loops)
	if order.PayloadDesc == "auto_return" {
		e.logFn("engine: order %d is already a return order, skipping auto-return", order.ID)
		return
	}

	// Skip auto-return for complex orders when the bin position is uncertain.
	// ApplyBinArrival only fires on FINISHED, so on failure mid-transit the DB
	// still shows the original pickup node while the bin is physically wherever
	// the robot stopped. A return order with the wrong source just sits forever.
	//
	// Exception: StatusStaged means the robot reached a wait node and dropped
	// the bin — the DB knows the bin's actual position, so a return is safe.
	if order.OrderType == dispatch.OrderTypeComplex && order.Status != dispatch.StatusStaged {
		e.logFn("engine: order %d is complex (status=%s), skipping auto-return (bin position uncertain)", order.ID, order.Status)
		return
	}

	// Don't create return orders for compound/reshuffle children
	if order.ParentOrderID != nil {
		return
	}

	// Multi-bin path: check junction table first.
	// Each bin gets its own return order with SourceNode set to the bin's
	// original pickup node (ob.NodeName), not the order's global SourceNode.
	//
	// INVARIANT: bins are still at their original pickup positions from the
	// DB's perspective because ApplyBinArrival never fires on cancelled/failed
	// orders. If partial-completion tracking (per-block receipts) is added
	// later, this assumption breaks and bins may be at intermediate positions.
	// Revisit this function if that feature is implemented.
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		for _, ob := range orderBins {
			e.createSingleReturnOrder(order, ob.BinID, ob.NodeName, reason)
		}
		e.db.DeleteOrderBins(order.ID)
		return
	}

	// Legacy single-bin path
	if order.BinID == nil {
		return
	}
	if order.SourceNode == "" {
		e.logFn("engine: order %d has no source node, cannot create return order", order.ID)
		return
	}
	e.createSingleReturnOrder(order, *order.BinID, order.SourceNode, reason)
}

// createSingleReturnOrder creates one STORE order to return a specific bin
// from its current location (sourceNodeName) to the root parent of that node.
func (e *Engine) createSingleReturnOrder(order *store.Order, binID int64, sourceNodeName, reason string) {
	sourceNode, err := e.db.GetNodeByDotName(sourceNodeName)
	if err != nil {
		e.logFn("engine: resolve source node %q for return order: %v", sourceNodeName, err)
		return
	}

	rootNode, err := e.db.GetRootNode(sourceNode.ID)
	if err != nil {
		e.logFn("engine: resolve root node for %q: %v", sourceNodeName, err)
		return
	}

	returnOrder := &store.Order{
		StationID:    order.StationID,
		OrderType:    dispatch.OrderTypeStore,
		Status:       dispatch.StatusPending,
		SourceNode:   sourceNodeName, // bin is still at origin — ApplyBinArrival never fires on failed/cancelled orders
		DeliveryNode: rootNode.Name,
		BinID:        &binID,
		PayloadDesc:  "auto_return",
	}

	if err := e.db.CreateOrder(returnOrder); err != nil {
		e.logFn("engine: create return order for order %d bin %d: %v", order.ID, binID, err)
		return
	}

	// Claim the bin for the return order. The bin was already unclaimed by
	// UnclaimOrderBins on the cancel/fail path, so claimed_by IS NULL.
	if err := e.db.ClaimBin(binID, returnOrder.ID); err != nil {
		e.logFn("engine: claim bin %d for return order %d: %v", binID, returnOrder.ID, err)
	}

	e.logFn("engine: created return order %d (store %s to %s) for %s order %d bin %d",
		returnOrder.ID, sourceNodeName, rootNode.Name, reason, order.ID, binID)
	e.db.AppendAudit("order", returnOrder.ID, "auto_return", "",
		fmt.Sprintf("returning bin %d from %s order %d", binID, reason, order.ID), "system")

	e.Events.Emit(Event{Type: EventOrderReceived, Payload: OrderReceivedEvent{
		OrderID:      returnOrder.ID,
		StationID:    returnOrder.StationID,
		OrderType:    returnOrder.OrderType,
		DeliveryNode: returnOrder.DeliveryNode,
	}})
}

// handleKanbanDemand checks if a bin event at a storage node should trigger
// demand signals to Edge stations via the demand registry.
//
// Kanban triggers:
//   - Bin moved FROM a storage slot → supply decreased → signal "produce" stations to replenish
//   - Bin moved TO a storage slot   → supply increased → signal "consume" stations that material is available
func (e *Engine) handleKanbanDemand(ev BinUpdatedEvent) {
	if ev.PayloadCode == "" {
		return
	}

	// Only bin movements trigger kanban demand.
	if ev.Action != "moved" {
		return
	}

	// Bin left a storage slot → supply decreased → tell producers to replenish.
	if ev.FromNodeID != 0 && e.isStorageSlot(ev.FromNodeID) {
		e.sendDemandSignals(ev.PayloadCode, "produce",
			fmt.Sprintf("bin %d removed from storage (payload %s)", ev.BinID, ev.PayloadCode))
	}

	// Bin arrived at a storage slot → supply increased → tell consumers material is available.
	if ev.ToNodeID != 0 && e.isStorageSlot(ev.ToNodeID) {
		e.sendDemandSignals(ev.PayloadCode, "consume",
			fmt.Sprintf("bin %d arrived at storage (payload %s)", ev.BinID, ev.PayloadCode))
	}
}

// isStorageSlot returns true if the node is a storage slot (child of a LANE node).
func (e *Engine) isStorageSlot(nodeID int64) bool {
	node, err := e.db.GetNode(nodeID)
	if err != nil || node.ParentID == nil {
		return false
	}
	parent, err := e.db.GetNode(*node.ParentID)
	if err != nil {
		return false
	}
	return parent.NodeTypeCode == "LANE"
}

// sendDemandSignals looks up the demand registry for the given payload code and role,
// then sends a DemandSignal to each matching Edge station.
func (e *Engine) sendDemandSignals(payloadCode, role, reason string) {
	entries, err := e.db.LookupDemandRegistry(payloadCode)
	if err != nil {
		e.logFn("engine: kanban demand registry lookup for %s: %v", payloadCode, err)
		return
	}

	for _, entry := range entries {
		if entry.Role != role {
			continue
		}
		signal := &protocol.DemandSignal{
			CoreNodeName: entry.CoreNodeName,
			PayloadCode:  payloadCode,
			Role:         role,
			Reason:       reason,
		}
		if err := e.SendDataToEdge(protocol.SubjectDemandSignal, entry.StationID, signal); err != nil {
			e.logFn("engine: send demand signal to %s for %s: %v", entry.StationID, payloadCode, err)
		} else {
			e.dbg("kanban: sent demand signal to %s: node=%s payload=%s role=%s",
				entry.StationID, entry.CoreNodeName, payloadCode, role)
		}
	}
}

// resolveStagingExpiry computes the staging expiry time for a node.
// Returns nil if staging is permanent (ttl=0 or ttl=none).
func (e *Engine) resolveStagingExpiry(node *store.Node) *time.Time {
	ttlStr := ""

	// Check node's own property first
	ttlStr = e.db.GetNodeProperty(node.ID, "staging_ttl")

	// If not set, check parent (via effective properties)
	if ttlStr == "" && node.ParentID != nil {
		ttlStr = e.db.GetNodeProperty(*node.ParentID, "staging_ttl")
	}

	// Parse the TTL value
	if ttlStr == "0" || strings.EqualFold(ttlStr, "none") {
		return nil // permanent staging
	}

	var ttl time.Duration
	if ttlStr != "" {
		parsed, err := time.ParseDuration(ttlStr)
		if err != nil {
			e.logFn("engine: staging ttl parse error for node %d: %q: %v", node.ID, ttlStr, err)
		} else {
			ttl = parsed
		}
	}

	// Fall back to global config default
	if ttl == 0 {
		ttl = e.cfg.Staging.TTL
	}
	if ttl <= 0 {
		return nil
	}

	t := time.Now().Add(ttl)
	return &t
}

// recordMissionEvent captures a state transition with robot position snapshot for telemetry.
func (e *Engine) recordMissionEvent(ev OrderStatusChangedEvent) {
	me := &store.MissionEvent{
		OrderID:       ev.OrderID,
		VendorOrderID: ev.VendorOrderID,
		OldState:      ev.OldStatus,
		NewState:      ev.NewStatus,
		RobotID:       ev.RobotID,
		Detail:        ev.Detail,
		BlocksJSON:    "[]",
		ErrorsJSON:    "[]",
	}

	// Snapshot robot position from cache
	if ev.RobotID != "" {
		if rs, ok := e.GetCachedRobotStatus(ev.RobotID); ok {
			me.RobotX = &rs.X
			me.RobotY = &rs.Y
			me.RobotAngle = &rs.Angle
			me.RobotBattery = &rs.BatteryLevel
			me.RobotStation = rs.CurrentStation
		}
	}

	// Capture block states and errors from vendor snapshot
	if ev.Snapshot != nil {
		if len(ev.Snapshot.Blocks) > 0 {
			if data, err := json.Marshal(ev.Snapshot.Blocks); err == nil {
				me.BlocksJSON = string(data)
			}
		}
		if len(ev.Snapshot.Errors) > 0 {
			if data, err := json.Marshal(ev.Snapshot.Errors); err == nil {
				me.ErrorsJSON = string(data)
			}
		}
	}

	if err := e.db.InsertMissionEvent(me); err != nil {
		e.logFn("engine: record mission event: %v", err)
	}

	// On terminal state, write the mission summary
	if e.fleet.IsTerminalState(ev.NewStatus) {
		e.finalizeMissionTelemetry(ev)
	}
}

// finalizeMissionTelemetry writes the summary row when a mission reaches a terminal state.
func (e *Engine) finalizeMissionTelemetry(ev OrderStatusChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: finalize telemetry: get order %d: %v", ev.OrderID, err)
		return
	}

	now := time.Now().UTC()
	mt := &store.MissionTelemetry{
		OrderID:       ev.OrderID,
		VendorOrderID: ev.VendorOrderID,
		RobotID:       ev.RobotID,
		StationID:     order.StationID,
		OrderType:     order.OrderType,
		SourceNode:    order.SourceNode,
		DeliveryNode:  order.DeliveryNode,
		TerminalState: ev.NewStatus,
		CoreCreated:   &order.CreatedAt,
		CoreCompleted: &now,
		DurationMS:    now.Sub(order.CreatedAt).Milliseconds(),
		BlocksJSON:    "[]",
		ErrorsJSON:    "[]",
		WarningsJSON:  "[]",
		NoticesJSON:   "[]",
	}

	if ev.Snapshot != nil {
		if ev.Snapshot.CreateTime > 0 {
			t := time.UnixMilli(ev.Snapshot.CreateTime)
			mt.VendorCreated = &t
		}
		if ev.Snapshot.TerminalTime > 0 {
			t := time.UnixMilli(ev.Snapshot.TerminalTime)
			mt.VendorCompleted = &t
		}
		if mt.VendorCreated != nil && mt.VendorCompleted != nil {
			mt.VendorDurationMS = mt.VendorCompleted.Sub(*mt.VendorCreated).Milliseconds()
		}
		if data, err := json.Marshal(ev.Snapshot.Blocks); err == nil {
			mt.BlocksJSON = string(data)
		}
		if data, err := json.Marshal(ev.Snapshot.Errors); err == nil {
			mt.ErrorsJSON = string(data)
		}
		if data, err := json.Marshal(ev.Snapshot.Warnings); err == nil {
			mt.WarningsJSON = string(data)
		}
		if data, err := json.Marshal(ev.Snapshot.Notices); err == nil {
			mt.NoticesJSON = string(data)
		}
	}

	if err := e.db.UpsertMissionTelemetry(mt); err != nil {
		e.logFn("engine: finalize telemetry: %v", err)
	}
}
