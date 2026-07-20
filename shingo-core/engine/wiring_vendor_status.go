// wiring_vendor_status.go — Fleet status change handling.
//
// handleVendorStatusChange maps vendor state strings to ShinGo order
// status, writes the transition to the DB, and emits the appropriate
// edge notifications (waybill on first robot assignment, status update,
// staged notification). On terminal states it dispatches to the
// delivery / failure / cancel handlers.

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/dispatch/eta"
	"shingocore/store/orders"
)

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

	newStatus := protocol.Status(e.fleet.MapState(ev.NewStatus))
	if newStatus == order.Status {
		// Idempotent path: status unchanged, check if robot ID changed
		if effectiveRobotID != order.RobotID {
			if err := e.db.UpdateOrderRobotID(order.ID, effectiveRobotID); err != nil {
				e.logFn("engine: update order %d robot: %v", order.ID, err)
			}
		}
		return
	}

	// Vendor-state-domain mapping has already produced newStatus (a ShinGo
	// status). Route through the typed lifecycle method matching the
	// target ShinGo status — the vendor terminal-check at line 81 below
	// guards against routing terminal vendor states through MarkInTransit/
	// MarkStaged. Cancel/Fail are dispatched in the post-mapping switch.
	lc := e.dispatcher.Lifecycle()
	switch newStatus {
	case dispatch.StatusInTransit:
		if err := lc.MarkInTransit(order, effectiveRobotID, "fleet"); err != nil {
			e.logFn("engine: mark in_transit order %d: %v", order.ID, err)
		}
	case dispatch.StatusStaged:
		if err := lc.MarkStaged(order, "fleet"); err != nil {
			e.logFn("engine: mark staged order %d: %v", order.ID, err)
		}
	case dispatch.StatusDelivered:
		// Move bins to their destinations FIRST, then transition the order.
		// The lifecycle's MarkDelivered fires the fireCompleted action which
		// synchronously dispatches EventOrderCompleted to handleOrderCompleted;
		// that subscriber's handleMultiBinCompleted path moves bins AND deletes
		// the order_bins junction rows. If we transition first, handleOrderDelivered
		// runs after the deletion and falls back to its single-bin branch (using
		// order.DeliveryNode = the FINAL step's node, which is the wrong target
		// for all but the last bin in a multi-step complex order).
		//
		// Calling handleOrderDelivered first makes handleMultiBinCompleted's
		// "skip bins already at destination" idempotency guard trigger correctly,
		// and the junction cleanup happens after the bins are already in place.
		e.handleOrderDelivered(order)
		if err := lc.MarkDelivered(order, "fleet"); err != nil {
			e.logFn("engine: mark delivered order %d: %v", order.ID, err)
		}
		// Robot-internal compound children (reshuffle / buried-bin / restock
		// legs) have no operator at the destination to file a receipt, so
		// Delivered (non-terminal) would otherwise sit until the 5-minute
		// reconciliation sweep before the next child could dispatch — an
		// N-step shuffle would take N×5min. Auto-confirm immediately for any
		// child order. ParentOrderID != nil is the discriminator: restock
		// children deliver to a real lane slot, so "destination is a shuffle
		// slot" and OrderType==Move are both wrong. The Confirmed transition's
		// fireCompleted re-enters AdvanceCompoundOrder and the sibling-in-flight
		// guard ensures the next child dispatches exactly once. Idempotent — a
		// later edge/reconciliation receipt becomes a no-op (CompletedAt set).
		if order.ParentOrderID != nil {
			if _, err := lc.ConfirmReceipt(order, order.StationID, "auto_confirm_internal", 0); err != nil {
				e.logFn("engine: auto-confirm child order %d: %v", order.ID, err)
			}
		}
	case dispatch.StatusAcknowledged:
		// Defensive: fleet.MapState never returns StatusAcknowledged today,
		// but handle for completeness in case a future fleet adapter reports
		// a distinct ACK phase.
		//
		// This arm is dead in practice. fleet.MapState (fleet/seerrds/mappers.go)
		// maps RDS states to dispatched/in_transit/staged/delivered/faulted/
		// cancelled — never acknowledged. Core's vendor ladder starts at
		// dispatched (CREATE/TO_BE_DISPATCHED → dispatched), so this is a
		// never-fires guard against a future adapter change, not a live code
		// path. Both `submitted` and `acknowledged` are Edge-lifecycle words in
		// Core's vendor flow.
		if err := lc.Acknowledge(order, "fleet"); err != nil {
			e.logFn("engine: acknowledge order %d: %v", order.ID, err)
		}
	case dispatch.StatusDispatched:
		// Fleet shouldn't actually report Dispatched — the dispatcher
		// writes that status itself after backend.CreateOrder returns.
		// If we see it from MapState, log and skip rather than silently
		// re-writing the status.
		e.logFn("engine: unexpected fleet-reported Dispatched for order %d, skipping", order.ID)
	case dispatch.StatusFailed, dispatch.StatusCancelled:
		// Handled by the post-mapping switch below.
	case dispatch.StatusFaulted:
		if err := lc.MarkFaulted(order, effectiveRobotID, fmt.Sprintf("fleet state: %s", ev.NewStatus)); err != nil {
			e.logFn("engine: mark faulted order %d: %v", order.ID, err)
		}
	default:
		// Unknown mapped status — should never fire under the current
		// seerrds adapter (MapState in fleet/seerrds/mappers.go produces
		// only the cases handled above). If it does fire, log and skip
		// rather than silently bypassing the state machine. A mapped
		// status outside the typed-method list signals an adapter
		// mismatch that needs investigation, not a generic DB write.
		e.logFn("engine: unknown mapped status %q for order %d (vendor=%q); skipping write — adapter MapState may be out of sync with dispatch.Status* constants", newStatus, order.ID, ev.NewStatus)
	}
	if err := e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, effectiveRobotID); err != nil {
		e.logFn("engine: update order %d vendor state: %v", order.ID, err)
	}

	// Send status update to ShinGo Edge. On transitions INTO in_transit
	// we compute a per-route ETA from the medians cache and include it
	// on the update; Edge stores it and the operator HMI renders an ETA
	// pill on the node tile. On any other status the ETA is left nil
	// (Edge doesn't render pills on pre-in-transit statuses and treats
	// terminal statuses as pill-hidden — see operator-render.js).
	update := &protocol.OrderUpdate{
		OrderUUID: order.EdgeUUID,
		Status:    string(newStatus),
		Detail:    fmt.Sprintf("fleet state: %s", ev.NewStatus),
	}
	if newStatus == dispatch.StatusInTransit {
		if etaStr := eta.Stamp(e.etaCache, order.SourceNode, order.DeliveryNode); etaStr != "" {
			update.ETA = etaStr
		}
	}
	if err := e.sendToEdge(protocol.TypeOrderUpdate, order.StationID, update); err != nil {
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
		// handleOrderDelivered already ran before MarkDelivered above so its
		// bin-movement happens before the action map's fireCompleted fires
		// EventOrderCompleted. Nothing left to do here for the delivery case.
	case dispatch.StatusFailed:
		e.handleFleetOrderFailed(order, ev.Detail)
	case dispatch.StatusCancelled:
		e.handleFleetOrderCancelled(order)
	}
}

