// wiring.go â€” Core event handler wiring.
//
// This is the reactive heart of ShinGo Core. wireEventHandlers() is the
// single master registry â€” every EventBus subscription lives here so
// the full reactive contract can be read top-to-bottom without cross-
// referencing other files. Handler implementations are split by
// functional concern into sibling files:
//
//   wiring_vendor_status.go   â€“ fleet status â†’ order status mapping,
//                                waybill/staged/terminal dispatch
//   wiring_completion.go      â€“ delivery arrival, completion cleanup,
//                                multi-bin junction-table paths
//   wiring_staging.go         â€“ resolveNodeStaging / resolveStagingExpiry
//   wiring_auto_return.go     â€“ maybeCreateReturnOrder and related
//   wiring_kanban.go          â€“ demand-registry signalling on bin moves
//   wiring_telemetry.go       â€“ per-transition mission events + summary
//   wiring_count_group.go     â€“ CountGroup broadcast to edges
//
// sendToEdge (the outbound envelope helper) also lives here since it
// is shared by the subscription handlers above.

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch"
)

// â”€â”€ Outbound messaging â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

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

// â”€â”€ Event subscriptions â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func (e *Engine) wireEventHandlers() {
	// â”€â”€ Dispatch tracking â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

	// â”€â”€ Vendor status changes â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

	// â”€â”€ Order failure â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderFailedEvent)
		e.logFn("engine: order %d failed: %s - %s", ev.OrderID, ev.ErrorCode, ev.Detail)
		e.db.AppendAudit("order", ev.OrderID, "failed", "", ev.Detail, "system")

		// Notify ShinGo Edge so it can transition the order locally.
		// Mirrors the EventOrderCancelled handler's notification block below.
		// The edge handler (HandleOrderError) is idempotent â€” duplicate
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

	// ── Order skipped ────────────────────────────────────────────────────
	// Mirrors the failure handler above but for the "work was never needed"
	// terminal. No return order, no anomaly audit — the operator-facing
	// surface treats this as a clean no-op. Edge advances the linked
	// changeover node task via HandleOrderSkipped.
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderSkippedEvent)
		e.logFn("engine: order %d skipped: %s - %s", ev.OrderID, ev.ErrorCode, ev.Detail)
		e.db.AppendAudit("order", ev.OrderID, "skipped", "", ev.Detail, "system")

		if ev.StationID != "" && ev.EdgeUUID != "" {
			if err := e.sendToEdge(protocol.TypeOrderSkipped, ev.StationID,
				&protocol.OrderSkipped{
					OrderUUID: ev.EdgeUUID,
					ErrorCode: ev.ErrorCode,
					Detail:    ev.Detail,
				}); err != nil {
				e.logFn("engine: skip notification to edge: %v", err)
			} else {
				e.dbg("skip notification sent to edge: station=%s uuid=%s", ev.StationID, ev.EdgeUUID)
			}
		}
	}, EventOrderSkipped)

	// â”€â”€ Order completion â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCompletedEvent)
		e.logFn("engine: order %d completed", ev.OrderID)
		e.db.AppendAudit("order", ev.OrderID, "completed", "", "", "system")
		e.handleOrderCompleted(ev)
	}, EventOrderCompleted)

	// â”€â”€ Order cancellation â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderCancelledEvent)
		e.logFn("engine: order %d cancelled: %s", ev.OrderID, ev.Reason)
		e.db.AppendAudit("order", ev.OrderID, "cancelled", "", ev.Reason, "system")

		// Notify ShinGo Edge so it can transition the order locally.
		// The dispatcher path (edge-initiated cancel) sends its own reply via
		// ReplySender.SendCancelled, but engine-initiated cancellations (web UI
		// terminate, fleet status change, recovery) go through this event handler.
		// The edge handler (HandleOrderCancelled) is idempotent â€” a duplicate
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
		if dispatch.IsPostDelivery(protocol.Status(ev.PreviousStatus)) {
			e.logFn("engine: order %d was %s before cancel, skipping auto-return (bin at destination)", ev.OrderID, ev.PreviousStatus)
		} else if order, err := e.db.GetOrder(ev.OrderID); err == nil {
			e.maybeCreateReturnOrder(order, "cancelled")
		}
	}, EventOrderCancelled)

	// â”€â”€ Audit-only subscriptions â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
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

	// â”€â”€ CMS transaction logging â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BinUpdatedEvent)
		if ev.Action == "moved" && ev.FromNodeID != 0 && ev.ToNodeID != 0 {
			e.RecordMovementTransactions(ev)
		}
	}, EventBinUpdated)

	// â”€â”€ Fulfillment scanner triggers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// Async trigger for high-volume signals (bin moves, order
	// completions). The scanner coalesces overlapping triggers via
	// its `pending` flag; a goroutine here keeps the emitting handler
	// chain non-blocking.
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
	// EventBinEnteredTransit is the slot-vacancy signal added in Phase 1
	// of the bin-transit-state project â€” every pickup that moves a bin
	// to _TRANSIT frees its source slot, which can unblock queued orders
	// that needed to drop something there. Subscribing here makes the
	// scanner re-evaluate without waiting for the order to fully complete.
	e.Events.SubscribeTypes(triggerFulfillment, EventBinEnteredTransit)

	// Sync trigger for fresh-intake (Phase 4b): EventOrderQueued.
	// HandleComplexOrderRequest creates new complex orders as queued and
	// fires this event; the scanner is the single sync point that calls
	// DispatchPreparedComplex, so capacity decisions are serialized via
	// scan-mu (no TOCTOU between two concurrent fresh intakes for the
	// same dropoff). Synchronous so the dispatched-status transition is
	// observable on return from HandleComplexOrderRequest â€” the existing
	// test fixtures rely on that ordering, and operator-facing latency
	// expectations don't tolerate "queued for ~1ms while a goroutine
	// gets scheduled."
	e.Events.SubscribeTypes(func(Event) {
		if e.fulfillment != nil {
			e.fulfillment.RunOnce()
		}
	}, EventOrderQueued)

	// â”€â”€ Per-block completion â†’ transit transition â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// Phase 2 of the bin-transit-state project: pickup blocks (BinTask=Load
	// or "pickup"-flavoured operations) drive the bin claimed at that step
	// onto the synthetic _TRANSIT node. The poller diffs per-block state
	// and fires EventBlockCompleted on the transition into FINISHED; this
	// handler routes by block kind.
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BlockCompletedEvent)
		e.handleBlockCompleted(ev)
	}, EventBlockCompleted)

	// â”€â”€ Queued order audit â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(OrderQueuedEvent)
		e.logFn("engine: order %d queued for payload %s", ev.OrderID, ev.PayloadCode)
		e.db.AppendAudit("order", ev.OrderID, "queued", "", fmt.Sprintf("payload=%s from %s", ev.PayloadCode, ev.StationID), "system")
	}, EventOrderQueued)

	// â”€â”€ Kanban demand â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// look up the demand registry and send a demand signal to Edge.
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(BinUpdatedEvent)
		e.handleKanbanDemand(ev)
	}, EventBinUpdated)

	// ── UOP-threshold replenishment monitor ─────────────────────────────
	// Combined bin + bucket UOP per payload — fires LoopBelowThresholdSignal
	// when a monitored (loader, payload) drops below its configured
	// threshold. Bucket-apply events go through OnBucketApplied from the
	// messaging layer; bin updates land via this subscription so cell-side
	// consume ticks and loader-side bin moves both re-evaluate.
	if e.thresholdMonitor != nil {
		e.Events.SubscribeTypes(func(evt Event) {
			ev := evt.Payload.(BinUpdatedEvent)
			e.thresholdMonitor.handleBinUpdated(ev)
		}, EventBinUpdated)
	}

	// â”€â”€ Count-group transitions â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	// When the countgroup runner detects a debounced occupancy change
	// (or fires the RDS-down fail-safe), ship a CountGroupCommand to
	// all edges. Each edge checks its own bindings map and either
	// drives the PLC tag or ignores.
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(CountGroupTransitionEvent)
		e.handleCountGroupTransition(ev)
	}, EventCountGroupTransition)
	// Grace-expiry: poller detected a faulted order whose grace period expired
	// without fleet recovery. Best-effort cancel at RDS, then local fail.
	e.Events.SubscribeTypes(func(evt Event) {
		ev := evt.Payload.(GraceExpiredEvent)
		e.handleGraceExpired(ev)
	}, EventGraceExpired)
}
