package engine

import (
	"fmt"
	"time"
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

	isStorageSlot := false
	if destNode.ParentID != nil {
		if parent, err := e.db.GetNode(*destNode.ParentID); err == nil && parent.NodeTypeCode == "LANE" {
			isStorageSlot = true
		}
	}

	var expiresAt *time.Time
	if !isStorageSlot {
		expiresAt = e.resolveStagingExpiry(destNode)
	}

	if err := e.db.RepairConfirmedOrderCompletion(order.ID, *order.BinID, destNode.ID, !isStorageSlot, expiresAt); err != nil {
		return err
	}

	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	e.db.AppendAudit("order", order.ID, "recovery.reapply_completion", "", actor, actor)
	e.db.RecordRecoveryAction("reapply_completion", "order", order.ID, "reapplied confirmed completion side effects", actor)

	sourceNodeID := int64(0)
	if order.PickupNode != "" {
		if sourceNode, err := e.db.GetNodeByDotName(order.PickupNode); err == nil {
			sourceNodeID = sourceNode.ID
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
	if bin.Status != "staged" {
		return fmt.Errorf("bin %d is not staged", binID)
	}
	if err := e.db.ReleaseStagedBin(binID); err != nil {
		return err
	}
	e.db.AppendAudit("bin", binID, "recovery.release_staged", "staged", "available", actor)
	e.db.RecordRecoveryAction("release_staged_bin", "bin", binID, "released staged bin back to available", actor)
	return nil
}

func (s *RecoveryService) CancelStuckOrder(orderID int64, actor string) error {
	e := s.engine
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	switch order.Status {
	case "confirmed", "cancelled", "failed":
		return fmt.Errorf("order %d is already terminal", orderID)
	}
	if err := e.TerminateOrder(orderID, actor); err != nil {
		return err
	}
	e.db.RecordRecoveryAction("cancel_stuck_order", "order", orderID, fmt.Sprintf("cancelled stuck order in status %s", order.Status), actor)
	return nil
}
