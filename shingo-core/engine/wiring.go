// wiring.go â€" Core event handler wiring.
//
// This is the reactive heart of ShinGo Core. wireEventHandlers() is the
// single master registry â€" every EventBus subscription lives here so
// the full reactive contract can be read top-to-bottom without cross-
// referencing other files. Handler implementations are split by
// functional concern into sibling files:
//
//   wiring_vendor_status.go   â€" fleet status â†’ order status mapping,
//                                waybill/staged/terminal dispatch
//   wiring_completion.go      â€" delivery arrival, completion cleanup,
//                                multi-bin junction-table paths
//   wiring_staging.go         â€" resolveNodeStaging / resolveStagingExpiry
//   wiring_auto_return.go     â€" maybeCreateReturnOrder and related
//   wiring_kanban.go          â€" demand-registry signalling on bin moves
//   wiring_telemetry.go       â€" per-transition mission events + summary
//   wiring_count_group.go     â€" CountGroup broadcast to edges
//
// sendToEdge (the outbound envelope helper) also lives here since it
// is shared by the subscription handlers above.
//
// Typed-payload note: every subscription whose handler reads the event
// payload uses eventbus.SubscribeTyped — the generic wrapper that pulls
// the concrete payload off TypedEvent[T, P] so callers don't write
// evt.Payload.(SomeEvent) assertions. The few subscriptions that don't
// read the payload (the fulfillment scanner trigger) keep the original
// Bus.SubscribeTypes form because there's no payload type to constrain.

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingocore/dispatch"
)

// â"€â"€ Outbound messaging â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€

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

// â"€â"€ Event subscriptions â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€

