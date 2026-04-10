package orders

import (
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/store"
)

type LifecycleService struct {
	db      *store.DB
	emitter EventEmitter
	debug   DebugLogFunc
}

func newLifecycleService(db *store.DB, emitter EventEmitter, debug DebugLogFunc) *LifecycleService {
	return &LifecycleService{db: db, emitter: emitter, debug: debug}
}

func (s *LifecycleService) Transition(orderID int64, newStatus, detail string) error {
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	if !IsValidTransition(order.Status, newStatus) {
		if order.Status == newStatus || IsTerminal(order.Status) {
			return nil // idempotent: already in target state or terminal
		}
		return fmt.Errorf("invalid transition from %s to %s", order.Status, newStatus)
	}
	if order.OrderType == TypeStore && newStatus == StatusSubmitted && !order.CountConfirmed {
		return fmt.Errorf("store order requires count confirmation before submitting")
	}
	return s.applyTransition(order, newStatus, detail, false)
}

func (s *LifecycleService) ForceTransition(orderID int64, newStatus, detail string) error {
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return err
	}
	if order.Status == newStatus {
		return nil
	}
	return s.applyTransition(order, newStatus, detail, true)
}

func (s *LifecycleService) applyTransition(order *store.Order, newStatus, detail string, forced bool) error {
	oldStatus := order.Status
	if forced {
		s.debug.log("force transition: id=%d uuid=%s %s->%s", order.ID, order.UUID, oldStatus, newStatus)
	} else {
		s.debug.log("transition: id=%d uuid=%s %s->%s", order.ID, order.UUID, oldStatus, newStatus)
	}
	if err := s.db.UpdateOrderStatus(order.ID, newStatus); err != nil {
		return fmt.Errorf("update order status: %w", err)
	}
	if err := s.db.InsertOrderHistory(order.ID, oldStatus, newStatus, detail); err != nil {
		log.Printf("insert order history: %v", err)
	}
	updated, _ := s.db.GetOrder(order.ID)
	eta := ""
	if updated != nil && updated.ETA != nil {
		eta = *updated.ETA
	}
	s.emitter.EmitOrderStatusChanged(order.ID, order.UUID, order.OrderType, oldStatus, newStatus, eta, nil, order.ProcessNodeID)
	if IsTerminal(newStatus) {
		s.emitter.EmitOrderCompleted(order.ID, order.UUID, order.OrderType, nil, order.ProcessNodeID)
		if newStatus == StatusFailed {
			s.emitter.EmitOrderFailed(order.ID, order.UUID, order.OrderType, detail)
		}
	}
	return nil
}

func (s *LifecycleService) HandleDelivered(order *store.Order, statusDetail string, stagedExpireAt *time.Time) error {
	if stagedExpireAt != nil {
		s.db.UpdateOrderStagedExpireAt(order.ID, stagedExpireAt)
	}
	return s.Transition(order.ID, StatusDelivered, statusDetail)
}

func (s *LifecycleService) ApplyCoreStatusSnapshot(snapshot protocol.OrderStatusSnapshot) error {
	order, err := s.db.GetOrderByUUID(snapshot.OrderUUID)
	if err != nil {
		return err
	}
	if !snapshot.Found || snapshot.Status == "" || snapshot.Status == order.Status {
		return nil
	}

	detail := "startup reconciliation with core"
	switch snapshot.Status {
	case StatusConfirmed:
		if order.Status == StatusDelivered {
			return s.Transition(order.ID, StatusConfirmed, detail)
		}
		return s.ForceTransition(order.ID, StatusConfirmed, detail)
	case StatusCancelled:
		return s.ForceTransition(order.ID, StatusCancelled, detail)
	case StatusFailed:
		return s.ForceTransition(order.ID, StatusFailed, detail)
	case StatusDelivered:
		return s.ForceTransition(order.ID, StatusDelivered, detail)
	case StatusStaged:
		return s.ForceTransition(order.ID, StatusStaged, detail)
	case StatusInTransit:
		return s.ForceTransition(order.ID, StatusInTransit, detail)
	case StatusAcknowledged:
		return s.ForceTransition(order.ID, StatusAcknowledged, detail)
	case StatusQueued:
		return s.ForceTransition(order.ID, StatusQueued, detail)
	default:
		return nil
	}
}
