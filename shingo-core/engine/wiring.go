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

func (e *Engine) wireEventHandlers() {
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

	// When the fleet reports a status change, update our order and notify ShinGo Edge
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

	// When an order fails, log it, handle compound orders, and auto-return bins
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

	// When an order is completed, update inventory and audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCompletedEvent)
		e.logFn("engine: order %d completed", ev.OrderID)
		e.db.AppendAudit("order", ev.OrderID, "completed", "", "", "system")
		e.handleOrderCompleted(ev)
	}, EventOrderCompleted)

	// When an order is cancelled, audit it, notify edge, and auto-return bins
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
		if ev.StationID != "" {
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

	// When an order is received, audit it
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

	// CMS transaction logging on bin movement
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BinUpdatedEvent)
		if ev.Action == "moved" && ev.FromNodeID != 0 && ev.ToNodeID != 0 {
			e.RecordMovementTransactions(ev)
		}
	}, EventBinUpdated)

	// Fulfillment scanner: trigger on events that may make bins available
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

	// Queued order: audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderQueuedEvent)
		e.logFn("engine: order %d queued for payload %s", ev.OrderID, ev.PayloadCode)
		e.db.AppendAudit("order", ev.OrderID, "queued", "", fmt.Sprintf("payload=%s from %s", ev.PayloadCode, ev.StationID), "system")
	}, EventOrderQueued)
}

func (e *Engine) handleVendorStatusChange(ev OrderStatusChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for status change: %v", ev.OrderID, err)
		return
	}

	// Update robot ID if we got one
	if ev.RobotID != "" && order.RobotID == "" {
		if err := e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, ev.RobotID); err != nil {
			e.logFn("engine: update order %d vendor (robot): %v", order.ID, err)
		}

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
		return
	}

	if err := e.db.UpdateOrderStatus(order.ID, newStatus, fmt.Sprintf("fleet: %s -> %s", ev.OldStatus, ev.NewStatus)); err != nil {
		e.logFn("engine: update order %d status to %s: %v", order.ID, newStatus, err)
	}
	if err := e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, ev.RobotID); err != nil {
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

	// Handle terminal states
	if e.fleet.IsTerminalState(ev.NewStatus) {
		switch newStatus {
		case dispatch.StatusDelivered:
			e.handleOrderDelivered(order)
		case dispatch.StatusFailed:
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
		case dispatch.StatusCancelled:
			previousStatus := order.Status // captured at top of function before status update
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
	}
}

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
}

// handleOrderCompleted moves payloads from source to dest after ShinGo Edge confirms physical receipt.
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

	// Legacy single-bin path: move one bin to Order.DeliveryNode.
	// Used by simple orders and single-pickup complex orders.
	if order.BinID == nil {
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

	staged, expiresAt := e.resolveNodeStaging(destNode)

	// Complex orders with operator-released waits: operator already confirmed.
	if order.OrderType == dispatch.OrderTypeComplex && order.WaitIndex > 0 {
		staged = false
		expiresAt = nil
	}

	if err := e.db.ApplyBinArrival(*order.BinID, destNode.ID, staged, expiresAt); err != nil {
		e.logFn("engine: apply bin arrival for order %d bin %d: %v", order.ID, *order.BinID, err)
		return
	}

	// Emit bin contents changed
	bin, binErr := e.db.GetBin(*order.BinID)
	if binErr != nil {
		e.logFn("engine: get bin %d for completion event: %v", *order.BinID, binErr)
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

// handleMultiBinCompleted processes completion for orders with multiple claimed bins.
// Each bin is moved to its per-step destination (from the order_bins junction table)
// in a single atomic transaction.
func (e *Engine) handleMultiBinCompleted(order *store.Order, orderBins []*store.OrderBin) {
	var instructions []store.BinArrivalInstruction

	// Complex orders with operator-released waits (WaitIndex > 0) have already
	// been confirmed by the operator. Don't stage bins — the "robot waiting for
	// operator" phase is over. This prevents bins from being permanently stuck
	// as staged after swap orders complete with no mechanism to unstage them.
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
		e.logFn("engine: order %d multi-bin completion: no valid instructions", order.ID)
		return
	}

	if err := e.db.ApplyMultiBinArrival(instructions); err != nil {
		e.logFn("engine: multi-bin arrival for order %d: %v", order.ID, err)
		return
	}

	// Emit BinUpdatedEvent for each bin
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

	// Clean up junction table rows after successful completion
	e.db.DeleteOrderBins(order.ID)

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