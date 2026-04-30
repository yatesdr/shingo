package engine

import (
	"fmt"
	"time"

	"shingocore/domain"
)

type RecoveryService struct {
	engine *Engine
}

func newRecoveryService(e *Engine) *RecoveryService {
	return &RecoveryService{engine: e}
}

func (s *RecoveryService) ReapplyOrderCompletion(orderID int64, actor string) error {
	e := s.engine
	order, err := e.db.GetOrder(orderID)
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

	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return fmt.Errorf("load delivery node: %w", err)
	}

	isStorage := e.isStorageSlot(destNode.ID)

	var expiresAt *time.Time
	if !isStorage {
		expiresAt = e.resolveStagingExpiry(destNode)
	}

	if err := e.db.RepairConfirmedOrderCompletion(order.ID, *order.BinID, destNode.ID, !isStorage, expiresAt); err != nil {
		return err
	}

	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	e.db.AppendAudit("order", order.ID, "recovery.reapply_completion", "", actor, actor)
	e.db.RecordRecoveryAction("reapply_completion", "order", order.ID, "reapplied confirmed completion side effects", actor)

	sourceNodeID := int64(0)
	if order.SourceNode != "" {
		if node, err := e.db.GetNodeByDotName(order.SourceNode); err == nil {
			sourceNodeID = node.ID
		}
	}
	if bin, err := e.db.GetBin(*order.BinID); err == nil {
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

func (s *RecoveryService) ReleaseTerminalBinClaim(binID int64, actor string) error {
	e := s.engine
	orderID, err := e.db.ReleaseTerminalBinClaim(binID)
	if err != nil {
		return err
	}

	e.db.AppendAudit("bin", binID, "recovery.release_claim", fmt.Sprintf("order=%d", orderID), "", actor)
	if orderID != 0 {
		e.db.AppendAudit("order", orderID, "recovery.release_claim", fmt.Sprintf("bin=%d", binID), "", actor)
	}
	e.db.RecordRecoveryAction("release_terminal_claim", "bin", binID, fmt.Sprintf("released stale claim held by order %d", orderID), actor)
	return nil
}

func (s *RecoveryService) ReleaseStagedBin(binID int64, actor string) error {
	e := s.engine
	bin, err := e.db.GetBin(binID)
	if err != nil {
		return fmt.Errorf("bin not found")
	}
	if bin.Status != domain.BinStatusStaged {
		return fmt.Errorf("bin %d is not staged", binID)
	}
	if err := e.db.ReleaseStagedBin(binID); err != nil {
		return err
	}
	e.db.AppendAudit("bin", binID, "recovery.release_staged", string(domain.BinStatusStaged), string(domain.BinStatusAvailable), actor)
	e.db.RecordRecoveryAction("release_staged_bin", "bin", binID, "released staged bin back to available", actor)
	return nil
}

func (s *RecoveryService) CancelStuckOrder(orderID int64, actor string) error {
	e := s.engine
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	if order.Status.IsTerminal() {
		return fmt.Errorf("order %d is already terminal", orderID)
	}
	if err := e.TerminateOrder(orderID, actor); err != nil {
		return err
	}
	e.db.RecordRecoveryAction("cancel_stuck_order", "order", orderID, fmt.Sprintf("cancelled stuck order in status %s", order.Status), actor)
	return nil
}
