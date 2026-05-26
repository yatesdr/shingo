package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

// RecoveryService owns the operator-triggered recovery actions
// (reapply-completion, release-claim, release-staged-bin, cancel-stuck-
// order, recover-faulted-order, reissue-terminate).
//
// Two dependencies on purpose: engine is the orchestration surface
// (Events bus, dispatcher, fleet adapter, TerminateOrder, slot
// classification helpers); db is the narrower DB surface declared in
// recovery_store.go. The two fields point at the same underlying state
// at construction time — keeping them explicit makes the DB dependency
// fakeable for tests without dragging in the rest of the engine.
type RecoveryService struct {
	engine *Engine
	db     RecoveryStore
}

func newRecoveryService(e *Engine) *RecoveryService {
	return &RecoveryService{engine: e, db: e.db}
}

func (s *RecoveryService) ReapplyOrderCompletion(orderID int64, actor string) error {
	e := s.engine
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	if order.Status != "confirmed" || order.CompletedAt != nil {
		return fmt.Errorf("order %d is not awaiting completion recovery", orderID)
	}
	if order.BinID == nil {
		return fmt.Errorf("order %d has no bin to complete", orderID)
	}
	if order.DeliveryNode == "" {
		return fmt.Errorf("order %d has no delivery node", orderID)
	}

	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return fmt.Errorf("load delivery node: %w", err)
	}

	isStorage := e.isStorageSlot(destNode.ID)

	var expiresAt *time.Time
	if !isStorage {
		expiresAt = e.resolveStagingExpiry(destNode)
	}

	if err := s.db.RepairConfirmedOrderCompletion(order.ID, *order.BinID, destNode.ID, !isStorage, expiresAt); err != nil {
		return err
	}

	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	s.db.AppendAudit("order", order.ID, "recovery.reapply_completion", "", actor, actor)
	s.db.RecordRecoveryAction("reapply_completion", "order", order.ID, "reapplied confirmed completion side effects", actor)

	sourceNodeID := int64(0)
	if order.SourceNode != "" {
		if node, err := s.db.GetNodeByDotName(order.SourceNode); err == nil {
			sourceNodeID = node.ID
		}
	}
	if bin, err := s.db.GetBin(*order.BinID); err == nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  sourceNodeID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
		}})
	}

	return nil
}

// ForceConfirmDelivered advances a delivered order through confirm →
// complete immediately, the same transition the 5-minute auto-confirm
// loop performs. Used when the operator is stuck — the bin has been
// moved elsewhere or the arrival side effects never propagated — and
// waiting 5 minutes isn't acceptable. The onOrderCompleted callback
// fires the standard completion chain; handleOrderCompleted's
// claim-based teleport guard ensures the bin isn't snapped back if it
// no longer claims this order, so calling it is safe regardless of
// the bin's current physical location.
func (s *RecoveryService) ForceConfirmDelivered(orderID int64, actor string) error {
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	if order.Status != "delivered" {
		return fmt.Errorf("order %d is not delivered (status=%q)", orderID, order.Status)
	}
	detail := fmt.Sprintf("manually force-confirmed by %s", actor)
	if err := s.db.UpdateOrderStatus(order.ID, "confirmed", detail); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if err := s.db.CompleteOrder(order.ID); err != nil {
		return fmt.Errorf("complete order: %w", err)
	}
	s.db.AppendAudit("order", order.ID, "recovery.force_confirm_delivered", "", "", actor)
	s.db.RecordRecoveryAction("force_confirm_delivered", "order", order.ID,
		"manually advanced delivered → confirmed → completed", actor)
	if s.engine != nil && s.engine.reconciliation != nil &&
		s.engine.reconciliation.onOrderCompleted != nil {
		s.engine.reconciliation.onOrderCompleted(order.ID, order.EdgeUUID, order.StationID)
	}
	return nil
}

func (s *RecoveryService) ReleaseTerminalBinClaim(binID int64, actor string) error {
	orderID, err := s.db.ReleaseTerminalBinClaim(binID)
	if err != nil {
		return err
	}

	s.db.AppendAudit("bin", binID, "recovery.release_claim", fmt.Sprintf("order=%d", orderID), "", actor)
	if orderID != 0 {
		s.db.AppendAudit("order", orderID, "recovery.release_claim", fmt.Sprintf("bin=%d", binID), "", actor)
	}
	s.db.RecordRecoveryAction("release_terminal_claim", "bin", binID, fmt.Sprintf("released stale claim held by order %d", orderID), actor)
	return nil
}

func (s *RecoveryService) ReleaseStagedBin(binID int64, actor string) error {
	bin, err := s.db.GetBin(binID)
	if err != nil {
		return fmt.Errorf("bin not found")
	}
	if bin.Status != domain.BinStatusStaged {
		return fmt.Errorf("bin %d is not staged", binID)
	}
	if err := s.db.ReleaseStagedBin(binID); err != nil {
		return err
	}
	s.db.AppendAudit("bin", binID, "recovery.release_staged", string(domain.BinStatusStaged), string(domain.BinStatusAvailable), actor)
	s.db.RecordRecoveryAction("release_staged_bin", "bin", binID, "released staged bin back to available", actor)
	return nil
}

func (s *RecoveryService) CancelStuckOrder(orderID int64, actor string) error {
	e := s.engine
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	if order.Status.IsTerminal() {
		return fmt.Errorf("order %d is already terminal", orderID)
	}
	if err := e.TerminateOrder(orderID, actor); err != nil {
		return err
	}
	s.db.RecordRecoveryAction("cancel_stuck_order", "order", orderID, fmt.Sprintf("cancelled stuck order in status %s", order.Status), actor)
	return nil
}

// RecoverFaultedOrder pushes a faulted order back to in_transit for operators
// who have cleared a fault and don't want to wait for the grace timer.
// Follows the CancelStuckOrder template: status guard, lifecycle method, audit.
func (s *RecoveryService) RecoverFaultedOrder(orderID int64, actor string) error {
	e := s.engine
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	if order.Status != protocol.StatusFaulted {
		return fmt.Errorf("order %d is not faulted (status: %s)", orderID, order.Status)
	}

	if err := e.dispatcher.Lifecycle().MarkInTransit(order, order.RobotID, "recovery"); err != nil {
		return fmt.Errorf("mark in_transit: %w", err)
	}

	s.db.AppendAudit("order", order.ID, "recovery.recover_faulted", "", actor, actor)
	s.db.RecordRecoveryAction("recover_faulted_order", "order", order.ID, "recovered faulted order to in_transit", actor)
	return nil
}

// ReissueTerminate re-fires /terminate for orders that grace-expired but the
// original best-effort terminate failed. Returns the error from the fleet
// adapter (NOT log-and-continue; the operator is asking to retry).
func (s *RecoveryService) ReissueTerminate(orderID int64, actor string) error {
	e := s.engine
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	if order.Status != protocol.StatusFailed {
		return fmt.Errorf("order %d is not failed (status: %s)", orderID, order.Status)
	}

	if err := e.fleet.CancelOrder(order.VendorOrderID); err != nil {
		return fmt.Errorf("fleet cancel: %w", err)
	}

	s.db.AppendAudit("order", order.ID, "recovery.reissue_terminate", order.VendorOrderID, actor, actor)
	s.db.RecordRecoveryAction("reissue_terminate", "order", order.ID, "re-issued terminate to fleet vendor", actor)
	return nil
}
