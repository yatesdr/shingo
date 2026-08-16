package orders

import (
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// HandleDeliveredWithExpiry processes a delivered reply with optional staged
// expiry. binID captures Core's bin id at delivery so the PLC tick path can
// attribute deltas to the right bin; nil for multi-bin orders. uop+epoch are
// Core's snapshot of that bin at delivery (from the OrderDelivered envelope)
// — Edge seeds its runtime cache + active_bin_epoch from them so tick deltas
// carry the right count baseline and load-lifecycle generation, with no
// separate HTTP pull. uop nil = older Core didn't send it; Edge falls back to
// its role default (see wiring_delivered.go).
//
// deliveryNode is the Core dot-name of the destination, forwarded from the
// OrderDelivered protocol message. When the order isn't found by UUID (Core-
// admin manual orders have no Edge row), a fallback bind event is emitted using
// deliveryNode so the runtime cache can still be updated.
func (m *Manager) HandleDeliveredWithExpiry(orderUUID, statusDetail string, stagedExpireAt *time.Time, binID *int64, uop *int, epoch int64, deliveryNode, binDestNode string) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		// Core-admin order — no Edge row. Emit a bind-only fallback event so the
		// runtime cache updates if the delivery node maps to an Edge process node.
		if binID != nil && deliveryNode != "" {
			m.emitter.EmitOrderDeliveredFallback(*binID, uop, epoch, deliveryNode)
		}
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}
	return m.handleDelivered(order, statusDetail, stagedExpireAt, binID, uop, epoch, binDestNode)
}

func (m *Manager) handleDelivered(order *orders.Order, statusDetail string, stagedExpireAt *time.Time, binID *int64, uop *int, epoch int64, binDestNode string) error {
	if err := m.lifecycle.HandleDelivered(order, statusDetail, stagedExpireAt, binID, uop, epoch, binDestNode); err != nil {
		return err
	}
	if order.AutoConfirm {
		m.DebugLog.Log("auto-confirm: id=%d uuid=%s qty=%d", order.ID, order.UUID, order.Quantity)
		return m.ConfirmDelivery(order.ID, order.Quantity)
	}
	return nil
}

// HandleDispatchReply processes an inbound reply from central dispatch.
func (m *Manager) HandleDispatchReply(orderUUID, replyType, waybillID, eta, statusDetail string) error {
	m.DebugLog.Log("dispatch reply: uuid=%s type=%s", orderUUID, replyType)
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}

	switch replyType {
	case ReplyAck:
		return m.mirrorTransition(order.ID, StatusAcknowledged, statusDetail)
	case ReplyWaybill:
		if err := m.db.UpdateOrderWaybill(order.ID, waybillID, eta); err != nil {
			return err
		}
		return m.mirrorTransition(order.ID, StatusInTransit, fmt.Sprintf("waybill %s, ETA %s", waybillID, eta))
	case ReplyQueued:
		// Order queued by Core — awaiting inventory
		return m.mirrorTransition(order.ID, StatusQueued, statusDetail)
	case ReplyUpdate:
		// Status update with ETA only — don't touch waybill_id.
		if eta != "" {
			if err := m.db.UpdateOrderETA(order.ID, eta); err != nil {
				return err
			}
		}
		return nil
	case ReplyDelivered:
		// Dispatch-reply delivery carries no bin snapshot (that rides the
		// OrderDelivered envelope); pass nil/0/"" so Edge uses the role default.
		return m.handleDelivered(order, statusDetail, nil, nil, nil, 0, "")
	case ReplyError:
		return m.TransitionOrder(order.ID, StatusFailed, statusDetail)
	case ReplySkipped:
		// "Work was never needed" terminal — distinct from ReplyError.
		// The post-skip cleanup (advancing a linked changeover node task
		// and writing the operator-facing note) happens in the edge_handler
		// HandleOrderSkipped path before this; here we only persist the
		// order's local status.
		return m.TransitionOrder(order.ID, StatusSkipped, statusDetail)
	case ReplyStaged:
		return m.mirrorTransition(order.ID, StatusStaged, statusDetail)
	case ReplyCancelled:
		return m.mirrorTransition(order.ID, StatusCancelled, statusDetail)
	default:
		return fmt.Errorf("unknown reply type: %s", replyType)
	}
}

