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
	if ev.RobotID != "" && order.RobotID == "" && !order.StationUnaware {
		if err := e.sendToEdge(protocol.TypeOrderWaybill, order.Requester(), &protocol.OrderWaybill{
			OrderUUID: order.EdgeUUID,
			WaybillID: order.VendorOrderID,
			RobotID:   ev.RobotID,
		}); err != nil {
			e.logFn("engine: waybill: %v", err)
		}
	}

	newStatus := protocol.Status(e.fleet.MapState(ev.NewStatus))
	if newStatus == order.Status {
		// Idempotent path: status unchanged, check if robot ID changed.
		//
		// THE VENDOR STATE STILL MOVED, and it is recorded. A complex release
		// marks the order in_transit itself, so the fleet's WAITING -> RUNNING
		// that follows maps to the status the order already has — and this
		// return used to skip the write, leaving vendor_state WAITING for the
		// whole last leg of every staged order (SPR 6903, 2026-09-24).
		if ev.NewStatus != order.VendorState {
			if err := e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, effectiveRobotID); err != nil {
				e.logFn("engine: update order %d vendor state: %v", order.ID, err)
			}
			return
		}
		if effectiveRobotID != order.RobotID {
			if err := e.db.UpdateOrderRobotID(order.ID, effectiveRobotID); err != nil {
				e.logFn("engine: update order %d robot: %v", order.ID, err)
			}
		}
		return
	}

	// Vendor-state-domain mapping has already produced newStatus (a ShinGo
	// status). Route through the typed lifecycle method matching the
	// target ShinGo status. A terminal vendor state never reaches
	// MarkInTransit/MarkStaged: rds.OrderState.CoreStatus maps FINISHED to
	// delivered and STOPPED to cancelled. The vendor terminal checks
	// (e.fleet.IsTerminalState(ev.NewStatus), at the refusal and after the
	// station push below) only decide whether the post-mapping switch runs,
	// which is where Cancel/Fail are dispatched. This comment used to credit
	// the check, cited as "line 81 below", with keeping terminal states out
	// of MarkInTransit/MarkStaged; it never did that, the mapping does.
	lc := e.dispatcher.Lifecycle()

	// wasFaulted is captured BEFORE any transition runs: order.Status is the
	// pre-transition status here and the recovery branch is the only thing that
	// can tell a recovery from an ordinary transit row.
	now := clock.Now().UTC()
	wasFaulted := order.Status == dispatch.StatusFaulted
	// refused is the lifecycle turning down a non-terminal transition. Core then
	// has NOT moved the order, so nothing below may tell the station it did.
	var refused error

	// WHAT THE STATION HEARS BESIDE THE STATUS. The station is told by the status
	// write itself, in its transaction (store queueTellTx), so a refused write
	// tells nothing and an accepted one cannot go untold. This is the part only
	// the fleet's report holds: its state as the sentence, its robot (written
	// with the status, so the vendor write below finds it unchanged), the ETA
	// into in_transit (the route medians cache), the fault clock into faulted.
	tell := protocol.OrderUpdate{
		Detail:  fmt.Sprintf("fleet state: %s", ev.NewStatus),
		RobotID: effectiveRobotID,
	}
	if newStatus == dispatch.StatusInTransit {
		tell.ETA = eta.Stamp(e.etaCache, order.SourceNode, order.DeliveryNode)
	}

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
			if err := lc.MarkFaultedRecovered(order, effectiveRobotID, ref, reason, tell); err != nil {
				e.logFn("engine: mark recovered order %d: %v", order.ID, err)
				refused = err
			}
			break
		}
		if err := lc.MarkInTransit(order, effectiveRobotID, "fleet", tell); err != nil {
			e.logFn("engine: mark in_transit order %d: %v", order.ID, err)
			refused = err
		}
	case dispatch.StatusStaged:
		if err := lc.MarkStaged(order, "fleet", tell); err != nil {
			e.logFn("engine: mark staged order %d: %v", order.ID, err)
			refused = err
		}
	case dispatch.StatusDelivered:
		// Move bins to their destinations FIRST, then transition the order.
		// The lifecycle's MarkDelivered fires the fireCompleted action which
		// synchronously dispatches EventOrderCompleted to handleOrderCompleted,
		// whose completion net re-applies any cart still owed its drop. Calling
		// handleOrderDelivered first lands every cart at its row's drop step, so
		// the net's "already at destination" check finds nothing left to do.
		e.handleOrderDelivered(order)
		if err := lc.MarkDelivered(order, "fleet", tell); err != nil {
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
			if _, err := lc.ConfirmReceipt(order, order.Requester(), "auto_confirm_internal", 0); err != nil {
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
			refused = err
		}
	case dispatch.StatusDispatched:
		// The dispatcher writes dispatched itself after backend.CreateOrder
		// returns, and the fleet's CREATED / TOBEDISPATCHED that follow map
		// to it and take the same-status path above. Here Core is somewhere
		// else and the fleet has gone back to an unassigned state (an order
		// it re-dispatches after RUNNING): Core does not move, so this
		// REFUSES like the arms above. It used to log and fall through, and
		// the push below then told the station `dispatched` while Core held
		// in_transit, the very announcement the rule below forbids.
		// Pinned by TestVendorStatus_UnassignedAfterRunningTellsTheStationNothing.
		refused = fmt.Errorf("unexpected fleet-reported Dispatched, skipping")
	case dispatch.StatusFailed, dispatch.StatusCancelled:
		// Handled by the post-mapping switch below.
	case dispatch.StatusFaulted:
		// The fleet's reason has ridden ev.Snapshot through five layers and was
		// dropped by this call until 2026-08-22. It is written as "Replanning"
		// because it always is at the instant of faulting — the row's detail is
		// a record of what was true when it was written, and the live sentence
		// an operator reads is computed at read time against the threshold.
		faultRef := faultRefFrom(ev.Snapshot, order)
		reason := protocol.FormatFaultSentence(
			protocol.FaultPhaseLive, faultRef, now, now, false)
		// The Edge board shows the sentence to an operator. "fleet state: FAILED"
		// is the vendor's word for a state 97% of which recovers on its own in
		// 20 seconds, and it is also the word this design refuses to use about a
		// live order. The fault clock rides with it.
		tell.Detail = reason
		e.attachFaultFields(&tell, faultRef, now)
		if err := lc.MarkFaulted(order, effectiveRobotID, faultRef, reason, tell); err != nil {
			e.logFn("engine: mark faulted order %d: %v", order.ID, err)
			refused = err
		}
	default:
		// Unknown mapped status — should never fire under the current
		// seerrds adapter (MapState in fleet/seerrds/mappers.go produces
		// only the cases handled above). If it does fire, log and skip
		// rather than silently bypassing the state machine. A mapped
		// status outside the typed-method list signals an adapter
		// mismatch that needs investigation, not a generic DB write.
		//
		// And it is a refusal, like the arms above: the push below used to
		// tell the station a status Core never wrote (the simulator maps a
		// state it does not define to the word "unknown"). Pinned by
		// TestVendorStatus_UnmappedStateTellsTheStationNothing.
		e.logFn("engine: unknown mapped status %q for order %d (vendor=%q); skipping write — adapter MapState may be out of sync with dispatch.Status* constants", newStatus, order.ID, ev.NewStatus)
		refused = fmt.Errorf("unknown mapped status %q", newStatus)
	}
	if err := e.db.UpdateOrderVendor(order.ID, order.VendorOrderID, ev.NewStatus, effectiveRobotID); err != nil {
		e.logFn("engine: update order %d vendor state: %v", order.ID, err)
	}
	// DO NOT ANNOUNCE A STATUS CORE REFUSED. The station acts on what it is told:
	// told "staged" for an order Core still holds as dispatched, it offered a
	// release that Core then rejected on every tap (SPR order 7664, 2026-10-08).
	// The vendor state is recorded above; the order's own status stays what Core
	// has, and the station hears nothing it would have to unlearn.
	if refused != nil && !e.fleet.IsTerminalState(ev.NewStatus) {
		e.logFn("engine: order %d: fleet reports %s but Core refused %s -> %s; the station is not told (%v)",
			order.ID, ev.NewStatus, order.Status, newStatus, refused)
		return
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
		stationWait, waitKind := e.dispatcher.MarkStationWaitIfOwned(order.ID)
		// AND A LANE WAIT GETS RE-ASKED THE MOMENT THE ROBOT IS THERE.
		//
		// For an inbound dweller this is a cheap extra firing of a question the
		// lane's own events already drive. For an OUTBOUND one it is the primary
		// trigger: a dig leg standing in the lane it just lifted from is waiting for
		// Core to choose where the blocker goes, and that choice is deferred to
		// exactly this moment on purpose — it is what keeps the destination
		// uncommitted while the robot works the other lane.
		e.dispatcher.EvaluateWaitLaneForStagedOrder(order.ID)
		// AND A NODE THAT TURNS THE TABLE TURNS IT THE MOMENT THE ROBOT IS THERE:
		// one send, never a wait. Nothing at a node with no angle
		// (dispatch/table_turn_hold.go).
		if e.dispatcher.MayOweATableTurn(order) {
			e.dispatcher.RedriveTableTurns(order.ID)
		}
		if !order.StationUnaware {
			if err := e.sendToEdge(protocol.TypeOrderStaged, order.Requester(), &protocol.OrderStaged{
				OrderUUID:   order.EdgeUUID,
				Detail:      "robot dwelling at staging node",
				StationWait: stationWait,
				WaitKind:    waitKind,
			}); err != nil {
				e.logFn("engine: staged notification: %v", err)
			}
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
		e.handleFleetOrderCancelled(order, ev.Snapshot)
	}
}

