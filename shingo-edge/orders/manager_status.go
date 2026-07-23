package orders

import (
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
func (m *Manager) HandleDeliveredWithExpiry(orderUUID, statusDetail string, stagedExpireAt *time.Time, binID *int64, uop *int, epoch int64, deliveryNode string) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		// Core-admin order — no Edge row. Emit a bind-only fallback event so the
		// runtime cache updates if the delivery node maps to an Edge process node.
		if binID != nil && deliveryNode != "" {
			m.emitter.EmitOrderDeliveredFallback(*binID, uop, epoch, deliveryNode)
		}
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}
	return m.handleDelivered(order, statusDetail, stagedExpireAt, binID, uop, epoch)
}

func (m *Manager) handleDelivered(order *orders.Order, statusDetail string, stagedExpireAt *time.Time, binID *int64, uop *int, epoch int64) error {
	if err := m.lifecycle.HandleDelivered(order, statusDetail, stagedExpireAt, binID, uop, epoch); err != nil {
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
		return m.TransitionOrder(order.ID, StatusAcknowledged, statusDetail)
	case ReplyWaybill:
		if err := m.db.UpdateOrderWaybill(order.ID, waybillID, eta); err != nil {
			return err
		}
		return m.TransitionOrder(order.ID, StatusInTransit, fmt.Sprintf("waybill %s, ETA %s", waybillID, eta))
	case ReplyQueued:
		// Order queued by Core — awaiting inventory
		return m.TransitionOrder(order.ID, StatusQueued, statusDetail)
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
		// OrderDelivered envelope); pass nil/0 so Edge uses the role default.
		return m.handleDelivered(order, statusDetail, nil, nil, nil, 0)
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
		return m.TransitionOrder(order.ID, StatusStaged, statusDetail)
	case ReplyCancelled:
		return m.TransitionOrder(order.ID, StatusCancelled, statusDetail)
	default:
		return fmt.Errorf("unknown reply type: %s", replyType)
	}
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