// mirrorJumpDetail prefixes the order_history detail written when the mirror had
// to jump. It is the string a count is taken on — `SELECT count(*) FROM
// order_history WHERE detail LIKE 'MIRROR JUMP%'` — so it is a constant rather
// than prose assembled at each site.
const mirrorJumpDetail = "MIRROR JUMP"

// expectedMirrorJumps are the forward jumps Core NEVER PROMISED TO ANNOUNCE, so
// the mirror catching up across them is the design working rather than a defect
// to go find. Each entry names the pure transition being skipped and why its
// silence is intended — at the transition, where the next reader will look.
//
// ── WHY THIS LIST HAD TO EXIST ────────────────────────────────────────────
//
// The jump row is a TICKET QUEUE: a non-zero count is supposed to mean "some
// transition is still not notifying, and the detail says which one". A soak
// produced 31 of them — 28 pending->acknowledged and 3 sourcing->in_transit —
// and every one was a transition that is silent BY CONSTRUCTION. A queue that
// is permanently non-zero for expected reasons stops being readable, and then a
// real gap arrives and nobody looks (standing law 9: a net that always fires
// means as little as one that never does).
//
// The judgment, per transition:
//
//	pending -> acknowledged (x28). The step between is pending->queued, which
//	  has no actionMap entry. dispatch/lifecycle.go's transition() applies ONLY
//	  actionMap actions — there is no general per-status push — and that map's
//	  own header states pure transitions (status write + audit, no side effects)
//	  deliberately have none. `queued` is an internal scheduling state Core
//	  passes through on its way to acknowledging; the Edge learns the outcome,
//	  not the scheduling. Nothing durable is wrong at the Edge afterwards.
//
//	sourcing -> in_transit (x3). Same shape, one state along: the step between
//	  is sourcing->dispatched (or ->queued), equally pure. Dispatch is Core
//	  handing the job to the fleet; the Edge's board cares that the robot is
//	  moving, which is exactly the status it receives.
//
// NOT a licence to silence future jumps. A jump that is NOT on this list still
// tickets, loudly, with the instruction to find the transition that does not
// notify — which is the whole reason the queue exists. Adding an entry here is a
// claim that Core intends the silence, and it has to be argued at the entry.
//
// The counterexample is in the same file's neighbourhood and worth keeping in
// view: {Reshuffling, Queued} DOES carry fireResumed, because there the Edge was
// stuck AT reshuffling and would never have moved again. Silence is expected on
// a state Core passes THROUGH, not on one the Edge is parked in.
var expectedMirrorJumps = map[[2]protocol.Status]string{
	{protocol.StatusPending, protocol.StatusAcknowledged}: "the step between is pending->queued, a pure transition with no actionMap entry: `queued` is Core's internal scheduling state, and the Edge is told the outcome rather than the scheduling",
	{protocol.StatusSourcing, protocol.StatusInTransit}:   "the step between is sourcing->dispatched, a pure transition with no actionMap entry: dispatch is Core handing the job to the fleet, and the Edge's board cares that the robot is moving",
}

