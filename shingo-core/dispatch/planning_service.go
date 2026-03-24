package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
)

type PlanningResult struct {
	SourceNode *store.Node
	DestNode   *store.Node
	Handled    bool
	Queued     bool // order should be queued — inventory not available
}

type planningError struct {
	Code   string
	Detail string
	Err    error
}

func (e *planningError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Detail
}

type PlanningHandler func(order *store.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError)

type PlanningService struct {
	db       *store.DB
	resolver NodeResolver
	laneLock *LaneLock
	debug    func(string, ...any)

	createCompound  func(parentOrder *store.Order, plan *ReshufflePlan) error
	advanceCompound func(parentOrderID int64) error

	handlers map[string]PlanningHandler
}

func newPlanningService(db *store.DB, resolver NodeResolver, laneLock *LaneLock, debug func(string, ...any), createCompound func(*store.Order, *ReshufflePlan) error, advanceCompound func(int64) error) *PlanningService {
	s := &PlanningService{
		db:              db,
		resolver:        resolver,
		laneLock:        laneLock,
		debug:           debug,
		createCompound:  createCompound,
		advanceCompound: advanceCompound,
		handlers:        make(map[string]PlanningHandler),
	}
	s.Register(OrderTypeRetrieve, s.planRetrieve)
	s.Register(OrderTypeMove, s.planMove)
	s.Register(OrderTypeStore, s.planStore)
	return s
}

func (s *PlanningService) dbg(format string, args ...any) {
	if s.debug != nil {
		s.debug(format, args...)
	}
}

func (s *PlanningService) Register(orderType string, handler PlanningHandler) {
	s.handlers[orderType] = handler
}

func (s *PlanningService) Plan(order *store.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	handler, ok := s.handlers[order.OrderType]
	if !ok {
		return nil, &planningError{
			Code:   "unknown_type",
			Detail: fmt.Sprintf("unknown order type: %s", order.OrderType),
		}
	}
	return handler(order, env, payloadCode)
}

func (s *PlanningService) planRetrieve(order *store.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	if err := s.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding source"); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}

	if order.PayloadDesc == "retrieve_empty" {
		return s.planRetrieveEmpty(order, payloadCode)
	}

	var source *store.Bin
	var sourceNode *store.Node

	if order.PickupNode != "" && s.resolver != nil {
		pickupNode, err := s.db.GetNodeByDotName(order.PickupNode)
		if err == nil && pickupNode.IsSynthetic && pickupNode.NodeTypeCode == "NGRP" {
			result, err := s.resolver.Resolve(pickupNode, OrderTypeRetrieve, payloadCode, nil)
			if err != nil {
				var buriedErr *BuriedError
				if errors.As(err, &buriedErr) {
					s.dbg("retrieve: bin %d buried in lane %d, planning reshuffle", buriedErr.Bin.ID, buriedErr.LaneID)
					return s.planBuriedReshuffle(order, buriedErr)
				}
				s.dbg("retrieve: no source in group %s for payload=%s, queuing order %d", order.PickupNode, payloadCode, order.ID)
				return &PlanningResult{Queued: true}, nil
			}
			source = result.Bin
			sourceNode, _ = s.db.GetNode(*source.NodeID)
		}
	}

	if source == nil {
		var err error
		source, err = s.db.FindSourceBinFIFO(payloadCode)
		if err != nil {
			s.dbg("retrieve: no source bin for payload=%s, queuing order %d", payloadCode, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
		sourceNode, err = s.db.GetNode(*source.NodeID)
		if err != nil {
			return nil, &planningError{Code: "node_error", Detail: err.Error(), Err: err}
		}
	}

	s.dbg("retrieve: FIFO source bin=%d payload=%s node=%s", source.ID, payloadCode, sourceNode.Name)
	if err := s.db.ClaimBin(source.ID, order.ID); err != nil {
		return nil, &planningError{Code: "claim_failed", Detail: err.Error(), Err: err}
	}
	order.BinID = &source.ID
	if err := s.db.UpdateOrderBinID(order.ID, source.ID); err != nil {
		log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
	}
	order.PickupNode = sourceNode.Name
	if err := s.db.UpdateOrderPickupNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, &planningError{Code: "node_error", Detail: err.Error(), Err: err}
	}
	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
}

func (s *PlanningService) planRetrieveEmpty(order *store.Order, payloadCode string) (*PlanningResult, *planningError) {
	var preferZone string
	if order.DeliveryNode != "" {
		if destNode, err := s.db.GetNodeByDotName(order.DeliveryNode); err == nil {
			preferZone = destNode.Zone
		}
	}
	bin, err := s.db.FindEmptyCompatibleBin(payloadCode, preferZone)
	if err != nil {
		s.dbg("retrieve_empty: no bin for payload=%s, queuing order %d", payloadCode, order.ID)
		return &PlanningResult{Queued: true}, nil
	}
	s.dbg("retrieve_empty: found bin=%d label=%s at node=%s", bin.ID, bin.Label, bin.NodeName)
	if err := s.db.ClaimBin(bin.ID, order.ID); err != nil {
		return nil, &planningError{Code: "claim_failed", Detail: err.Error(), Err: err}
	}
	order.BinID = &bin.ID
	if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
		log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
	}
	sourceNode, err := s.db.GetNode(*bin.NodeID)
	if err != nil {
		return nil, &planningError{Code: "node_error", Detail: err.Error(), Err: err}
	}
	order.PickupNode = sourceNode.Name
	if err := s.db.UpdateOrderPickupNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, &planningError{Code: "node_error", Detail: err.Error(), Err: err}
	}
	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
}

