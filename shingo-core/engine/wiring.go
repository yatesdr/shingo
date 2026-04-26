// wiring.go — Core event handler wiring.
//
// This is the reactive heart of ShinGo Core. wireEventHandlers() is the
// single master registry — every EventBus subscription lives here so
// the full reactive contract can be read top-to-bottom without cross-
// referencing other files. Handler implementations are split by
// functional concern into sibling files:
//
//   wiring_vendor_status.go   – fleet status → order status mapping,
//                                waybill/staged/terminal dispatch
//   wiring_completion.go      – delivery arrival, completion cleanup,
//                                multi-bin junction-table paths
//   wiring_staging.go         – resolveNodeStaging / resolveStagingExpiry
//   wiring_auto_return.go     – maybeCreateReturnOrder and related
//   wiring_kanban.go          – demand-registry signalling on bin moves
//   wiring_telemetry.go       – per-transition mission events + summary
//   wiring_count_group.go     – CountGroup broadcast to edges
//
// sendToEdge (the outbound envelope helper) also lives here since it
// is shared by the subscription handlers above.

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch"
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

		// Notify ShinGo Edge so it can transition the order locally.
		// Mirrors the EventOrderCancelled handler's notification block below.
		// The edge handler (HandleOrderError) is idempotent — duplicate
		// failure notifications for an already-failed order are harmless.
		// Auto-return orders have empty EdgeUUID by design (Core-internal);
		// the gate correctly skips them.
		if ev.StationID != "" && ev.EdgeUUID != "" {
			if err := e.sendToEdge(protocol.TypeOrderError, ev.StationID,
				&protocol.OrderError{
					OrderUUID: ev.EdgeUUID,
					ErrorCode: ev.ErrorCode,
					Detail:    ev.Detail,
				}); err != nil {
				e.logFn("engine: fail notification to edge: %v", err)
			} else {
				e.dbg("fail notification sent to edge: station=%s uuid=%s", ev.StationID, ev.EdgeUUID)
			}
		}

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
		if dispatch.IsPostDelivery(ev.PreviousStatus) {
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

	// ── Count-group transitions ────────────────────────────────────
	// When the countgroup runner detects a debounced occupancy change
	// (or fires the RDS-down fail-safe), ship a CountGroupCommand to
	// all edges. Each edge checks its own bindings map and either
	// drives the PLC tag or ignores.
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(CountGroupTransitionEvent)
		e.handleCountGroupTransition(ev)
	}, EventCountGroupTransition)
}
