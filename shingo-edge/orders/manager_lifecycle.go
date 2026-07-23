package orders

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/orders"
)

// ReleaseOrder sends a release message for a staged (dwelling) order.
//
// remainingUOP late-binds the bin's manifest at Core's release handler. Pass
// nil when no manifest change is intended (legacy/Order-A/produce paths). Pass
// &0 to mark the bin empty (NOTHING PULLED disposition). Pass &N (N>0) to
// preserve the manifest with a synced count (SEND PARTIAL BACK disposition).
// See protocol.OrderRelease and BinManifestService.SyncOrClearForReleased.
//
// calledBy carries the operator identity through to Core's bin audit so the
// "who released this bin" question is answerable from Core's audit_log
// table. Empty for system/internal paths (wiring fallbacks, restore); Core
// substitutes "system" in that case.
//
// Thin wrapper that ships no Disposition — used by every fallback / early-
// return release path. Callers that have the structured disposition (the
// main ReleaseOrderWithLineside path) call ReleaseOrderWithDisposition
// directly so Core gets the override-audit context.
func (m *Manager) ReleaseOrder(orderID int64, remainingUOP *int, calledBy string) error {
	return m.ReleaseOrderWithDisposition(orderID, remainingUOP, nil, calledBy)
}

// ReleaseOrderWithDisposition is the Phase 0b release path that carries
// the structured UOPDisposition (kind + operator-submitted vs system-
// suggested values) alongside the legacy RemainingUOP pointer. Core's
// HandleOrderRelease uses RemainingUOP for the manifest sync (unchanged
// behavior); Disposition.CountSuggested / CapturesSuggested drive the
// override audit log.
//
// disposition may be nil — callers without an override-aware body
// (legacy fallback paths) ship only the legacy pointer.
func (m *Manager) ReleaseOrderWithDisposition(orderID int64, remainingUOP *int, disposition *protocol.UOPDisposition, calledBy string) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	// Pre-2026-04-27 this required order.Status == StatusStaged. Relaxed to
	// "non-terminal" because the simplified consolidated-release path
	// (ReleaseStagedOrders, see operator_stations.go) fans out to both legs
	// of a two-robot swap regardless of where each leg is in its choreography.
	// The transition to in_transit below is idempotent (applyTransition no-ops
	// when old == new), and Core's HandleOrderRelease only needs the order to
	// have been dispatched (have a VendorOrderID) — which is true once any
	// status past "acknowledged" is reached. See shingo_todo.md for the
	// pre-dispatch edge case worth eventually guarding.
	if IsTerminal(order.Status) {
		return fmt.Errorf("order is terminal (%s), cannot release", order.Status)
	}
	// Pre-dispatch guard: if Core hasn't acknowledged the order yet (no
	// VendorOrderID, status is pending/submitted), there's nothing to release
	// against. Core's HandleOrderRelease would fail trying to send blocks to
	// an empty VendorOrderID. In practice this can't happen during a real
	// two-robot swap (Robot B reaching staged means both orders dispatched),
	// but the consolidated release fans out unconditionally so we guard here.
	// Silent no-op: log and return nil so the consolidated call site doesn't
	// abort over a pre-dispatch sibling. See shingo_todo.md.
	if order.Status == StatusPending || order.Status == StatusSubmitted {
		m.DebugLog.Log("release: id=%d uuid=%s status=%q is pre-dispatch — skipping (no VendorOrderID for Core to release against)",
			orderID, order.UUID, order.Status)
		return nil
	}

	if err := m.sender.Queue(protocol.TypeOrderRelease, &protocol.OrderRelease{
		OrderUUID:    order.UUID,
		RemainingUOP: remainingUOP,
		Disposition:  disposition,
		CalledBy:     calledBy,
	}); err != nil {
		return fmt.Errorf("enqueue release: %w", err)
	}

	// Transition Edge status to in_transit now that the robot is resuming.
	// Core won't send a dedicated in_transit message (TypeOrderUpdate ignores
	// status), so we transition locally to keep Edge in sync.
	if err := m.TransitionOrder(orderID, StatusInTransit, "released from staging"); err != nil {
		return fmt.Errorf("transition to in_transit: %w", err)
	}

	// Single log shape regardless of nil-ness — keeps log-parsing tools
	// from having to handle two different formats for the same event.
	// Nil prints as "<nil>" via %v.
	m.DebugLog.Log("release: id=%d uuid=%s remaining_uop=%v disposition=%v called_by=%q",
		orderID, order.UUID, remainingUOP, disposition, calledBy)
	return nil
}

