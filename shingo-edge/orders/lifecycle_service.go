package orders

import (
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/orders"
)

type LifecycleService struct {
	db      *store.DB
	emitter EventEmitter
	debug   DebugLogFunc
}

func newLifecycleService(db *store.DB, emitter EventEmitter, debug DebugLogFunc) *LifecycleService {
	return &LifecycleService{db: db, emitter: emitter, debug: debug}
}

func (s *LifecycleService) Transition(orderID int64, newStatus protocol.Status, detail string) error {
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

func (s *LifecycleService) ForceTransition(orderID int64, newStatus protocol.Status, detail string) error {
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return err
	}
	if order.Status == newStatus {
		return nil
	}
	return s.applyTransition(order, newStatus, detail, true)
}

func (s *LifecycleService) applyTransition(order *orders.Order, newStatus protocol.Status, detail string, forced bool) error {
	oldStatus := order.Status
	if forced {
		s.debug.Log("force transition: id=%d uuid=%s %s->%s", order.ID, order.UUID, oldStatus, newStatus)
	} else {
		s.debug.Log("transition: id=%d uuid=%s %s->%s", order.ID, order.UUID, oldStatus, newStatus)
	}
	if err := s.db.UpdateOrderStatus(order.ID, string(newStatus)); err != nil {
		return fmt.Errorf("update order status: %w", err)
	}
	if err := s.db.InsertOrderHistory(order.ID, string(oldStatus), string(newStatus), detail); err != nil {
		log.Printf("insert order history: %v", err)
	}
	updated, _ := s.db.GetOrder(order.ID)
	eta := ""
	if updated != nil && updated.ETA != nil {
		eta = *updated.ETA
	}
	s.emitter.EmitOrderStatusChanged(order.ID, order.UUID, order.OrderType, string(oldStatus), string(newStatus), eta, nil, order.ProcessNodeID)
	// Delivered fires *before* the terminal-status branch so the runtime
	// UOP handler binds the cache to the actually-arrived bin the moment
	// the bin lands, independent of when (or whether) the operator
	// confirms. Order.BinID is read from the post-transition row because
	// HandleDelivered may have just stamped Core's resolved bin id.
	if newStatus == StatusDelivered {
		var binID *int64
		if updated != nil {
			binID = updated.BinID
		}
		s.emitter.EmitOrderDelivered(order.ID, order.UUID, order.OrderType, order.ProcessNodeID, binID)
	}
	if IsTerminal(newStatus) {
		s.emitter.EmitOrderCompleted(order.ID, order.UUID, order.OrderType, nil, order.ProcessNodeID)
		if newStatus == StatusFailed {
			s.emitter.EmitOrderFailed(order.ID, order.UUID, order.OrderType, detail)
		}
	}
	return nil
}

func (s *LifecycleService) HandleDelivered(order *orders.Order, statusDetail string, stagedExpireAt *time.Time, binID *int64) error {
	if stagedExpireAt != nil {
		if err := s.db.UpdateOrderStagedExpireAt(order.ID, stagedExpireAt); err != nil {
			log.Printf("lifecycle: update staged_expire_at for order=%d: %v", order.ID, err)
		}
	}
	// Capture Core's bin id at delivery so the PLC tick path can
	// attribute deltas to the right bin. Nil for multi-bin orders /
	// older Core builds. Post-flip (6d226d1) Edge's runtime cache is
	// authoritative for at-node bin UOP; the OrderDelivered envelope
	// no longer carries a UOP snapshot.
	if binID != nil {
		if err := s.db.UpdateOrderBinID(order.ID, binID); err != nil {
			log.Printf("update order bin_id: %v", err)
		}
	}
	return s.Transition(order.ID, StatusDelivered, statusDetail)
}

func (s *LifecycleService) ApplyCoreStatusSnapshot(snapshot protocol.OrderStatusSnapshot) error {
	order, err := s.db.GetOrderByUUID(snapshot.OrderUUID)
	if err != nil {
		return err
	}
	snapStatus := protocol.Status(snapshot.Status)
	if !snapshot.Found || snapStatus == "" || snapStatus == order.Status {
		return nil
	}

	detail := "startup reconciliation with core"
	switch snapStatus {
	case StatusConfirmed:
		if order.Status == StatusDelivered {
			return s.Transition(order.ID, StatusConfirmed, detail)
		}
		return s.ForceTransition(order.ID, StatusConfirmed, detail)
	case StatusCancelled:
		return s.ForceTransition(order.ID, StatusCancelled, detail)
	case StatusFailed:
		return s.ForceTransition(order.ID, StatusFailed, detail)
	case StatusSkipped:
		return s.ForceTransition(order.ID, StatusSkipped, detail)
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
	case StatusReshuffling:
		// Complex-order reshuffles can sit in Reshuffling for minutes
		// while the compound runs. Without this arm an edge restart in
		// that window would leave the edge mirror stuck on a stale
		// pre-reshuffle status. Same fix lets simple-retrieve
		// reshuffles surface correctly after edge reconnect.
		return s.ForceTransition(order.ID, StatusReshuffling, detail)
	default:
		return nil
	}
}
