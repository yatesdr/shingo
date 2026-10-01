package orders

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	storemsg "shingoedge/store/messaging"
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
func (m *Manager) ReleaseOrder(orderID int64, remainingUOP *int, calledBy string, stationWait *int) error {
	return m.ReleaseOrderWithDisposition(orderID, remainingUOP, nil, calledBy, stationWait)
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
//
// stationWait is the echo (protocol.OrderRelease.StationWait): the station
// wait this release is for. The release's outbox row and its intent's sent_at
// are written in one transaction.
func (m *Manager) ReleaseOrderWithDisposition(orderID int64, remainingUOP *int, disposition *protocol.UOPDisposition, calledBy string, stationWait *int) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	// The release act sends only staged and in_transit legs (release.Releasable);
	// a terminal order here is a caller outside the act, and is refused.
	if IsTerminal(order.Status) {
		return fmt.Errorf("order is terminal (%s), cannot release", order.Status)
	}

	env, err := m.sender.build(protocol.TypeOrderRelease, &protocol.OrderRelease{
		OrderUUID:    order.UUID,
		RemainingUOP: remainingUOP,
		Disposition:  disposition,
		CalledBy:     calledBy,
		StationWait:  stationWait,
	})
	if err != nil {
		return fmt.Errorf("enqueue release: %w", err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("enqueue release: marshal envelope: %w", err)
	}
	if err := m.db.InTx(func(tx *sql.Tx) error {
		if _, err := store.EnqueueOutboxIn(tx, data, env.Type); err != nil {
			return err
		}
		return orders.MarkReleaseIntentSent(tx, orderID, time.Now().UTC().Format(time.RFC3339Nano))
	}); err != nil {
		return fmt.Errorf("enqueue release: %w", err)
	}
	storemsg.NotifyEnqueued()

	// Transition Edge status to in_transit now, at the click, without waiting
	// for Core. Core does push in_transit too — its OrderUpdate carries the
	// status and the Edge applies it (ApplyCoreStatus) — but only once the fleet
	// moves, and Core answers a release only with errors, never an acceptance.
	// Recording the release locally is what lets the board and the next click
	// see it at once; a Core refusal rolls it back (RollbackReleaseRejection).
	if err := m.TransitionOrder(orderID, StatusInTransit, ReleasedFromStagingDetail); err != nil {
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

// SetOrderFaultClock persists (or clears) Core's fault window for an order.
// Called from the edge handler on every OrderUpdate and boot snapshot.
//
// Nothing branches on it — it is what the board reads to render the fault
// sentence and tick its clock. Deliberately inert beyond display, the way
// QueueCode is: the day something does need to branch, the value is already
// there and it is a read, not a schema change.
func (m *Manager) SetOrderFaultClock(uuid string, since, deadline *time.Time, noticeAfterS int, ref string) error {
	return m.db.SetOrderFaultClock(uuid, since, deadline, noticeAfterS, ref)
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
	m.clearIntentForRejection(order.ID, detail)
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
// THE SENTENCE IS COMPOSED HERE, not at the message handler, because this is
// where the order is in hand. HandleOrderError never loads it and so cannot
// name what is actually blocking the release — it appended "Click release to
// retry." to every rejection, including the ones that will fail identically
// until something upstream clears. Telling an operator to press a button that
// cannot work is worse than telling them nothing: they press it, it fails the
// same way, and the message says to press it again.
//
// There is no attempt counter to show, and that is a protocol fact rather than
// an omission: protocol.OrderError carries {OrderUUID, ErrorCode, Detail} and
// nothing else. Core's mirrored QueueReason/QueueCode is the best account of
// the blocker this side has, and it is already on the order row.
func (m *Manager) RollbackReleaseRejection(orderUUID, coreDetail string) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("get order %s: %w", orderUUID, err)
	}
	if order.Status != StatusInTransit {
		m.DebugLog.Log("release rejection ignored for order %s (status=%s, not in_transit)", orderUUID, order.Status)
		return nil
	}
	detail := releaseRejectionDetail(order, coreDetail)
	m.clearIntentForRejection(order.ID, detail)
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ForceTransition(order.ID, StatusStaged, detail)
}

// releaseRejectionDetail builds the operator-facing sentence for an
// invalid_state rollback.
//
// The chip shows it from orders.release_held (clearIntentForRejection).
//
// The retry advice is CONDITIONAL, which is the whole point. When Core has told
// us why the order is queued, that blocker is named and no retry is suggested:
// the release will be refused the same way until it clears. Only when we have
// no account of the blocker is "click release to retry" honest, because then
// trying again genuinely is the way to find out.
func releaseRejectionDetail(order *orders.Order, coreDetail string) string {
	var b strings.Builder
	b.WriteString("Core rejected the release")
	if coreDetail != "" {
		b.WriteString(": ")
		b.WriteString(coreDetail)
	}
	b.WriteString(".")
	if reason := strings.TrimSpace(order.QueueReason); reason != "" {
		b.WriteString(" Blocked by: ")
		b.WriteString(reason)
		if code := strings.TrimSpace(order.QueueCode); code != "" {
			b.WriteString(" (")
			b.WriteString(code)
			b.WriteString(")")
		}
		b.WriteString(". Release again once that clears.")
		return b.String()
	}
	b.WriteString(" Click release to retry.")
	return b.String()
}

// ReleasedFromStagingDetail is the order_history detail this Edge writes when
// it releases a leg from a wait. PassedAStationWait reads it back.
const ReleasedFromStagingDetail = "released from staging"

// clearIntentForRejection is a Core rejection's effect on the release intent:
// cleared, in its own statement, before the rollback's status emit (so the
// emit cannot wake a re-fire of the release Core just refused), and the
// rejection's sentence written for the chip.
func (m *Manager) clearIntentForRejection(orderID int64, sentence string) {
	if err := m.db.SetOrderReleaseIntent(orderID, ""); err != nil {
		m.DebugLog.Log("rollback: clear release intent for order %d: %v", orderID, err)
	}
	if err := m.db.SetOrderReleaseHeld(orderID, sentence); err != nil {
		m.DebugLog.Log("rollback: write release held for order %d: %v", orderID, err)
	}
}

// NoteReleaseHeld records, on the order, why its release is held at a live
// light curtain (Q8's sentence). It changes no status: the order stays where it
// is, and the note is
// an order_history row the board's release chip reads (store's chip prefixes),
// so the refusal is somewhere an operator can see it rather than only in a log.
func (m *Manager) NoteReleaseHeld(orderID int64, sentence string) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order %d: %w", orderID, err)
	}
	return m.db.InsertOrderHistory(order.ID, string(order.Status), string(order.Status), sentence)
}
