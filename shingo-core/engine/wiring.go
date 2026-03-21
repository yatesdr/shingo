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

	// When an order fails, log it and handle compound orders
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderFailedEvent)
		e.logFn("engine: order %d failed: %s - %s", ev.OrderID, ev.ErrorCode, ev.Detail)
		e.db.AppendAudit("order", ev.OrderID, "failed", "", ev.Detail, "system")

		// If child of a compound order, handle parent failure
		if order, err := e.db.GetOrder(ev.OrderID); err == nil && order.ParentOrderID != nil && e.dispatcher != nil {
			e.dispatcher.HandleChildOrderFailure(*order.ParentOrderID, ev.OrderID)
		}
	}, EventOrderFailed)

	// When an order is completed, update inventory and audit
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCompletedEvent)
		e.logFn("engine: order %d completed", ev.OrderID)
		e.db.AppendAudit("order", ev.OrderID, "completed", "", "", "system")
		e.handleOrderCompleted(ev)
	}, EventOrderCompleted)

	// When an order is cancelled, audit it
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCancelledEvent)
		e.logFn("engine: order %d cancelled: %s", ev.OrderID, ev.Reason)
		e.db.AppendAudit("order", ev.OrderID, "cancelled", "", ev.Reason, "system")
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
			if err := e.db.UpdateOrderStatus(order.ID, dispatch.StatusFailed, "fleet order failed"); err != nil {
				e.logFn("engine: update order %d status to failed: %v", order.ID, err)
			}
			e.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
				OrderID:   order.ID,
				EdgeUUID:  order.EdgeUUID,
				StationID: order.StationID,
				ErrorCode: "fleet_failed",
				Detail:    "fleet order failed",
			}})
		case dispatch.StatusCancelled:
			if err := e.db.UpdateOrderStatus(order.ID, dispatch.StatusCancelled, "fleet order stopped"); err != nil {
				e.logFn("engine: update order %d status to cancelled: %v", order.ID, err)
			}
		}
	}
}

func (e *Engine) handleOrderDelivered(order *store.Order) {
	if err := e.db.UpdateOrderStatus(order.ID, dispatch.StatusDelivered, "payload delivered"); err != nil {
		e.logFn("engine: update order %d status to delivered: %v", order.ID, err)
	}

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

	if order.PickupNode == "" || order.DeliveryNode == "" {
		return
	}

	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for completion: %v", order.DeliveryNode, err)
		return
	}

	sourceNode, _ := e.db.GetNodeByDotName(order.PickupNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	// Bin-centric: move the bin and unclaim
	if order.BinID != nil {
		e.db.MoveBin(*order.BinID, destNode.ID)
		if err := e.db.UnclaimBin(*order.BinID); err != nil {
			e.logFn("engine: unclaim bin %d after delivery: %v", *order.BinID, err)
		}

		// Mark bin as staged at lineside nodes to prevent poaching.
		// Storage slots (children of LANEs) keep available status.
		isStorageSlot := false
		if destNode.ParentID != nil {
			if parent, err := e.db.GetNode(*destNode.ParentID); err == nil && parent.NodeTypeCode == "LANE" {
				isStorageSlot = true
			}
		}
		if !isStorageSlot {
			expiresAt := e.resolveStagingExpiry(destNode)
			if err := e.db.StageBin(*order.BinID, expiresAt); err != nil {
				e.logFn("engine: stage bin %d: %v", *order.BinID, err)
			}
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
		PickupNode:    order.PickupNode,
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
