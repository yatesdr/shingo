package messaging

import (
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/orders"
)

// EdgeHandler holds the order-channel reply handlers — the message
// types whose handling is intrinsic to Edge's orders.Manager and not
// composable from the composition root. Subject-channel (TypeData)
// dispatch is owned by a router.SubjectRouter wired in the composition
// root: cmd/shingoedge/main.go and shingoedge/testharness register the
// per-subject closures that need to capture engine/heartbeater/etc.
// state, rather than threading those references through this struct.
//
// Pre-router, this struct also held nine `onX func(...)` callback fields
// populated by nine SetXHandler setters — a field-pattern workaround for
// the init-ordering problem (handlers needed by Kafka subscription
// before engine subsystems they call into existed). Phase 3.4g deletes
// that pattern; SubjectRouter is now the registration surface.
type EdgeHandler struct {
	orderMgr *orders.Manager
	DebugLog DebugLogFunc
}

// NewEdgeHandler creates a handler for order-channel reply messages.
// All subject-channel dispatch lives on the SubjectRouter wired
// alongside this handler at the composition root.
func NewEdgeHandler(orderMgr *orders.Manager) *EdgeHandler {
	return &EdgeHandler{orderMgr: orderMgr}
}

func (h *EdgeHandler) HandleOrderAck(env *protocol.Envelope, p *protocol.OrderAck) {
	h.DebugLog.Log("order_ack uuid=%s shingo_id=%d", p.OrderUUID, p.ShingoOrderID)
	log.Printf("edge_handler: order ack: uuid=%s shingo_id=%d", p.OrderUUID, p.ShingoOrderID)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyAck, "", "", p.SourceNode); err != nil {
		log.Printf("edge_handler: handle ack for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderWaybill(env *protocol.Envelope, p *protocol.OrderWaybill) {
	h.DebugLog.Log("order_waybill uuid=%s waybill=%s", p.OrderUUID, p.WaybillID)
	log.Printf("edge_handler: order waybill: uuid=%s waybill=%s", p.OrderUUID, p.WaybillID)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyWaybill, p.WaybillID, p.ETA, ""); err != nil {
		log.Printf("edge_handler: handle waybill for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderUpdate(env *protocol.Envelope, p *protocol.OrderUpdate) {
	h.DebugLog.Log("order_update uuid=%s status=%s", p.OrderUUID, p.Status)
	log.Printf("edge_handler: order update: uuid=%s status=%s", p.OrderUUID, p.Status)

	// ETA side-write (into in_transit Core stamps an ETA on the update; Edge
	// stores it and the HMI renders an ETA pill). Preserved exactly from the
	// legacy ReplyUpdate arm — independent of the status write below.
	if p.ETA != "" {
		if err := h.orderMgr.SetOrderETA(p.OrderUUID, p.ETA); err != nil {
			log.Printf("edge_handler: set eta for %s: %v", p.OrderUUID, err)
		}
	}

	// Apply Core's pushed status to the Edge row through the ApplyCoreStatus
	// mapping shared with boot reconciliation, instead of the legacy "branch on
	// queued, discard everything else" — so sourcing/dispatched/faulted pushes
	// now update the Edge row instead of leaving a stale acknowledged rendering
	// as "IN TRANSIT". staged/delivered/terminal are no-ops here (dedicated
	// envelopes own them); a push that isn't reachable from the current status
	// returns an error that is logged and swallowed, matching the old discard
	// behavior.
	if err := h.orderMgr.HandleCoreStatusPush(p.OrderUUID, protocol.Status(p.Status), p.Detail); err != nil {
		log.Printf("edge_handler: apply core status %s for %s: %v", p.Status, p.OrderUUID, err)
	}

	if p.QueueReason != "" {
		if err := h.orderMgr.SetOrderQueueReason(p.OrderUUID, p.QueueReason, p.QueueCode); err != nil {
			log.Printf("edge_handler: set queue_reason for %s: %v", p.OrderUUID, err)
		}
	}
}

func (h *EdgeHandler) HandleOrderDelivered(env *protocol.Envelope, p *protocol.OrderDelivered) {
	h.DebugLog.Log("order_delivered uuid=%s at=%s", p.OrderUUID, p.DeliveredAt)
	log.Printf("edge_handler: order delivered: uuid=%s at=%s", p.OrderUUID, p.DeliveredAt)
	if err := h.orderMgr.HandleDeliveredWithExpiry(p.OrderUUID, p.DeliveredAt.Format(time.RFC3339), p.StagedExpireAt, p.BinID, p.UOPRemaining, p.DeltaEpoch, p.DeliveryNode); err != nil {
		log.Printf("edge_handler: handle delivered for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderError(env *protocol.Envelope, p *protocol.OrderError) {
	h.DebugLog.Log("order_error uuid=%s code=%s", p.OrderUUID, p.ErrorCode)
	log.Printf("edge_handler: order error: uuid=%s code=%s detail=%s", p.OrderUUID, p.ErrorCode, p.Detail)

	// Recoverable error: Core couldn't sync the bin manifest at release time
	// (claim mismatch, locked bin, transient DB issue). The bin is still in
	// the same physical state; the operator can fix the underlying issue
	// and click release again. Roll the order back to StatusStaged with a
	// friendly detail so it reappears in the active order list with a
	// "release error" chip rather than disappearing into the failed pile.
	if p.ErrorCode == "manifest_sync_failed" {
		detail := "Manifest sync failed at Core: " + p.Detail + ". Click release to retry."
		if err := h.orderMgr.RollbackForRetry(p.OrderUUID, detail); err != nil {
			log.Printf("edge_handler: rollback for retry %s: %v", p.OrderUUID, err)
		}
		return
	}

	// Recoverable error: Core rejected a release precondition (invalid_state) —
	// e.g. the consolidated two-robot release fan-out reached a leg that's
	// faulted/recovering or already finished. A release rejection must never
	// terminally kill the Edge mirror (the ALN_003 divergence): roll an
	// in_transit leg back to staged for a retry, and ignore the rejection for a
	// terminal/pre-release leg. (Core's A1 fix stops emitting this for the
	// faulted case; this is defense-in-depth for the rest.)
	if p.ErrorCode == "invalid_state" {
		detail := "Core rejected the release: " + p.Detail + ". Click release to retry."
		if err := h.orderMgr.RollbackReleaseRejection(p.OrderUUID, detail); err != nil {
			log.Printf("edge_handler: rollback release rejection %s: %v", p.OrderUUID, err)
		}
		return
	}

	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyError, "", "", p.Detail); err != nil {
		log.Printf("edge_handler: handle error for %s: %v", p.OrderUUID, err)
	}
}

// HandleOrderSkipped processes Core's "the work was never needed" terminal
// notification. Today's sole producer is DispatchPreparedComplex's
// no_source_bin path — a complex evac order whose source nodes were
// emptied externally before dispatch (e.g. operator pulled the bin to
// quality hold). The handler:
//
//  1. Transitions the local order row to StatusSkipped (terminal, distinct
//     from Failed — same atomic-write semantics on Core).
//  2. Looks up the linked changeover_node_tasks row and advances its state
//     to the same completion state a successful run would have produced
//     (line_cleared for evac, released for supply). This keeps the
//     changeover state machine progressing without operator intervention.
//  3. Records the operator-facing reason on the node task's skip_note so
//     the HMI surfaces a "bin missing, manual recovery if needed" chip
//     instead of a sticky red error toast.
//
// Idempotent — duplicate skip notifications for an already-skipped order
// land on a terminal row, the HandleDispatchReply path is no-op-safe, and
// the node-task updates are last-writer-wins on the same row.
func (h *EdgeHandler) HandleOrderSkipped(env *protocol.Envelope, p *protocol.OrderSkipped) {
	h.DebugLog.Log("order_skipped uuid=%s code=%s", p.OrderUUID, p.ErrorCode)
	log.Printf("edge_handler: order skipped: uuid=%s code=%s detail=%s", p.OrderUUID, p.ErrorCode, p.Detail)

	if err := h.orderMgr.HandleSkipped(p.OrderUUID, p.ErrorCode, p.Detail); err != nil {
		log.Printf("edge_handler: handle skipped for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderCancelled(env *protocol.Envelope, p *protocol.OrderCancelled) {
	h.DebugLog.Log("order_cancelled uuid=%s reason=%s", p.OrderUUID, p.Reason)
	log.Printf("edge_handler: order cancelled: uuid=%s reason=%s", p.OrderUUID, p.Reason)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyCancelled, "", "", p.Reason); err != nil {
		log.Printf("edge_handler: handle cancelled for %s: %v", p.OrderUUID, err)
	}
}

func (h *EdgeHandler) HandleOrderStaged(env *protocol.Envelope, p *protocol.OrderStaged) {
	h.DebugLog.Log("order_staged uuid=%s detail=%s", p.OrderUUID, p.Detail)
	log.Printf("edge_handler: order staged: uuid=%s detail=%s", p.OrderUUID, p.Detail)
	if err := h.orderMgr.HandleDispatchReply(p.OrderUUID, orders.ReplyStaged, "", "", p.Detail); err != nil {
		log.Printf("edge_handler: handle staged for %s: %v", p.OrderUUID, err)
	}
}
