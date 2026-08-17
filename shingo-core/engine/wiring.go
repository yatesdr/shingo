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
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingocore/dispatch"
	"shingocore/notify"
)

func lookupRobotID(e *Engine, orderID int64) string {
	if orderID == 0 {
		return ""
	}
	order, err := e.db.GetOrder(orderID)
	if err != nil || order == nil {
		return ""
	}
	return order.RobotID
}

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

		// Notify ShinGo Edge, for the same reason the cancellation handler
		// below does — and this arm's absence is why Edge rows strand at
		// `delivered` forever.
		//
		// THERE IS NO OTHER PATH. EmitOrderCompleted reaches Core's own event
		// bus and stops there; nothing in protocol/ or shingo-edge/messaging
		// carries an order completion. Two comments in this repo assert
		// otherwise (engine.go's confirmDelivered closure and
		// recovery_service.ForceConfirmDelivered both say the emit chain
		// "notifies Edge"), which is why the gap survived three separate
		// investigations — the code documents a message it never sends.
		//
		// Placed on the completion EVENT rather than on the reconciliation
		// sweep that exposed it, because Core confirms behind Edge's back from
		// three sites: the 5-minute stuck-delivered sweep (engine.go), the
		// compound-child auto-confirm (wiring_vendor_status.go, receipt
		// `auto_confirm_internal`), and the operator's force-confirm button
		// (recovery_service.go). Every one funnels through here. Wiring the
		// sweep alone would have fixed a third of it.
		//
		// Springfield 2026-08-03: 115 of 331 swap legs in 14 days were
		// confirmed by the sweep, each leaving Edge and Core disagreeing until
		// the next edge restart. One of them (order 4017) still read
		// `delivered` two and a half hours later, kept a finished order in
		// ALN_001's active list, and handed the operator-station modal a stale
		// `queue_reason` to display during the NEXT changeover.
		//
		// Reuses TypeOrderUpdate rather than minting a completion message:
		// Edge's ApplyCoreStatus is already the total Core→Edge status mapping
		// and already runs on this envelope. The echo — Edge confirms, tells
		// Core, Core completes and tells Edge it confirmed — costs one message
		// and no writes: that arm returns early when the Edge row is already
		// terminal, which after an Edge-side confirm it always is.
		if ev.StationID != "" && ev.EdgeUUID != "" {
			if err := e.sendToEdge(protocol.TypeOrderUpdate, ev.StationID,
				&protocol.OrderUpdate{
					OrderUUID: ev.EdgeUUID,
					Status:    string(protocol.StatusConfirmed),
					Detail:    "confirmed at Core",
				}); err != nil {
				e.logFn("engine: completion notification to edge: %v", err)
			} else {
				e.dbg("completion notification sent to edge: station=%s uuid=%s", ev.StationID, ev.EdgeUUID)
			}
		}

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

		// A DISSOLVED DIG'S CANCELS RE-DRIVE THEIR COMPOUND, so the terminal arm can
		// return the parent to the acquiring set. The dissolve deliberately does not
		// transition the parent itself: {Reshuffling → Queued} fires the SYNCHRONOUS
		// fulfillment scanner, and the dissolve is reachable from inside that scanner
		// (tryFulfill → PlanBuriedReshuffle → CreateCompoundOrder →
		// AdvanceCompoundOrder) under a non-reentrant scanMu. This is the hop that
		// breaks the loop — the same reason triggerFulfillment above spawns rather
		// than calls.
		//
		// SCOPED TO DISSOLVE CANCELS, and narrowly, because the first version was
		// not. Re-driving on EVERY child cancel put an advance in the middle of every
		// other teardown, and the operator-cancel path is the one that bit: it
		// cancels the children BEFORE the parent, so the re-drive arrived while the
		// parent still read `reshuffling`, dissolved the next leg, and raced the
		// parent's own cancel to a `failed` finish. An operator asked for cancelled
		// and got failed.
		//
		// The other teardowns need nothing from this: they are ending the compound,
		// not re-planning it, and the reconciliation sweep remains their backstop
		// exactly as before.
		// GATE 1 WIDENED IT BY ONE MARKER, not by loosening it. A chapter now
		// also ends when a LEG FAILS, and those cancels need the same hop for
		// the same reason: without it the demand's disposition waits for the
		// 30-second reconciliation sweep instead of arriving on the event.
		// dispatch.IsChapterEndCancel is the one list both sides read, so the
		// narrow scoping this comment block is about survives being widened.
		if e.dispatcher != nil && dispatch.IsChapterEndCancel(ev.Reason) {
			if order, err := e.db.GetOrder(ev.OrderID); err == nil && order.ParentOrderID != nil {
				parentID := *order.ParentOrderID
				go func() {
					if err := e.dispatcher.AdvanceCompoundOrder(parentID); err != nil {
						e.logFn("engine: advance dissolved compound %d after leg %d cancelled: %v",
							parentID, ev.OrderID, err)
					}
				}()
			}
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
	// EventOrderSkipped — the one terminal this set was missing, and the
	// unification is what made the gap matter. TerminalizeOrder releases an
	// order's reservations in the same transaction as the status write for EVERY
	// terminal including skip (store/orders.go → reservations.ReleaseByOrder), so
	// a skipped order frees its lane occupancy exactly as a cancelled one does —
	// but only the other three re-drove the scanner, so a plain order parked on
	// that occupancy waited for the ticker instead of for the event. The lane-gate
	// evaluator already subscribed to all four (engine/wiring_lane_gate.go); this
	// makes the two trigger sets agree.
	e.Events.SubscribeTypes(triggerFulfillment, EventOrderSkipped)
	// EventBinEnteredTransit is the slot-vacancy signal added in Phase 1
	// of the bin-transit-state project â€" every pickup that moves a bin
	// to _TRANSIT frees its source slot, which can unblock queued orders
	// that needed to drop something there. Subscribing here makes the
	// scanner re-evaluate without waiting for the order to fully complete.
	e.Events.SubscribeTypes(triggerFulfillment, EventBinEnteredTransit)
	// NOTE: a sixth trigger — EventBlockCompleted — is deliberately registered
	// further down, immediately after the handleBlockCompleted subscription,
	// because it must observe the mouth row that handler releases. See there.

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

	// Fulfillment trigger on per-block completion (A′ — placement release).
	// A store parked by the tiered-entry gate is waiting on a DEEPER store to
	// get its bin into the lane; that moment is the deeper store's dropoff
	// block reaching FINISHED, where handleStoreBlockCompleted deletes its
	// inbound mouth row (wiring_block_completed.go → ReleaseInboundLaneForOrder)
	// and the gate's active set stops counting it (dispatch/lane_entry.go
	// stillWorkingLaneMouth). Nothing re-scanned on that signal before, so a
	// parked order sat until the blocker's whole ORDER completed — the gate was
	// completion-coarse purely for want of this subscription.
	//
	// REGISTRATION ORDER IS LOAD-BEARING. The bus dispatches synchronously in
	// registration order (protocol/eventbus: "Subscribers are called in
	// registration order on the emitting goroutine"), so this MUST stay AFTER
	// the handleBlockCompleted subscription above — that handler is what drops
	// the mouth row. Registered before it, the scan would read the pre-release
	// state, still see the placer as a blocker, and the admit would slip to the
	// next trigger or the periodic sweep. Do not reorder these two.
	e.Events.SubscribeTypes(triggerFulfillment, EventBlockCompleted)

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
		// Lane mouth gate (§4): release the order's hold on the lane its bin just
		// left, as soon as the bin physically clears (a no-op when the gate is off).
		e.dispatcher.HandleTransitForLaneGate(evt.Payload.OrderID, evt.Payload.FromNodeID)
	}, EventBinEnteredTransit)

	// Parent terminal: drop the lane-lock release listener so the lane
	// isn't stuck held if the parent terminates before its pickup. All
	// four terminal statuses are wired:
	//
	//   - Cancelled / Failed: explicit cleanup paths.
	//   - Skipped: a complex parent that gets skipped at Queued (e.g.,
	//     ApplyComplexPlan returns no_source_bin because the unburied
	//     target was moved or anomalied between unbury completion and
	//     scanner pickup) needs the same cleanup — no pickup happens,
	//     so the bin-transit release will never fire.
	//   - Completed: defensive idempotent sweep. On the normal happy
	//     path the bin-transit release already fired before the parent
	//     reached Confirmed, so this is a no-op. Covers the rare path
	//     where an admin / recovery action force-confirms a parent past
	//     the pickup leg.
	//
	// Safe to call on a parent with no hold — it no-ops when nothing matches.
	// THE EXPOSE BRIDGE'S RELEASE ARMS USED TO HANG HERE. A complex parent going
	// terminal, and a bin entering transit, each consumed a pending_lane_extensions
	// row to drop a lane lock that had been TRANSFERRED to the parent past its
	// compound's completion. Nothing transfers a lock any more — the demand is not
	// re-parented into its own dig, so it never comes back and the lane is released
	// at the compound's terminal like every other dig's (compound.go). The
	// subscriptions are kept, empty of that call, because their OTHER arms are live.
	terminal := func(orderID int64) {
		if e.dispatcher == nil {
			return
		}
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

	// ── Resume push: the parent left `reshuffling` ────────────────────────
	//
	// UNCONDITIONAL, WHICH IS THE POINT. The queue-reason push above returns
	// early without a block sentence and without an acquiring status; a resumed
	// parent has neither, so it fell through both and the Edge never learned the
	// order had left `reshuffling`. Its mirror then rejected every later push as
	// an illegal jump and the order became unreleasable — three robots a run.
	//
	// Status is written as `queued` rather than read back off the row: by the
	// time this runs the in-band scanner may already have dispatched the order,
	// and the Edge needs the step it MISSED, not the one Core is on now.
	// reshuffling → queued is the only legal edge out of reshuffling toward the
	// live path, so it is the one the mirror has to be walked through.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderResumedEvent]) {
		ev := evt.Payload
		if ev.EdgeUUID == "" || ev.StationID == "" {
			return
		}
		if err := e.sendToEdge(protocol.TypeOrderUpdate, ev.StationID, &protocol.OrderUpdate{
			OrderUUID: ev.EdgeUUID,
			Status:    string(protocol.StatusQueued),
			Detail:    "reshuffle complete; parent requeued",
		}); err != nil {
			e.logFn("engine: resume notification to edge for order %d: %v", ev.OrderID, err)
		} else {
			e.dbg("resume notification sent to edge: station=%s uuid=%s", ev.StationID, ev.EdgeUUID)
		}
	}, EventOrderResumed)

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

		// P2-C9 contradiction check: a manual swap request (a complex order —
		// the shape the SNF3 operator swap took) for a payload the ledger reads
		// as fully stocked is a contradiction. Log it, chip it, and re-read.
		// Filtered to complex orders (the swap/manual-request family); the
		// stocked-ledger gate + per-payload throttle keep it quiet in normal ops.
		eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderReceivedEvent]) {
			if evt.Payload.OrderType == protocol.OrderTypeComplex {
				e.thresholdMonitor.NoteSwapRequestContradiction(evt.Payload.PayloadCode)
			}
		}, EventOrderReceived)
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

	// ── Notifications (email alerts) ──────────────────────────────
	// Subscribers are always registered so toggling the enabled checkbox
	// at runtime takes effect without a restart. Each handler checks
	// Enabled() at dispatch time.
	//
	// Fault emails are buffered: the order must remain faulted for 3
	// minutes before the alert fires. A recovery within that window
	// cancels the pending email. Fail and GraceExpired fire immediately.
	//
	// Fault and Cleared emails are threaded: the fault email carries a
	// Message-ID header; the cleared email replies to it so email
	// clients group them as a conversation.

	type faultSentInfo struct {
		messageID string
		sentAt    time.Time
		robotID   string
		edgeUUID  string
		stationID string
	}

	var faultTimersMu sync.Mutex
	faultTimers := make(map[int64]*time.Timer)

	var faultSentMu sync.Mutex
	faultSent := make(map[int64]faultSentInfo)

	const faultBufferDuration = 1 * time.Minute

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFaultedEvent]) {
		if !e.notifier.Enabled() {
			return
		}
		ev := evt.Payload
		robotID := lookupRobotID(e, ev.OrderID)

		faultTimersMu.Lock()
		if existing, ok := faultTimers[ev.OrderID]; ok {
			existing.Stop()
		}
		faultTimers[ev.OrderID] = time.AfterFunc(faultBufferDuration, func() {
			faultTimersMu.Lock()
			delete(faultTimers, ev.OrderID)
			faultTimersMu.Unlock()

			msgID := notify.GenerateMessageID(fmt.Sprintf("fault-%d", ev.OrderID))
			_ = e.notifier.SendWithHeaders(
				notify.FaultSubject(robotID),
				notify.FaultAlert(ev.OrderID, ev.EdgeUUID, ev.StationID, ev.Reason, robotID),
				notify.WithMessageID(msgID),
			)

			faultSentMu.Lock()
			faultSent[ev.OrderID] = faultSentInfo{
				messageID: msgID,
				sentAt:    time.Now(),
				robotID:   robotID,
				edgeUUID:  ev.EdgeUUID,
				stationID: ev.StationID,
			}
			faultSentMu.Unlock()
		})
		faultTimersMu.Unlock()
	}, EventOrderFaulted)

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFaultedRecoveredEvent]) {
		if !e.notifier.Enabled() {
			return
		}
		ev := evt.Payload
		robotID := ev.RobotID
		if robotID == "" {
			robotID = lookupRobotID(e, ev.OrderID)
		}

		faultTimersMu.Lock()
		if existing, ok := faultTimers[ev.OrderID]; ok {
			existing.Stop()
			delete(faultTimers, ev.OrderID)
		}
		faultTimersMu.Unlock()

		var opts []notify.SendOption
		var timeFaulted string

		faultSentMu.Lock()
		if info, ok := faultSent[ev.OrderID]; ok {
			d := time.Since(info.sentAt).Round(time.Second)
			timeFaulted = fmt.Sprintf("%d m %d s", int(d.Minutes()), int(d.Seconds())%60)
			opts = []notify.SendOption{
				notify.WithInReplyTo(info.messageID),
				notify.WithReferences(info.messageID),
				notify.WithMessageID(notify.GenerateMessageID(fmt.Sprintf("cleared-%d", ev.OrderID))),
			}
			delete(faultSent, ev.OrderID)
			if ev.EdgeUUID == "" {
				ev.EdgeUUID = info.edgeUUID
			}
			if ev.StationID == "" {
				ev.StationID = info.stationID
			}
		}
		faultSentMu.Unlock()

		_ = e.notifier.SendWithHeaders(
			notify.FaultClearedSubject(robotID),
			notify.FaultClearedAlert(ev.OrderID, ev.EdgeUUID, ev.StationID, robotID, timeFaulted),
			opts...,
		)
	}, EventOrderFaultedRecovered)

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		if !e.notifier.Enabled() {
			return
		}
		ev := evt.Payload
		robotID := lookupRobotID(e, ev.OrderID)
		_ = e.notifier.Send(
			notify.FailSubject(robotID),
			notify.FailAlert(ev.OrderID, ev.EdgeUUID, ev.StationID, ev.ErrorCode, ev.Detail, robotID),
		)
	}, EventOrderFailed)

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, GraceExpiredEvent]) {
		if !e.notifier.Enabled() {
			return
		}
		ev := evt.Payload
		robotID := lookupRobotID(e, ev.OrderID)
		_ = e.notifier.Send(
			notify.GraceExpiredSubject(),
			notify.GraceExpiredAlert(ev.OrderID, ev.VendorOrderID, robotID),
		)
	}, EventGraceExpired)

	// ── Lane-gate release evaluator ─────────────────────────────────────
	// Registered LAST on purpose. The bus dispatches synchronously in
	// registration order, and the evaluator has to observe the mouth rows that
	// handlers above it release — handleBlockCompleted on a dropoff, and the
	// bin-transit handler on a pickup. Registering it last is the cheapest way
	// to be after all of them; see wiring_lane_gate.go.
	e.wireLaneGateHandlers()
}