// handleVendorRobotChange records a robot the fleet moved an order to without
// changing its state (rds.Poller reportRobotSwap): orders.robot_id, and a
// mission_events row so the swap is on the mission's timeline. The status is
// not touched, whatever the vendor state maps to: this event is not a
// transition (EventOrderRobotChanged).
//
// The station is told by the write: a change to a named robot queues the
// order.update in the robot write's own transaction (store UpdateOrderRobotID),
// with the order's status unchanged and its version bumped, so the robot on the
// station's tile is the one the fleet moved the order to.
//
// A report for a vendor order the Core order no longer runs (a redirect raced
// the poll) names another job's robot, and is dropped.
func (e *Engine) handleVendorRobotChange(ev OrderRobotChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for robot change: %v", ev.OrderID, err)
		return
	}
	if protocol.IsTerminal(order.Status) || ev.RobotID == "" || ev.RobotID == order.RobotID ||
		ev.VendorOrderID != order.VendorOrderID {
		return
	}
	if err := e.db.UpdateOrderRobotID(order.ID, ev.RobotID); err != nil {
		e.logFn("engine: update order %d robot: %v", order.ID, err)
		return
	}
	e.logFn("engine: order %d: the fleet moved it from robot %s to %s (%s)",
		order.ID, ev.FromRobotID, ev.RobotID, ev.VendorState)
	e.recordMissionEvent(OrderStatusChangedEvent{
		OrderID:       order.ID,
		VendorOrderID: ev.VendorOrderID,
		OldStatus:     ev.VendorState,
		NewStatus:     ev.VendorState,
		RobotID:       ev.RobotID,
		Detail:        fmt.Sprintf("fleet robot: %s -> %s", ev.FromRobotID, ev.RobotID),
	})
}