// TransitionOrder moves an order to a new status with validation.
func (m *Manager) TransitionOrder(orderID int64, newStatus protocol.Status, detail string) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.Transition(orderID, newStatus, detail)
}

// SetOrderQueueReason persists Core's blocking reason and its structured code
// for a queued order. Called from the edge handler when an OrderUpdate (or boot
// snapshot) carries QueueReason + QueueCode fields.
//
// A waiting_for_material code on a changeover SUPPLY order additionally stamps
// the owning node task awaiting_material — the C(ii) park made visible. The
// stamp rides the same push (and the boot snapshot) so a restart re-derives it.
func (m *Manager) SetOrderQueueReason(uuid, reason, code string) error {
	if err := m.db.SetOrderQueueReason(uuid, reason, code); err != nil {
		return err
	}
	if code == string(protocol.QueueWaitingForMaterial) {
		m.stampAwaitingMaterial(uuid)
	}
	return nil
}

// stampAwaitingMaterial moves a changeover supply leg's node task to
// awaiting_material when Core parks the supply order for lack of material at
// its node-local pool. Best-effort: a miss here only costs visibility (the
// order itself is already parked), so every bail-out is silent or logged, never
// an error to the push path.
//
// Guards: supply leg only (NextMaterialOrderID — an evac never parks for
// material), changeover still active, task not already terminal for its
// situation. The task does NOT revert when the order un-parks; it is genuinely
// "waiting for material to arrive at the line" until the staged-delivery writer
// advances it, and the abandon path re-checks the order's live status anyway.
func (m *Manager) stampAwaitingMaterial(uuid string) {
	order, err := m.db.GetOrderByUUID(uuid)
	if err != nil || order == nil {
		return
	}
	task, coState, terr := m.db.FindChangeoverNodeTaskByOrderID(order.ID)
	if terr != nil || task == nil {
		return
	}
	if task.NextMaterialOrderID == nil || *task.NextMaterialOrderID != order.ID {
		return
	}
	if coState.IsTerminal() {
		return
	}
	if task.State == domain.NodeTaskAwaitingMaterial || task.State.IsTerminal(task.Situation) {
		return
	}
	if err := m.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskAwaitingMaterial); err != nil {
		log.Printf("orders: stamp node task %d awaiting_material: %v", task.ID, err)
		return
	}
	m.DebugLog.Log("changeover: node task %d (%s) -> awaiting_material (supply order %d parked by core)",
		task.ID, task.NodeName, order.ID)
}

// SetOrderETA persists Core's ETA stamp for an order. Called from the edge
// handler when an OrderUpdate carries an ETA field (Core stamps it on
// transitions into in_transit). Independent of the status write so the HMI's
// ETA pill can update even when the status push is a no-op in the mapping.
func (m *Manager) SetOrderETA(uuid, eta string) error {
	order, err := m.db.GetOrderByUUID(uuid)
	if err != nil {
		return err
	}
	return m.db.UpdateOrderETA(order.ID, eta)
}

// AbortOrder cancels a non-terminal order and enqueues a cancel message.
// The cancel message is enqueued BEFORE the local transition so that Core
// is guaranteed to receive the cancellation — preventing a robot from
// continuing to execute a cancelled order on the floor.
func (m *Manager) AbortOrder(orderID int64) error {
	return m.AbortOrderWithReason(orderID, "aborted by operator")
}

// AbortOrderWithReason is AbortOrder with a caller-chosen cancel reason. The
// reason travels in the OrderCancel envelope and Core keys behavior on it —
// protocol.CancelReasonAcceptHalfSwap tells Core's swap-peer arm to leave the
// partner leg alone (the accepted half-swap) where any other reason cascades
// the cancel. It is also the local transition detail, so the HMI shows it.
func (m *Manager) AbortOrderWithReason(orderID int64, reason string) error {
	m.DebugLog.Log("abort: id=%d reason=%q", orderID, reason)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	// Build and enqueue cancel message BEFORE transitioning locally.
	// If enqueue fails, the order stays in its current state so the
	// operator can retry rather than having a locally-cancelled order
	// with a robot still executing on the floor.
	if err := m.sender.Queue(protocol.TypeOrderCancel, &protocol.OrderCancel{
		OrderUUID: order.UUID,
		Reason:    reason,
	}); err != nil {
		return fmt.Errorf("enqueue cancel message: %w", err)
	}

	if err := m.TransitionOrder(orderID, StatusCancelled, reason); err != nil {
		return err
	}
	return nil
}