// mirrorTransition applies a status CORE has already reached.
//
// ── THE MIRROR FOLLOWS THE AUTHORITY, AND SAYS SO WHEN IT HAD TO CATCH UP ──
//
// Every caller of this is a Core-authored report: an ack, a waybill, a staged, a
// cancel. Core owns order state and the Edge reflects it — ApplyCoreStatus has
// said exactly that for its own arms since the Springfield 2026-07-31 refusals
// ("a mirror does not validate its source"). The dispatch-reply path never got
// the same treatment: it validated, so a single missed notification left the
// Edge behind the authority permanently, refusing every later push as illegal.
//
// Measured: Core walked reshuffling → queued → … → staged; the Edge, never told
// about `queued`, rejected both in_transit and staged and held three robots for
// an entire soak with a board that could not offer Release.
//
// SO A FORWARD JUMP IS ACCEPTED AND RECORDED, NOT SWALLOWED. Reachable-but-not-
// adjacent means "I missed a message", and the history row names the gap so the
// count is a ticket queue: a non-zero trend means a transition somewhere is
// still not notifying, and the detail says which one to go find.
//
// BACKWARD AND IMPOSSIBLE STAY REFUSED. Nothing reachable from a terminal, so a
// terminal is never resurrected; and a status Core could not have arrived at is
// a bug in the sender, not a gap in the wire. Strictness is kept exactly where
// it still means something.
func (m *Manager) mirrorTransition(orderID int64, target protocol.Status, detail string) error {
	err := m.TransitionOrder(orderID, target, detail)
	if err == nil || !errors.Is(err, ErrInvalidTransition) {
		return err
	}
	order, gErr := m.db.GetOrder(orderID)
	if gErr != nil || order == nil {
		return err
	}
	if !protocol.IsForwardJump(order.Status, target) {
		return err // backward, or somewhere Core could not have got to
	}
	// EXPECTED JUMPS CATCH UP THE SAME WAY AND DO NOT TICKET. The mirror still
	// moves in one step — that behaviour is unchanged and is what keeps the Edge
	// from wedging — but the history row is an ordinary catch-up rather than a
	// defect naming a transition to go fix. See expectedMirrorJumps for the
	// per-transition argument.
	if why, expected := expectedMirrorJumps[[2]protocol.Status{order.Status, target}]; expected {
		caught := fmt.Sprintf("mirror caught up %s->%s (expected: %s). %s",
			order.Status, target, why, detail)
		m.DebugLog.Log("orders: mirror caught up order %d %s->%s — expected silence: %s",
			orderID, order.Status, target, why)
		return m.lifecycle.ForceTransition(orderID, target, caught)
	}
	jump := fmt.Sprintf("%s %s->%s: a notification was skipped; Core is already at %s. %s",
		mirrorJumpDetail, order.Status, target, target, detail)
	log.Printf("orders: MIRROR JUMP order %d %s->%s — the Edge was never told about the step(s) "+
		"between, so it is catching up to Core in one move. Find the transition that does not "+
		"notify: %s", orderID, order.Status, target, detail)
	return m.lifecycle.ForceTransition(orderID, target, jump)
}

// ApplyCoreStatusSnapshot reconciles a local order with Core's authoritative status.
func (m *Manager) ApplyCoreStatusSnapshot(snapshot protocol.OrderStatusSnapshot) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ApplyCoreStatusSnapshot(snapshot)
}

// ApplyCoreStatus is the Core→Edge status mapping — the one function used by
// both the live-push path (HandleCoreStatusPush, driven by HandleOrderUpdate)
// and the boot-reconcile path (ApplyCoreStatusSnapshot). See
// LifecycleService.ApplyCoreStatus for the arm-by-arm mapping.
func (m *Manager) ApplyCoreStatus(order *orders.Order, coreStatus protocol.Status, detail string) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ApplyCoreStatus(order, coreStatus, detail)
}

// HandleCoreStatusPush is the live-channel entry point for the total Core→Edge
// status mapping. edge_handler.HandleOrderUpdate calls this with Core's pushed
// status string (after handling the queued branch and ETA side-write). It
// replaces the legacy "branch on queued, discard everything else" behavior.
func (m *Manager) HandleCoreStatusPush(orderUUID string, coreStatus protocol.Status, detail string) error {
	m.DebugLog.Log("core status push: uuid=%s status=%s", orderUUID, coreStatus)
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}
	return m.ApplyCoreStatus(order, coreStatus, detail)
}