func (e *Engine) handleFleetOrderFailed(order *orders.Order, fleetDetail string) {
	detail := fleetDetail
	if detail == "" {
		detail = "fleet order failed"
	}
	if err := e.dispatcher.Lifecycle().Fail(order, order.StationID, "fleet_failed", detail); err != nil {
		e.logFn("engine: fail order %d: %v", order.ID, err)
	}
}

func (e *Engine) handleFleetOrderCancelled(order *orders.Order) {
	// lifecycle.CancelOrder handles fleet-cancel + atomic transition + emit.
	// PreviousStatus is captured by transition() before the status flip and
	// passed through to emitCancelled via the Event.
	e.dispatcher.Lifecycle().CancelOrder(order, order.StationID, "fleet order stopped")
}
func (e *Engine) handleGraceExpired(ev GraceExpiredEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: grace-expiry: load order %d: %v", ev.OrderID, err)
		return
	}
	if protocol.IsTerminal(order.Status) {
		e.logFn("engine: grace-expiry: order %d already terminal (%s), skipping", order.ID, order.Status)
		return
	}

	// Fail locally FIRST so grace_timeout is the recorded terminal cause. If we
	// cancelled at the vendor first, the status poller could observe the
	// resulting RDS STOP and flip the order to cancelled "fleet order stopped"
	// before this Fail lands — mislabeling a shingo-initiated timeout failure as
	// an unattributed vendor cancel (Q-030 grace-expiry race). Once the order is
	// locally terminal, the poller's cancel echo no-ops on CancelOrder's
	// terminality guard (lifecycle.go).
	if err := e.dispatcher.Lifecycle().Fail(order, order.StationID, "grace_timeout", "grace period expired without fleet recovery"); err != nil {
		e.logFn("engine: grace-expiry fail order %d: %v", order.ID, err)
	}

	// Best-effort stop at the fleet vendor (the order is already locally
	// terminal). RDS may be unreachable; the local fail above stands regardless.
	if err := e.fleet.CancelOrder(order.VendorOrderID); err != nil {
		e.logFn("engine: terminate order %d (RDS %s): %v", order.ID, order.VendorOrderID, err)
	}
}