func (s *PlanningService) planBuriedReshuffle(order *store.Order, buried *BuriedError) (*PlanningResult, *planningError) {
	if s.laneLock.IsLocked(buried.LaneID) {
		return nil, &planningError{Code: "lane_locked", Detail: fmt.Sprintf("lane %d is locked by another reshuffle", buried.LaneID)}
	}
	lane, err := s.db.GetNode(buried.LaneID)
	if err != nil || lane.ParentID == nil {
		return nil, &planningError{Code: "reshuffle_error", Detail: "cannot determine node group for lane", Err: err}
	}
	plan, err := PlanReshuffle(s.db, buried.Bin, buried.Slot, lane, *lane.ParentID)
	if err != nil {
		return nil, &planningError{Code: "reshuffle_error", Detail: fmt.Sprintf("cannot plan reshuffle: %v", err), Err: err}
	}
	if !s.laneLock.TryLock(buried.LaneID, order.ID) {
		return nil, &planningError{Code: "lane_locked", Detail: "lane locked concurrently"}
	}
	if err := s.createCompound(order, plan); err != nil {
		s.laneLock.Unlock(buried.LaneID)
		return nil, &planningError{Code: "reshuffle_error", Detail: fmt.Sprintf("cannot create compound order: %v", err), Err: err}
	}
	if err := s.db.UpdateOrderStatus(order.ID, StatusReshuffling, fmt.Sprintf("reshuffling lane — %d steps", len(plan.Steps))); err != nil {
		log.Printf("dispatch: update order %d status to reshuffling: %v", order.ID, err)
	}
	s.dbg("retrieve: compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))
	if err := s.advanceCompound(order.ID); err != nil {
		return nil, &planningError{Code: "reshuffle_error", Detail: err.Error(), Err: err}
	}
	return &PlanningResult{Handled: true}, nil
}

func (s *PlanningService) planMove(order *store.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	if err := s.db.UpdateOrderStatus(order.ID, StatusSourcing, "validating move"); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}
	if order.PickupNode == "" {
		return nil, &planningError{Code: "missing_pickup", Detail: "move order requires pickup_node"}
	}
	pickupNode, err := s.db.GetNodeByDotName(order.PickupNode)
	if err != nil {
		return nil, &planningError{Code: "invalid_node", Detail: fmt.Sprintf("pickup node %q not found", order.PickupNode), Err: err}
	}
	bins, _ := s.db.ListBinsByNode(pickupNode.ID)
	binClaimed := false
	for _, bin := range bins {
		if bin.ClaimedBy != nil {
			continue
		}
		if payloadCode != "" && bin.PayloadCode != payloadCode {
			continue
		}
		if err := s.db.ClaimBin(bin.ID, order.ID); err == nil {
			order.BinID = &bin.ID
			if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
				log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
			}
			s.dbg("move: claimed bin=%d at %s", bin.ID, order.PickupNode)
			binClaimed = true
			break
		}
	}
	if !binClaimed && payloadCode != "" {
		return nil, &planningError{Code: "no_payload", Detail: fmt.Sprintf("no unclaimed %s bin at %s", payloadCode, order.PickupNode)}
	}
	if err := s.db.UpdateOrderPickupNode(order.ID, pickupNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, &planningError{Code: "node_error", Detail: err.Error(), Err: err}
	}
	return &PlanningResult{SourceNode: pickupNode, DestNode: destNode}, nil
}

func (s *PlanningService) planStore(order *store.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	if err := s.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding storage destination"); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}
	originalDeliveryNode := order.DeliveryNode
	destNode, err := s.db.FindStorageDestination(payloadCode)
	if err != nil {
		return nil, &planningError{Code: "no_storage", Detail: "no available storage node found", Err: err}
	}
	s.dbg("store: selected destination=%s for order %d", destNode.Name, order.ID)
	order.DeliveryNode = destNode.Name
	if err := s.db.UpdateOrderDeliveryNode(order.ID, destNode.Name); err != nil {
		log.Printf("dispatch: update order %d delivery_node: %v", order.ID, err)
	}

	var pickupNode *store.Node
	if order.PickupNode != "" {
		pickupNode, err = s.db.GetNodeByDotName(order.PickupNode)
		if err != nil {
			return nil, &planningError{Code: "invalid_node", Detail: fmt.Sprintf("pickup node %q not found", order.PickupNode), Err: err}
		}
	} else if originalDeliveryNode != "" {
		pickupNode, err = s.db.GetNodeByDotName(originalDeliveryNode)
		if err != nil {
			return nil, &planningError{Code: "invalid_node", Detail: fmt.Sprintf("node %q not found", originalDeliveryNode), Err: err}
		}
	}
	if pickupNode == nil {
		return nil, &planningError{Code: "missing_pickup", Detail: "store order requires a pickup location"}
	}
	if order.BinID == nil {
		bins, _ := s.db.ListBinsByNode(pickupNode.ID)
		for _, bin := range bins {
			if bin.ClaimedBy == nil {
				if err := s.db.ClaimBin(bin.ID, order.ID); err == nil {
					order.BinID = &bin.ID
					if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
						log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
					}
					s.dbg("store: claimed bin=%d at %s", bin.ID, pickupNode.Name)
					break
				}
			}
		}
	}
	if err := s.db.UpdateOrderPickupNode(order.ID, pickupNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}
	return &PlanningResult{SourceNode: pickupNode, DestNode: destNode}, nil
}