// RedirectOrder changes the delivery node of a non-terminal order and enqueues a redirect message.
// The envelope is built and enqueued before updating the local DB so that
// Core receives the redirect. If enqueue fails, the error is returned.
func (m *Manager) RedirectOrder(orderID int64, newDeliveryNode string) (*orders.Order, error) {
	m.DebugLog.Log("redirect: id=%d new_delivery=%s", orderID, newDeliveryNode)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return nil, fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	// Build and enqueue redirect message first. If this fails, don't
	// update local state — the operator can retry.
	if err := m.sender.Queue(protocol.TypeOrderRedirect, &protocol.OrderRedirect{
		OrderUUID:       order.UUID,
		NewDeliveryNode: newDeliveryNode,
	}); err != nil {
		return nil, fmt.Errorf("enqueue redirect: %w", err)
	}

	if err := m.db.UpdateOrderDeliveryNode(orderID, newDeliveryNode); err != nil {
		return nil, fmt.Errorf("update delivery node: %w", err)
	}

	return m.db.GetOrder(orderID)
}

// SubmitOrder transitions a pending order to submitted and enqueues it.
func (m *Manager) SubmitOrder(orderID int64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	m.DebugLog.Log("submit: id=%d uuid=%s type=%s", orderID, order.UUID, order.OrderType)

	return m.TransitionOrder(orderID, StatusSubmitted, "submitted to dispatch")
}

// ConfirmDelivery sends a delivery receipt and transitions to confirmed.
func (m *Manager) ConfirmDelivery(orderID int64, finalCount int64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	if order.Status != StatusDelivered {
		return fmt.Errorf("order must be in delivered status to confirm, got %s", order.Status)
	}

	m.DebugLog.Log("confirm: id=%d uuid=%s count=%d", orderID, order.UUID, finalCount)

	if err := m.db.UpdateOrderFinalCount(orderID, finalCount, true); err != nil {
		return err
	}

	// Enqueue delivery receipt — failure is logged but does not block
	// the confirmation. The receipt is informational; Core tracks delivery
	// via its own fleet polling. The outbox will retry if Kafka is down.
	if err := m.sender.Queue(protocol.TypeOrderReceipt, &protocol.OrderReceipt{
		OrderUUID:   order.UUID,
		ReceiptType: "confirmed",
		FinalCount:  finalCount,
	}); err != nil {
		return fmt.Errorf("enqueue delivery receipt %s: %w", order.UUID, err)
	}

	return m.TransitionOrder(orderID, StatusConfirmed, fmt.Sprintf("confirmed with count %d", finalCount))
}

// RollbackForRetry force-transitions an order back to StatusStaged with a
// friendly detail message. Used for recoverable Core errors (e.g.
// manifest_sync_failed) where the operator can simply click release again
// instead of having to recreate the whole order.
//
// Why force-transition: the order may currently be in StatusInTransit (the
// release click already ran on Edge) or any non-terminal state, so the
// regular Transition rules don't apply. The caller has already validated
// that the rollback is appropriate (typically by inspecting an OrderError
// code from Core).
//
// The friendly detail string is what the operator UI surfaces as the
// "release error" chip on the node — see StationNodeView.LastReleaseError
// and the rendering in operator-station/operator.js.
func (m *Manager) RollbackForRetry(orderUUID, detail string) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderUUID, err)
	}
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ForceTransition(order.ID, StatusStaged, detail)
}

// RollbackReleaseRejection handles a Core release-rejection (e.g. invalid_state)
// without ever terminally failing the order — the scoped-B hardening for the
// ALN_003 divergence (Springfield 2026-06-12). A release rejection means Core
// declined to release this leg, typically because Edge's consolidated two-robot
// release fanned out to a leg that isn't releasable (recovering, or already
// finished). The Edge mirror must not die on it. Only an in_transit leg is
// rolled back — the release moved it toward dispatch and Core bounced it, so
// return it to staged for a retry. Any other state is left untouched: a
// still-staged leg is already retryable, and a terminal or pre-release leg must
// not be resurrected or re-failed by a stray fan-out rejection.
func (m *Manager) RollbackReleaseRejection(orderUUID, detail string) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderUUID, err)
	}
	if order.Status != StatusInTransit {
		m.DebugLog.Log("release rejection ignored for order %s (status=%s, not in_transit)", orderUUID, order.Status)
		return nil
	}
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ForceTransition(order.ID, StatusStaged, detail)
}