// HandleSkipped processes Core's terminal "the work was never needed"
// notification for an order. Today the sole producer is the complex-order
// dispatcher's no_source_bin path (every pickup node was genuinely empty,
// e.g. evac for a bin that was pulled to quality hold before dispatch).
//
// Three-step write, in order:
//
//  1. Transition the local order row to StatusSkipped via the standard
//     dispatch-reply path — keeps lifecycle audit consistent with every
//     other terminal reply.
//  2. Look up the changeover_node_tasks row linked to this order (either
//     leg). If found, advance its state to the post-completion state a
//     successful run would have produced — line_cleared for an evac leg,
//     released for a supply leg. This unsticks the changeover state
//     machine without requiring operator intervention.
//  3. Write skip_note on the same node task so the HMI surfaces a chip
//     ("evac skipped: bin missing — recover manually if needed") instead
//     of a sticky red error toast.
//
// Idempotent: a duplicate skip on an already-skipped order lands on a
// terminal row (TransitionOrder no-op) and the node-task updates are
// last-writer-wins on the same row.
func (m *Manager) HandleSkipped(orderUUID, errorCode, detail string) error {
	m.DebugLog.Log("dispatch reply: uuid=%s type=skipped code=%s", orderUUID, errorCode)
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}
	// Force-transition rather than validate against the state machine.
	// Core is authoritative on Skipped: its planner decided the work was
	// never needed and the order will never dispatch to the fleet. Edge's
	// local order may have already advanced past the protocol's allowed
	// "skippable" pre-set (e.g. Acknowledged) due to event-ordering races
	// between OrderAck and OrderSkipped, leaving the validated transition
	// rejected and the HMI stuck on Acknowledged while Core shows Skipped
	// (plant 2026-05-12, ALN_002). The protocol intentionally disallows
	// Acknowledged→Skipped for Edge-initiated transitions (don't let a
	// stale client drop in-flight work) but Core-driven skip is an
	// authority override. Log loudly so we have an audit trail.
	if order.Status != StatusSkipped && !IsValidTransition(order.Status, StatusSkipped) {
		log.Printf("orders: core-driven skip overriding local status: uuid=%s old=%s -> skipped detail=%q",
			order.UUID, order.Status, detail)
	}
	m.lifecycle.debug = m.DebugLog
	if err := m.lifecycle.ForceTransition(order.ID, StatusSkipped, detail); err != nil {
		return err
	}
	task, _, terr := m.db.FindChangeoverNodeTaskByOrderID(order.ID)
	if terr != nil || task == nil {
		// Not a changeover-linked order — nothing more to advance.
		return nil
	}
	postState := skippedTerminalState(task, order.ID)
	if err := m.db.UpdateChangeoverNodeTaskState(task.ID, postState); err != nil {
		log.Printf("orders: advance node task %d to %s on skip: %v", task.ID, postState, err)
	}
	note := formatSkipNote(task, errorCode, detail)
	if err := m.db.SetChangeoverNodeTaskSkipNote(task.ID, note); err != nil {
		log.Printf("orders: set skip_note on node task %d: %v", task.ID, err)
	}
	return nil
}

// skippedTerminalState picks the post-completion node-task state that a
// successful run of the skipped order would have produced. Mirrors the
// completion-handler shape in wiring_completion.go:
//
//   - evac leg (OldMaterialReleaseOrderID == orderID): line_cleared
//   - supply leg (NextMaterialOrderID == orderID): released
//   - neither matches (shouldn't happen — FindChangeoverNodeTaskByOrderID
//     OR-matches): default to released to keep the state machine moving.
func skippedTerminalState(task *processes.NodeTask, orderID int64) domain.NodeTaskState {
	if task.OldMaterialReleaseOrderID != nil && *task.OldMaterialReleaseOrderID == orderID {
		return domain.NodeTaskLineCleared
	}
	return domain.NodeTaskReleased
}

// formatSkipNote builds the operator-facing chip text. Keep it short —
// the HMI renders it on a small chip; the full Detail string is logged
// elsewhere for forensics.
func formatSkipNote(task *processes.NodeTask, errorCode, detail string) string {
	leg := "order"
	if task.OldMaterialReleaseOrderID != nil {
		leg = "evac"
	} else if task.NextMaterialOrderID != nil {
		leg = "supply"
	}
	if errorCode == "no_source_bin" {
		return leg + " skipped: bin missing at " + task.NodeName
	}
	return leg + " skipped: " + detail
}
