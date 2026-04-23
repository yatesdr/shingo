package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/service"
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

func (e *planningError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type PlanningHandler func(order *store.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError)

type PlanningService struct {
	db          *store.DB
	resolver    NodeResolver
	laneLock    *LaneLock
	binManifest *service.BinManifestService
	debug       func(string, ...any)

	createCompound  func(parentOrder *store.Order, plan *ReshufflePlan) error
	advanceCompound func(parentOrderID int64) error

	handlers map[string]PlanningHandler

	// postFindHook is called after a bin lookup succeeds but before the claim.
	// Nil by default ( no-op in production. Set via SetPostFindHook for tests
	// to widen the the TOCTOU race window for deterministic concurrent testing.
	postFindHook func()
}

func newPlanningService(db *store.DB, resolver NodeResolver, laneLock *LaneLock, binManifest *service.BinManifestService, debug func(string, ...any), createCompound func(*store.Order, *ReshufflePlan) error, advanceCompound func(int64) error) *PlanningService {
	s := &PlanningService{
		db:              db,
		resolver:        resolver,
		laneLock:        laneLock,
		binManifest:     binManifest,
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

// extractRemainingUOP parses the envelope payload to extract the remaining_uop
// field from an OrderRequest. Returns nil if the field is absent or unparseable.
func extractRemainingUOP(env *protocol.Envelope) *int {
	if env == nil || len(env.Payload) == 0 {
		return nil
	}
	// Decode the Data wrapper first, then the body
	var data protocol.Data
	if err := json.Unmarshal(env.Payload, &data); err != nil {
		return nil
	}
	var partial struct {
		RemainingUOP *int `json:"remaining_uop,omitempty"`
	}
	if err := json.Unmarshal(data.Body, &partial); err != nil {
		return nil
	}
	return partial.RemainingUOP
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

	if order.SourceNode != "" && s.resolver != nil {
		sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
		if err == nil && sourceNode.IsSynthetic && sourceNode.NodeTypeCode == "NGRP" {
			result, err := s.resolver.Resolve(sourceNode, OrderTypeRetrieve, payloadCode, nil)
			if err != nil {
				var buriedErr *BuriedError
				if errors.As(err, &buriedErr) {
					s.dbg("retrieve: bin %d buried in lane %d, planning reshuffle", buriedErr.Bin.ID, buriedErr.LaneID)
					return s.planBuriedReshuffle(order, buriedErr)
				}
				var structErr *StructuralError
				if errors.As(err, &structErr) {
					s.dbg("retrieve: STRUCTURAL failure in group %s: %s",
						order.SourceNode, structErr.Reason)
					return nil, &planningError{
						Code:   "structural",
						Detail: structErr.Error(),
						Err:    structErr,
					}
				}
				s.dbg("retrieve: no source in group %s for payload=%s, queuing order %d", order.SourceNode, payloadCode, order.ID)
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
	if s.postFindHook != nil {
		s.postFindHook()
	}
	remainingUOP := extractRemainingUOP(env)
	if err := s.binManifest.ClaimForDispatch(source.ID, order.ID, remainingUOP); err != nil {
		return nil, &planningError{Code: "claim_failed", Detail: err.Error(), Err: err}
	}
	order.BinID = &source.ID
	if err := s.db.UpdateOrderBinID(order.ID, source.ID); err != nil {
		log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
	}
	order.SourceNode = sourceNode.Name
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d source_node: %v", order.ID, err)
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

	// Check if the bin is buried in a lane — FindEmptyCompatibleBin is lane-unaware.
	if bin.NodeID != nil {
		accessible, accErr := s.db.IsSlotAccessible(*bin.NodeID)
		if accErr == nil && !accessible {
			slot, slotErr := s.db.GetNode(*bin.NodeID)
			if slotErr == nil && slot.ParentID != nil {
				lane, laneErr := s.db.GetNode(*slot.ParentID)
				if laneErr == nil && lane.NodeTypeCode == "LANE" {
					s.dbg("retrieve_empty: bin %d is buried at slot %s in lane %s, triggering reshuffle",
						bin.ID, slot.Name, lane.Name)
					return s.planBuriedReshuffle(order, &BuriedError{Bin: bin, Slot: slot, LaneID: lane.ID})
				}
			}
		}
	}

	if s.postFindHook != nil {
		s.postFindHook()
	}
	// retrieve_empty always does a plain claim — no manifest change needed
	// (the bin is already empty).
	if err := s.binManifest.ClaimForDispatch(bin.ID, order.ID, nil); err != nil {
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
	order.SourceNode = sourceNode.Name
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d source_node: %v", order.ID, err)
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
	if order.SourceNode == "" {
		return nil, &planningError{Code: "missing_source", Detail: "move order requires source_node"}
	}
	sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
	if err != nil {
		return nil, &planningError{Code: "invalid_node", Detail: fmt.Sprintf("source node %q not found", order.SourceNode), Err: err}
	}

	// If the source is a synthetic NGRP (supermarket group), resolve to a
	// concrete bin within the group. Without this, ListBinsByNode on the NGRP
	// returns zero bins (they live at child slots, not on the NGRP itself),
	// causing the order to dispatch without a bin claim. On completion the
	// bin's DB location would never update — it'd still show the old slot.
	//
	// We reuse OrderTypeRetrieve semantics: finding the best bin in an NGRP
	// for a move-from-supermarket is the same operation as a retrieve.
	if sourceNode.IsSynthetic && sourceNode.NodeTypeCode == "NGRP" && s.resolver != nil {
		result, rErr := s.resolver.Resolve(sourceNode, OrderTypeRetrieve, payloadCode, nil)
		if rErr != nil {
			var buriedErr *BuriedError
			if errors.As(rErr, &buriedErr) {
				s.dbg("move: bin %d buried in lane %d, planning reshuffle", buriedErr.Bin.ID, buriedErr.LaneID)
				return s.planBuriedReshuffle(order, buriedErr)
			}
			var structErr *StructuralError
			if errors.As(rErr, &structErr) {
				s.dbg("move: STRUCTURAL failure in group %s: %s (falling through to FIFO)",
					order.SourceNode, structErr.Reason)
			}
			s.dbg("move: no source in group %s for payload=%s, queuing order %d", order.SourceNode, payloadCode, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
		if result.Bin != nil {
			remainingUOP := extractRemainingUOP(env)
			if err := s.binManifest.ClaimForDispatch(result.Bin.ID, order.ID, remainingUOP); err != nil {
				return nil, &planningError{Code: "claim_failed", Detail: err.Error(), Err: err}
			}
			order.BinID = &result.Bin.ID
			if err := s.db.UpdateOrderBinID(order.ID, result.Bin.ID); err != nil {
				log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
			}
			// Update sourceNode to the resolved concrete slot so that
			// SourceNode in the DB reflects the actual pickup location,
			// not the NGRP name. This is critical for handleOrderCompleted.
			concreteNode, cErr := s.db.GetNode(*result.Bin.NodeID)
			if cErr != nil {
				return nil, &planningError{Code: "node_error", Detail: fmt.Sprintf("resolve slot for bin %d: %v", result.Bin.ID, cErr), Err: cErr}
			}
			sourceNode = concreteNode
			s.dbg("move: NGRP resolved bin=%d at %s (remainingUOP=%v)", result.Bin.ID, sourceNode.Name, remainingUOP)
		} else {
			// Resolver returned a node but no specific bin — queue and retry.
			s.dbg("move: NGRP resolved node %s but no bin, queuing order %d", result.Node.Name, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
	} else {
		// Concrete source node: claim a bin directly at the node.
		bins, _ := s.db.ListBinsByNode(sourceNode.ID)
		binClaimed := false
		remainingUOP := extractRemainingUOP(env)
		// Per-bin reject reasons. Used in the no_bin/no_payload failure log so
		// "why" is in the log alongside "what failed" — mirrors the
		// claimComplexBins skip-reason pattern. Previously only payload
		// mismatches were logged at debug level, leaving already-claimed and
		// status-rejected bins silent.
		var rejects []string
		for _, bin := range bins {
			if reason := BinUnavailableReason(bin, payloadCode); reason != "" {
				rejects = append(rejects, fmt.Sprintf("bin=%d (%s): %s", bin.ID, bin.Label, reason))
				continue
			}
			if err := s.binManifest.ClaimForDispatch(bin.ID, order.ID, remainingUOP); err != nil {
				// Was silently swallowed before. Surface the SQL-guard error
				// (locked, claimed_by mismatch, etc.) so the operator can see
				// why a bin that passed the availability check still couldn't
				// be claimed.
				rejects = append(rejects, fmt.Sprintf("bin=%d (%s): ClaimForDispatch failed: %v", bin.ID, bin.Label, err))
				log.Printf("dispatch: move order %d ClaimForDispatch failed for bin %d at %s — %v",
					order.ID, bin.ID, order.SourceNode, err)
				continue
			}
			order.BinID = &bin.ID
			if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
				log.Printf("dispatch: WARNING move order %d UpdateOrderBinID(bin=%d) failed — order.BinID will read NULL on next load: %v",
					order.ID, bin.ID, err)
			}
			s.dbg("move: claimed bin=%d at %s (remainingUOP=%v)", bin.ID, order.SourceNode, remainingUOP)
			binClaimed = true
			break
		}
		if !binClaimed {
			detail := fmt.Sprintf("no unclaimed bin at %s for move order %d (evaluated %d bin(s); rejects: [%s])",
				order.SourceNode, order.ID, len(bins), joinRejects(rejects))
			log.Printf("dispatch: move order %d at %s — %s", order.ID, order.SourceNode, detail)
			if payloadCode != "" {
				return nil, &planningError{Code: "no_payload", Detail: fmt.Sprintf("no unclaimed %s bin at %s", payloadCode, order.SourceNode)}
			}
			// Safety net: a move order without a claimed bin would silently
			// dispatch to the fleet, but handleOrderCompleted would skip the
			// bin arrival update (BinID == nil). Fail loudly instead.
			return nil, &planningError{Code: "no_bin", Detail: detail}
		}
	}

	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d source_node: %v", order.ID, err)
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, &planningError{Code: "node_error", Detail: err.Error(), Err: err}
	}
	// Guard: source and destination must differ. A same-node move is physically
	// impossible and would waste a fleet transport order.
	if sourceNode.ID == destNode.ID {
		return nil, &planningError{Code: "same_node", Detail: fmt.Sprintf("source and destination are the same node (%s)", sourceNode.Name)}
	}
	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
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

	var sourceNode *store.Node
	if order.SourceNode != "" {
		sourceNode, err = s.db.GetNodeByDotName(order.SourceNode)
		if err != nil {
			return nil, &planningError{Code: "invalid_node", Detail: fmt.Sprintf("source node %q not found", order.SourceNode), Err: err}
		}
	} else if originalDeliveryNode != "" {
		sourceNode, err = s.db.GetNodeByDotName(originalDeliveryNode)
		if err != nil {
			return nil, &planningError{Code: "invalid_node", Detail: fmt.Sprintf("node %q not found", originalDeliveryNode), Err: err}
		}
	}
	if sourceNode == nil {
		return nil, &planningError{Code: "missing_source", Detail: "store order requires a source location"}
	}
	if order.BinID == nil {
		bins, _ := s.db.ListBinsByNode(sourceNode.ID)
		for _, bin := range bins {
			if bin.ClaimedBy == nil {
				// Store orders: plain claim, no manifest change.
				if err := s.binManifest.ClaimForDispatch(bin.ID, order.ID, nil); err == nil {
					order.BinID = &bin.ID
					if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
						log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
					}
					s.dbg("store: claimed bin=%d at %s", bin.ID, sourceNode.Name)
					break
				}
			}
		}
	}
	if order.BinID == nil {
		return nil, &planningError{Code: "no_bin", Detail: fmt.Sprintf("no available bin at %s", sourceNode.Name)}
	}
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d source_node: %v", order.ID, err)
	}
	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
}
