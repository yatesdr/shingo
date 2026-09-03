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
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/dispatch"
	"shingocore/dispatch/eta"
	"shingocore/fleet"
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

	// The fault fields for this transition, shared between the lifecycle write
	// and the Edge push below. wasFaulted is captured BEFORE any transition
	// runs: order.Status is the pre-transition status here and the recovery
	// branch is the only thing that can tell a recovery from an ordinary
	// transit row.
	now := clock.Now().UTC()
	wasFaulted := order.Status == dispatch.StatusFaulted
	var faultRef protocol.TermRef
	var faultSince time.Time

	switch newStatus {
	case dispatch.StatusInTransit:
		if wasFaulted {
			// RECOVERY. 706 of 730 faults end here, and until 2026-08-22 they
			// wrote "fleet reported in transit" — the same row an order that
			// was never in trouble writes. MarkFaultedRecovered existed and had
			// no caller because MapState("RUNNING") routes to MarkInTransit;
			// this is the branch that makes the dead method the real path.
			//
			// The dwell comes from the faulted history row, NOT orders.updated_at
			// — UpdateOrderVendor below bumps that after every poll, so it would
			// measure the last time the fleet spoke rather than the fault.
			ref, since := e.latestFaultReason(order.ID)
			reason := protocol.FormatFaultSentence(
				protocol.FaultPhaseRecovered, ref, since, now, false)
			if err := lc.MarkFaultedRecovered(order, effectiveRobotID, ref, reason); err != nil {
				e.logFn("engine: mark recovered order %d: %v", order.ID, err)
			}
			break
		}
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
		// The fleet's reason has ridden ev.Snapshot through five layers and was
		// dropped by this call until 2026-08-22. It is written as "Replanning"
		// because it always is at the instant of faulting — the row's detail is
		// a record of what was true when it was written, and the live sentence
		// an operator reads is computed at read time against the threshold.
		faultRef, faultSince = faultRefFrom(ev.Snapshot, order), now
		reason := protocol.FormatFaultSentence(
			protocol.FaultPhaseLive, faultRef, now, now, false)
		if err := lc.MarkFaulted(order, effectiveRobotID, faultRef, reason); err != nil {
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
	if newStatus == dispatch.StatusFaulted {
		// The Edge board shows this string to an operator. "fleet state: FAILED"
		// is the vendor's word for a state 97% of which recovers on its own in
		// 20 seconds, and it is also the word this design refuses to use about a
		// live order.
		update.Detail = protocol.FormatFaultSentence(
			protocol.FaultPhaseLive, faultRef, faultSince, now, false)
		e.attachFaultFields(update, faultRef, faultSince)
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
		// AND PUT THE REASON ON THE ROW. A robot parked at a STATION-owned wait
		// used to carry nothing at all: no code, no cause, no sentence. On the
		// board and in every diagnostic query that is indistinguishable from an
		// order nobody has evaluated, which is exactly how three of them stood
		// for a whole soak while the investigation hunted a fence that was
		// refusing them (§12.49). Nothing was refusing them.
		//
		// Only for STATION waits. A lane wait is Core's and the evaluator writes
		// its own cause when it refuses; overwriting that here would replace a
		// specific refusal ("lane-occupied", "lane-target-buried") with a
		// generic one.
		e.dispatcher.MarkStationWaitIfOwned(order.ID)
		// AND A LANE WAIT GETS RE-ASKED THE MOMENT THE ROBOT IS THERE.
		//
		// For an inbound dweller this is a cheap extra firing of a question the
		// lane's own events already drive. For an OUTBOUND one it is the primary
		// trigger: a dig leg standing in the lane it just lifted from is waiting for
		// Core to choose where the blocker goes, and that choice is deferred to
		// exactly this moment on purpose — it is what keeps the destination
		// uncommitted while the robot works the other lane.
		e.dispatcher.EvaluateWaitLaneForStagedOrder(order.ID)
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

// faultRefFrom builds the history-row reference for a fault: where the order
// was, and what the fleet said about it.
//
// Node and Payload are filled EXPLICITLY rather than left to historyReason's
// default, which only fires when the whole ref is empty. A ref carrying just a
// vendor code would otherwise record why and silently lose where — and where is
// the field that has been carried on 87% of faulted rows all along.
//
// Only the first error is taken. RDS reports errors[] as a list but the live
// data is one entry when there is any at all (22 orders in 30 days, one code);
// a second entry would be a new shape worth looking at before it is rendered
// into a sentence on the floor.
func faultRefFrom(snap *fleet.OrderSnapshot, ord *orders.Order) protocol.TermRef {
	ref := protocol.TermRef{Node: ord.ProcessNode, Payload: ord.PayloadCode}
	if ref.Node == "" {
		ref.Node = ord.DeliveryNode
	}
	if snap != nil && len(snap.Errors) > 0 {
		ref.VendorCode = snap.Errors[0].Code
		ref.VendorDesc = snap.Errors[0].Desc
	}
	return ref
}

// attachFaultFields puts the fault clock on an outbound Edge update.
//
// FaultNotice is computed here rather than left to the Edge because the
// threshold is Core's config and this is the same "server decides, client
// renders" rule the rest of the fault surfaces follow. It is almost always
// false on this push — nothing has been faulted for a minute at the instant it
// faults — which is why the threshold itself rides along: the Edge flips its
// own sentence as the clock passes it without owning the rule.
//
// A zero since means the faulted row could not be read. The clock fields are
// then omitted rather than sent as the zero time, which the Edge would render
// as a fault that started in year one.
func (e *Engine) attachFaultFields(update *protocol.OrderUpdate, ref protocol.TermRef, since time.Time) {
	noticeAfter := e.cfg.RDS.FaultNoticeAfter
	update.FaultNoticeAfterS = int(noticeAfter.Seconds())
	if !ref.Empty() {
		update.FaultRef = &ref
	}
	if since.IsZero() {
		return
	}
	deadline := since.Add(e.cfg.RDS.FaultGrace)
	update.FaultSince = &since
	update.FaultDeadline = &deadline
	update.FaultNotice = clock.Now().UTC().Sub(since) >= noticeAfter
}

// latestFaultReason reads back the order's most recent faulted row for the ref
// the fleet gave and the instant the fault started.
//
// The DB is the source rather than the poller's memory because the poller has
// dropped its deadline entry by the time grace expires (rds/poller.go), and
// because poller memory does not survive a Core restart. A read failure or a
// missing row degrades to an empty ref and a zero time: the sentence then says
// less, which is correct, rather than nothing, which would lose the transition.
func (e *Engine) latestFaultReason(orderID int64) (protocol.TermRef, time.Time) {
	h, err := e.db.LatestOrderHistoryForStatus(orderID, dispatch.StatusFaulted)
	if err != nil {
		e.logFn("engine: read faulted row for order %d: %v", orderID, err)
		return protocol.TermRef{}, time.Time{}
	}
	if h == nil {
		return protocol.TermRef{}, time.Time{}
	}
	var ref protocol.TermRef
	if h.Ref != nil {
		ref = *h.Ref
	}
	return ref, h.CreatedAt
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
	// Uncoded: the fleet stopping a vendor order reads as cancelled today and
	// every code that describes it honestly buckets as failed. See CancelCause.
	e.dispatcher.Lifecycle().CancelOrder(order, order.StationID, "fleet order stopped",
		dispatch.CancelCause{})
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
	// Carry the fault's own reason onto the terminal row. The order was faulted
	// for the whole grace window and the fleet may have said why at the start of
	// it; the poller dropped its deadline entry to fire this event, so the
	// faulted history row is the last place that reason exists. A failed order
	// that says "Gave up after 45m · cannot replan (60011)" answers the question
	// the old literal ("grace period expired without fleet recovery") only
	// restated.
	ref, since := e.latestFaultReason(order.ID)
	detail := protocol.FormatFaultSentence(
		protocol.FaultPhaseGaveUp, ref, since, clock.Now().UTC(), true)
	if err := e.dispatcher.Lifecycle().FailWithRef(order, order.StationID,
		string(protocol.TermGraceTimeout), detail, ref); err != nil {
		e.logFn("engine: grace-expiry fail order %d: %v", order.ID, err)
	}

	// Best-effort stop at the fleet vendor (the order is already locally
	// terminal). RDS may be unreachable; the local fail above stands regardless.
	if err := e.fleet.CancelOrder(order.VendorOrderID); err != nil {
		e.logFn("engine: terminate order %d (RDS %s): %v", order.ID, order.VendorOrderID, err)
	}
}