// faultRefFrom builds the history-row reference for a fault: where the order
// was, and what the fleet said about it.
//
// Node and Payload are filled EXPLICITLY rather than left to historyReason's
// default, which only fires when the whole ref is empty. A ref carrying just a
// vendor code would otherwise record why and silently lose where — and where is
// the field that has been carried on 87% of faulted rows all along.
//
// The NEWEST error is taken, by the same rule as a stop's reason
// (newestDistinctErrors): RDS keeps errors[] for the order's whole life, so a
// second fault on one order used to inherit the first fault's reason, and so did
// its grace-expiry sentence. It took Errors[0] until 2026-11-05, on the reading
// that live data has one entry; one entry is the case where both rules agree.
func faultRefFrom(snap *fleet.OrderSnapshot, ord *orders.Order) protocol.TermRef {
	ref := protocol.TermRef{Node: ord.ProcessNode, Payload: ord.PayloadCode}
	if ref.Node == "" {
		ref.Node = ord.DeliveryNode
	}
	if snap != nil {
		if errs := newestDistinctErrors(snap.Errors); len(errs) > 0 {
			ref.VendorCode = errs[0].Code
			ref.VendorDesc = errs[0].Desc
		}
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
	if err := e.dispatcher.Lifecycle().Fail(order, order.Requester(), "fleet_failed", detail); err != nil {
		e.logFn("engine: fail order %d: %v", order.ID, err)
	}
}

func (e *Engine) handleFleetOrderCancelled(order *orders.Order, snap *fleet.OrderSnapshot) {
	// lifecycle.CancelOrder handles fleet-cancel + atomic transition + emit.
	// PreviousStatus is captured by transition() before the status flip and
	// passed through to emitCancelled via the Event.
	// Uncoded: the fleet stopping a vendor order reads as cancelled today and
	// every code that describes it honestly buckets as failed. See CancelCause.
	//
	// The reason carries what the fleet said (fleetStopReason). It is the one
	// string that reaches orders.error_detail, the history row, the Edge's
	// OrderCancelled and — through swap_peer.go — the partner leg's sentence.
	e.dispatcher.Lifecycle().CancelOrder(order, order.Requester(), fleetStopReason(snap),
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
	if err := e.dispatcher.Lifecycle().FailWithRef(order, order.Requester(),
		string(protocol.TermGraceTimeout), detail, ref); err != nil {
		e.logFn("engine: grace-expiry fail order %d: %v", order.ID, err)
	}

	// Best-effort stop at the fleet vendor (the order is already locally
	// terminal). RDS may be unreachable; the local fail above stands regardless.
	if err := e.fleet.CancelOrder(order.VendorOrderID); err != nil {
		e.logFn("engine: terminate order %d (RDS %s): %v", order.ID, order.VendorOrderID, err)
	}
}