func (e *Engine) wireEventHandlers() {
	// â"€â"€ Dispatch tracking â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	// When an order is dispatched, track it in the tracker
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderDispatchedEvent]) {
		ev := evt.Payload
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

	// â"€â"€ Vendor status changes â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderStatusChangedEvent]) {
		ev := evt.Payload
		e.dbg("vendor status change: order=%d vendor=%s %s->%s robot=%s", ev.OrderID, ev.VendorOrderID, ev.OldStatus, ev.NewStatus, ev.RobotID)
		e.handleVendorStatusChange(ev)
	}, EventOrderStatusChanged)

	// Record mission telemetry on every vendor status change
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderStatusChangedEvent]) {
		e.recordMissionEvent(evt.Payload)
	}, EventOrderStatusChanged)

	// â"€â"€ Order failure â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		ev := evt.Payload
		e.logFn("engine: order %d failed: %s - %s", ev.OrderID, ev.ErrorCode, ev.Detail)
		e.db.AppendAudit("order", ev.OrderID, "failed", "", ev.Detail, "system")

		// Notify ShinGo Edge so it can transition the order locally.
		// Mirrors the EventOrderCancelled handler's notification block below.
		// The edge handler (HandleOrderError) is idempotent â€" duplicate
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
			// If child of a compound order, handle parent failure. Otherwise a
			// top-level order failing may be a two-robot swap leg — unwind its
			// sibling so a half-swap can't strand/collide the line (ALN_003
			// post-dispatch window; HandleSwapPeerTerminal self-gates on the
			// durable sibling link, so it is a no-op for non-swap orders).
			if order.ParentOrderID != nil && e.dispatcher != nil {
				e.dispatcher.HandleChildOrderFailure(*order.ParentOrderID, ev.OrderID)
			} else if e.dispatcher != nil {
				e.dispatcher.HandleSwapPeerTerminal(ev.OrderID, dispatch.SwapTerminalFailed)
			}
		}
	}, EventOrderFailed)

	// ── Order skipped ────────────────────────────────────────────────────
	// Mirrors the failure handler above but for the "work was never needed"
	// terminal. No return order, no anomaly audit — the operator-facing
	// surface treats this as a clean no-op. Edge advances the linked
	// changeover node task via HandleOrderSkipped.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderSkippedEvent]) {
		ev := evt.Payload
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

		// A skipped two-robot swap SUPPLY is a lost replacement — unwind the evac
		// so it cannot strand the line. A skipped EVAC is moot (the resident was
		// already gone) and the handler treats it as a clean no-op.
		if e.dispatcher != nil {
			e.dispatcher.HandleSwapPeerTerminal(ev.OrderID, dispatch.SwapTerminalSkipped)
		}
	}, EventOrderSkipped)

	// â"€â"€ Order completion â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCompletedEvent]) {
		ev := evt.Payload
		e.logFn("engine: order %d completed", ev.OrderID)
		e.db.AppendAudit("order", ev.OrderID, "completed", "", "", "system")
		e.handleOrderCompleted(ev)
	}, EventOrderCompleted)

	// â"€â"€ Order cancellation â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCancelledEvent]) {
		ev := evt.Payload
		e.logFn("engine: order %d cancelled: %s", ev.OrderID, ev.Reason)
		e.db.AppendAudit("order", ev.OrderID, "cancelled", "", ev.Reason, "system")

		// Notify ShinGo Edge so it can transition the order locally.
		// The dispatcher path (edge-initiated cancel) sends its own reply via
		// ReplySender.SendCancelled, but engine-initiated cancellations (web UI
		// terminate, fleet status change, recovery) go through this event handler.
		// The edge handler (HandleOrderCancelled) is idempotent â€" a duplicate
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

		// Auto-return (minting a store order to send the bin back to its origin on
		// cancel/fail) was removed with the plain-store family — it was dormant
		// (never completed in production) and store was the wrong vehicle. The bin's
		// claim is released by the standard terminal teardown; the physical bin stays
		// where the robot left it until an operator moves it. The requirement (return
		// a bin on cancel/fail) is preserved as a future COORDINATED return-order build.

		// If a two-robot swap leg was cancelled (operator terminate, fleet fault,
		// or this handler cancelling the sibling), unwind its peer so a half-swap
		// can't strand/collide the line. Self-gates on the durable sibling link;
		// the peer's own re-entrant call terminates on the IsTerminal guard.
		if e.dispatcher != nil {
			kind := dispatch.SwapTerminalCancelled
			// An operator-accepted half-swap must NOT cancel the committed
			// partner — the marker rides the cancel reason end to end.
			if ev.Reason == protocol.CancelReasonAcceptHalfSwap {
				kind = dispatch.SwapTerminalAbandoned
			}
			e.dispatcher.HandleSwapPeerTerminal(ev.OrderID, kind)
		}
	}, EventOrderCancelled)

	// â"€â"€ Audit-only subscriptions â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderReceivedEvent]) {
		ev := evt.Payload
		e.logFn("engine: order %d received from %s: %s %s -> %s", ev.OrderID, ev.StationID, ev.OrderType, ev.PayloadCode, ev.DeliveryNode)
		e.db.AppendAudit("order", ev.OrderID, "received", "", fmt.Sprintf("%s %s from %s", ev.OrderType, ev.PayloadCode, ev.StationID), "system")
	}, EventOrderReceived)

	// Bin contents changes: audit
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
		ev := evt.Payload
		e.db.AppendAudit("bin", ev.BinID, ev.Action, "", fmt.Sprintf("payload=%s node=%d", ev.PayloadCode, ev.NodeID), "system")
	}, EventBinUpdated)

	// Node updates: audit
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, NodeUpdatedEvent]) {
		ev := evt.Payload
		e.db.AppendAudit("node", ev.NodeID, ev.Action, "", ev.NodeName, "system")
	}, EventNodeUpdated)

	// Corrections: audit
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, CorrectionAppliedEvent]) {
		ev := evt.Payload
		e.db.AppendAudit("correction", ev.CorrectionID, ev.CorrectionType, "", ev.Reason, ev.Actor)
	}, EventCorrectionApplied)

	// â"€â"€ CMS transaction logging â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
		ev := evt.Payload
		if ev.Action == "moved" && ev.FromNodeID != 0 && ev.ToNodeID != 0 {
			e.RecordMovementTransactions(ev)
		}
	}, EventBinUpdated)

	// â"€â"€ Fulfillment scanner triggers â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	// Async trigger for high-volume signals (bin moves, order
	// completions). The scanner coalesces overlapping triggers via
	// its `pending` flag; a goroutine here keeps the emitting handler
	// chain non-blocking. Subscribes to several event types whose
	// payloads differ; stays on the untyped SubscribeTypes form
	// because the trigger doesn't read the payload.
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
	// of the bin-transit-state project â€" every pickup that moves a bin
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
	// observable on return from HandleComplexOrderRequest â€" the existing
	// test fixtures rely on that ordering, and operator-facing latency
	// expectations don't tolerate "queued for ~1ms while a goroutine
	// gets scheduled." Untyped subscribe — handler doesn't read payload.
	e.Events.SubscribeTypes(func(Event) {
		if e.fulfillment != nil {
			e.fulfillment.RunOnce()
		}
	}, EventOrderQueued)

	// â"€â"€ Per-block completion â†’ transit transition â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	// Phase 2 of the bin-transit-state project: pickup blocks (BinTask=Load
	// or "pickup"-flavoured operations) drive the bin claimed at that step
	// onto the synthetic _TRANSIT node. The poller diffs per-block state
	// and fires EventBlockCompleted on the transition into FINISHED; this
	// handler routes by block kind.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BlockCompletedEvent]) {
		e.handleBlockCompleted(evt.Payload)
	}, EventBlockCompleted)

	// ── Restore-blockers + lane-lock-extension listeners ──────────────
	// Both listeners trigger on the same bin-transit and parent-
	// terminal events:
	//
	//   - Restore-blockers (toggle-on path): when the complex parent
	//     picks up its target, dispatch the synthetic-restock
	//     compound. Idempotent: ConsumeByBin one-shots.
	//   - Lane-lock-extension (v7 Step 4.5, expose mode only): when
	//     the complex parent picks up its target, release the lane
	//     lock that was held through the post-compound / pre-pickup
	//     window. Also releases on parent cancel/fail so a never-
	//     picked-up parent doesn't strand the lock.
	//
	// Both no-op when no entry matches the event — safe to wire even
	// for groups with neither feature in play.
	//
	// REFACTOR TARGET: v7 added these two reshuffle-specific subscribers
	// (restore-blockers cleanup, lane-lock release) on top of the
	// existing auto-return and audit subscribers. If you're modifying
	// any of the reshuffle terminal handlers, consider consolidating
	// them into a single dispatcher.onComplexParentTerminal(event)
	// subscriber that fans to internal idempotent helpers. Auto-return
	// and audit stay separate — they aren't reshuffle-coupled. See
	// "Refactor targets" in complex-order-buried-reshuffle-scope.md §10
	// for shape and rationale.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinEnteredTransitEvent]) {
		if e.dispatcher == nil {
			return
		}
		e.dispatcher.HandleBinEnteredTransit(evt.Payload.BinID, evt.Payload.FromNodeID)
		e.dispatcher.HandleBinTransitForLaneLock(evt.Payload.BinID, evt.Payload.FromNodeID)
	}, EventBinEnteredTransit)

	// Parent terminal: drop both listeners so the lock isn't stuck
	// and the synthetic-restock parent is cancelled. All four terminal
	// statuses are wired:
	//
	//   - Cancelled / Failed: explicit cleanup paths.
	//   - Skipped: a complex parent that gets skipped at Queued (e.g.,
	//     ApplyComplexPlan returns no_source_bin because the unburied
	//     target was moved or anomalied between unbury completion and
	//     scanner pickup) needs the same cleanup — no pickup happens,
	//     so the bin-transit listener will never fire.
	//   - Completed: defensive idempotent sweep. In the normal happy
	//     path the bin-transit listener already consumed the in-memory
	//     entry and deleted the DB row before the parent reached
	//     Confirmed, so this is a no-op. Covers the rare path where
	//     an admin / recovery action force-confirms a parent past the
	//     pickup leg.
	//
	// Both handlers are safe to call on a parent with no entry —
	// they no-op when nothing matches.
	terminal := func(orderID int64) {
		if e.dispatcher == nil {
			return
		}
		e.dispatcher.HandleComplexParentTerminal(orderID)
		e.dispatcher.HandleComplexParentTerminalForLaneLock(orderID)
	}
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCancelledEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderCancelled)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderFailed)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderSkippedEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderSkipped)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCompletedEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderCompleted)

	// â"€â"€ Queued order audit â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderQueuedEvent]) {
		ev := evt.Payload
		e.logFn("engine: order %d queued for payload %s", ev.OrderID, ev.PayloadCode)
		e.db.AppendAudit("order", ev.OrderID, "queued", "", fmt.Sprintf("payload=%s from %s", ev.PayloadCode, ev.StationID), "system")
	}, EventOrderQueued)

	// â"€â"€ Queue-reason push â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	// Runs third for EventOrderQueued — after the sync scanner (1st) and
	// the audit handler (2nd) above — so the scanner's latest
	// SetOrderQueueDetail call is visible when we read the order back.
	// Only pushes if the order is still acquiring (queued or sourcing — the
	// scanner didn't dispatch) and carries a non-empty blocking reason; orders
	// the scanner dispatched transition out of the acquiring set, suppressing
	// the push. Widened from queued-only to the acquiring set so a `sourcing`
	// order's block reason still reaches Edge (its actual status rides along).
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderQueuedEvent]) {
		ev := evt.Payload
		if ev.EdgeUUID == "" || ev.StationID == "" {
			return
		}
		order, err := e.db.GetOrder(ev.OrderID)
		if err != nil {
			e.logFn("engine: queue_reason push: load order %d: %v", ev.OrderID, err)
			return
		}
		if !protocol.IsAcquiring(order.Status) || order.QueueReason == "" {
			return
		}
		if err := e.sendToEdge(protocol.TypeOrderUpdate, ev.StationID, &protocol.OrderUpdate{
			OrderUUID:   ev.EdgeUUID,
			Status:      string(order.Status),
			QueueReason: order.QueueReason,
			QueueCode:   order.QueueCode,
		}); err != nil {
			e.logFn("engine: queue_reason update to edge: %v", err)
		}
	}, EventOrderQueued)

	// â"€â"€ Kanban demand â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	// look up the demand registry and send a demand signal to Edge.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
		e.handleKanbanDemand(evt.Payload)
	}, EventBinUpdated)

	// ── UOP-threshold replenishment monitor ─────────────────────────────
	// Combined bin + bucket UOP per payload — fires LoopBelowThresholdSignal
	// when a monitored (loader, payload) drops below its configured
	// threshold. Bucket-apply events go through OnBucketApplied from the
	// messaging layer; bin updates land via this subscription so cell-side
	// consume ticks and loader-side bin moves both re-evaluate.
	if e.thresholdMonitor != nil {
		eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
			e.thresholdMonitor.handleBinUpdated(evt.Payload)
		}, EventBinUpdated)
	}

	// ── Sourceability monitor ───────────────────────────────────────────
	// Keeps the per-(process, style) sourceability answer fresh on change.
	// A bin move (claimed / unclaimed / loaded / cleared) changes the
	// available pool; a new order changes reservations. Both funnel through
	// the payload → styles index and one debounced recompute. Payload-less
	// order events (status/complete/cancel) are covered by the coincident bin
	// events plus the periodic full recompute.
	if e.sourceabilityMonitor != nil {
		eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
			e.sourceabilityMonitor.onPayloadChanged(evt.Payload.PayloadCode)
		}, EventBinUpdated)
		eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderReceivedEvent]) {
			e.sourceabilityMonitor.onPayloadChanged(evt.Payload.PayloadCode)
		}, EventOrderReceived)
		eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderQueuedEvent]) {
			e.sourceabilityMonitor.onPayloadChanged(evt.Payload.PayloadCode)
		}, EventOrderQueued)
	}

	// â"€â"€ Count-group transitions â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€â"€
	// When the countgroup runner detects a debounced occupancy change
	// (or fires the RDS-down fail-safe), ship a CountGroupCommand to
	// all edges. Each edge checks its own bindings map and either
	// drives the PLC tag or ignores.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, CountGroupTransitionEvent]) {
		e.handleCountGroupTransition(evt.Payload)
	}, EventCountGroupTransition)
	// Grace-expiry: poller detected a faulted order whose grace period expired
	// without fleet recovery. Best-effort cancel at RDS, then local fail.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, GraceExpiredEvent]) {
		e.handleGraceExpired(evt.Payload)
	}, EventGraceExpired)
}
