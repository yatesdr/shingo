package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/dispatch/binsource"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

type PlanningResult struct {
	SourceNode *nodes.Node
	DestNode   *nodes.Node
	Handled    bool
	Queued     bool // order should be queued — inventory not available
}

type planningError struct {
	Code   string
	Detail string
	Err    error
}

// asPlanningError wraps a ClaimForDispatch error (or a synthetic race signal
// with err==nil) as a codeClaimFailed planningError — the transient-contention
// code that queues the order for scanner retry. All six claim-failure sites that
// feed this helper have already had structural failures pre-filtered by
// BinUnavailableReason or an explicit claimed_by==nil guard, so a non-nil error
// or raced==true at those sites specifically indicates a SQL CAS miss, not an
// operator configuration problem.
//
// Do NOT use for planStore: that path's inline loop deliberately produces
// codeNoBin (terminal) regardless of whether the CAS failed or the bin was
// pre-filtered. See the TODO(Phase1) comment in planStore.
func asPlanningError(err error, detail string) *planningError {
	return &planningError{Code: codeClaimFailed, Detail: detail, Err: err}
}

// planningError code values. These strings are matched as literals at producer
// and consumer sites (the Transient() switch, the complex-dispatch router) and
// serialize verbatim into the orders.queue_reason / skip-reason DB columns, so
// the values are part of a persisted, compared contract: renaming a constant is
// safe, changing the string it holds is not.
const (
	codeUnknownType   = "unknown_type"
	codeStructural    = "structural"
	codeLoaderSource  = "loader_source"
	codeNode          = "node_error"
	codeClaimFailed   = "claim_failed"
	codeLaneLocked    = "lane_locked"
	codeReshuffle     = "reshuffle_error"
	codeMissingSource = "missing_source"
	codeInvalidNode   = "invalid_node"
	codeSameNode      = "same_node"
	codeNoPayload     = "no_payload"
	codeNoBin         = "no_bin"
	codeNoStorage     = "no_storage"
	codeNoSourceBin   = "no_source_bin"
)

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

// Transient reports whether the planning failure is contention that clears on its
// own, so the order must be QUEUED for the fulfillment scanner to retry rather than
// terminally failed:
//   - claim_failed: a source bin existed but was claimed by a concurrent order in the
//     TOCTOU gap between FindSourceBin and ClaimBin.
//   - lane_locked: the buried source bin's lane is mid-reshuffle for another order.
//
// Both resolve within moments. Failing them drops an order that just needed to wait —
// and multi-window loaders pulling empties in parallel make this contention routine.
// The reshuffle/complex dispatch path already queues lane_locked; Transient() makes
// every simple-planner path (retrieve, store, ingest) agree.
func (e *planningError) Transient() bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case codeClaimFailed, codeLaneLocked:
		return true
	}
	return false
}

type PlanningHandler func(order *orders.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError)

// plannerLifecycle is the narrow lifecycle surface the planning service
// depends on. *LifecycleService satisfies this interface for free
// (structural). Declared at the consumer side so the planner's
// dependency on lifecycle is exactly the methods it actually invokes.
type plannerLifecycle interface {
	MoveToSourcing(ord *orders.Order, actor, reason string) error
}

type PlanningService struct {
	db          *store.DB
	resolver    NodeResolver
	laneLock    *LaneLock
	binManifest *service.BinManifestService
	debug       func(string, ...any)
	lifecycle   plannerLifecycle

	createCompound func(parentOrder *orders.Order, plan *ReshufflePlan) error

	handlers map[protocol.OrderType]PlanningHandler

	// postFindHook is called after a bin lookup succeeds but before the claim.
	// Nil by default ( no-op in production. Set via SetPostFindHook for tests
	// to widen the the TOCTOU race window for deterministic concurrent testing.
	postFindHook func()
}

func newPlanningService(db *store.DB, resolver NodeResolver, laneLock *LaneLock, binManifest *service.BinManifestService, lifecycle plannerLifecycle, debug func(string, ...any), createCompound func(*orders.Order, *ReshufflePlan) error) *PlanningService {
	s := &PlanningService{
		db:             db,
		resolver:       resolver,
		laneLock:       laneLock,
		binManifest:    binManifest,
		debug:          debug,
		lifecycle:      lifecycle,
		createCompound: createCompound,
		handlers:       make(map[protocol.OrderType]PlanningHandler),
	}
	s.Register(OrderTypeRetrieve, s.planRetrieve)
	s.Register(OrderTypeRetrieveEmpty, s.planRetrieveEmpty)
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

func (s *PlanningService) Register(orderType protocol.OrderType, handler PlanningHandler) {
	s.handlers[orderType] = handler
}

func (s *PlanningService) Plan(order *orders.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	handler, ok := s.handlers[order.OrderType]
	if !ok {
		return nil, &planningError{
			Code:   codeUnknownType,
			Detail: fmt.Sprintf("unknown order type: %s", order.OrderType),
		}
	}
	return handler(order, env, payloadCode)
}

func (s *PlanningService) planRetrieve(order *orders.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	// Phase 4 of bin-transit-state: dropoff-capacity gate before any
	// state transition. Self-exclusion (order.ID) prevents the order's
	// own pending row from counting against itself in the in-flight
	// tally. If blocked, queue without claiming a source bin — the
	// fulfillment scanner replays when slot vacancy fires.
	if blocked, reason := CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		s.dbg("retrieve: order %d queued — %s", order.ID, reason)
		if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
			log.Printf("dispatch: set queue_reason for order %d: %v", order.ID, err)
		}
		return &PlanningResult{Queued: true}, nil
	}

	if err := s.lifecycle.MoveToSourcing(order, "planner", "finding source"); err != nil {
		log.Printf("dispatch: planRetrieve order %d → sourcing: %v", order.ID, err)
	}

	var source *bins.Bin
	var sourceNode *nodes.Node

	if order.SourceNode != "" && s.resolver != nil {
		// `srcGroup` (not `sourceNode`) so the success-path write at the
		// bottom of this block lands in the outer `sourceNode` rather than
		// a shadow — the shadow form panicked on `sourceNode.Name` below
		// once unloader auto-push lit up the NGRP retrieve path.
		srcGroup, err := s.db.GetNodeByDotName(order.SourceNode)
		if err == nil && srcGroup.IsSynthetic && srcGroup.NodeTypeCode == protocol.NodeClassNGRP {
			result, err := s.resolver.Resolve(srcGroup, OrderTypeRetrieve, payloadCode, nil)
			if err != nil {
				// Route through the same classifier the complex-
				// intake path uses. Behavior unchanged on this
				// surface — buried → planBuriedReshuffle, structural
				// → terminal planningError, capacity → queue. The
				// classifier just replaces the two open-coded
				// errors.As blocks.
				switch class, payload := classifyResolutionError(err); class {
				case ResolutionBuried:
					buriedErr := payload.(*BuriedError)
					s.dbg("retrieve: bin %d buried in lane %d, planning reshuffle", buriedErr.Bin.ID, buriedErr.LaneID)
					return s.planBuriedReshuffle(order, buriedErr)
				case ResolutionStructural:
					structErr := payload.(*StructuralError)
					s.dbg("retrieve: STRUCTURAL failure in group %s: %s",
						order.SourceNode, structErr.Reason)
					return nil, &planningError{
						Code:   codeStructural,
						Detail: structErr.Error(),
						Err:    structErr,
					}
				default:
					// ResolutionCapacity, Transient, and Fatal all
					// queue here today. The pre-v7 path made the
					// same call — anything not buried-or-structural
					// fell through to the queue branch. Preserve.
					s.dbg("retrieve: no source in group %s for payload=%s, queuing order %d", order.SourceNode, payloadCode, order.ID)
					return &PlanningResult{Queued: true}, nil
				}
			}
			source = result.Bin
			sourceNode, _ = s.db.GetNode(*source.NodeID)
		}
	}

	// Dedicated home loader: a concrete position node sources from its loader's
	// whole pool (every home position ∪ the kept-partial buffer), oldest part X
	// first — so a partial parked in the buffer is consumed before a fresh full.
	// Only fires when the source IS a loader position; any other node leaves
	// source nil and falls through to the global FIFO scan below.
	if source == nil && order.SourceNode != "" {
		bin, bnode, isLoaderPos, lerr := s.sourceFromDedicatedLoader(order.SourceNode, payloadCode, binsource.Drain)
		if lerr != nil {
			return nil, &planningError{Code: codeLoaderSource, Detail: lerr.Error(), Err: lerr}
		}
		if isLoaderPos {
			if bin == nil {
				// Loader position with no eligible bin of X — queue; do NOT fall
				// through to the global scan, which would pull plant-wide.
				s.dbg("retrieve: loader pool for %s has no %s, queuing order %d", order.SourceNode, payloadCode, order.ID)
				return &PlanningResult{Queued: true}, nil
			}
			source, sourceNode = bin, bnode
		}
	}

	if source == nil {
		// Resolve destination first so the source-finder can exclude it —
		// prevents same-node retrieve when a matching bin is already at the
		// destination. See SHINGO_TODO.md "Same-node retrieve" entry.
		var excludeNodeID int64
		if order.DeliveryNode != "" {
			if destNode, err := s.db.GetNodeByDotName(order.DeliveryNode); err == nil && destNode != nil {
				excludeNodeID = destNode.ID
			}
		}
		var err error
		source, err = s.db.FindSourceBinFIFO(payloadCode, excludeNodeID)
		if err != nil {
			s.dbg("retrieve: no source bin for payload=%s, queuing order %d", payloadCode, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
		sourceNode, err = s.db.GetNode(*source.NodeID)
		if err != nil {
			return nil, &planningError{Code: codeNode, Detail: err.Error(), Err: err}
		}
	}

	s.dbg("retrieve: FIFO source bin=%d payload=%s node=%s", source.ID, payloadCode, sourceNode.Name)
	if s.postFindHook != nil {
		s.postFindHook()
	}
	remainingUOP := extractRemainingUOP(env)
	if err := s.binManifest.ClaimForDispatch(source.ID, order.ID, remainingUOP); err != nil {
		return nil, asPlanningError(err, err.Error())
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
		return nil, &planningError{Code: codeNode, Detail: err.Error(), Err: err}
	}
	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
}

// planRetrieveEmpty is registered against OrderTypeRetrieveEmpty. The env
// parameter is part of the PlanningHandler contract and is unused here —
// retrieve_empty has no envelope fields beyond what's already on the order.
func (s *PlanningService) planRetrieveEmpty(order *orders.Order, _ *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	// Same prelude as planRetrieve: dropoff-capacity gate + sourcing transition.
	// Used to ride on planRetrieve's prelude when retrieve_empty was a payload_desc
	// sniff; now that it's registered as its own handler it needs its own.
	if blocked, reason := CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		s.dbg("retrieve_empty: order %d queued — %s", order.ID, reason)
		if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
			log.Printf("dispatch: set queue_reason for order %d: %v", order.ID, err)
		}
		return &PlanningResult{Queued: true}, nil
	}

	if err := s.lifecycle.MoveToSourcing(order, "planner", "finding source"); err != nil {
		log.Printf("dispatch: planRetrieveEmpty order %d → sourcing: %v", order.ID, err)
	}

	var bin *bins.Bin

	// Destination resolution is shared by both source-group and fallback
	// paths — used for excludeNodeID (prevent same-node retrieve) and
	// preferZone (zone-preferring fallback). Hoisted out of the inner
	// branches so the fallback can reuse it without re-querying.
	var preferZone string
	var excludeNodeID int64
	if order.DeliveryNode != "" {
		if destNode, derr := s.db.GetNodeByDotName(order.DeliveryNode); derr == nil && destNode != nil {
			preferZone = destNode.Zone
			excludeNodeID = destNode.ID
		}
	}

	// Dedicated home loader (Fill): a concrete position node sources a CONTAINER
	// for X from the loader's pool — a partial of X to top up (oldest), else the
	// cheapest empty. Mirrors planRetrieve's Drain branch, with Fill intent. The
	// claim below is plain (no manifest change), so a topped-up partial keeps its
	// X manifest; core completion moves the bin without clearing it. A non-loader
	// (or NGRP/LANE) source falls through to the supermarket/global empty finders.
	if order.SourceNode != "" {
		chosen, _, isLoaderPos, lerr := s.sourceFromDedicatedLoader(order.SourceNode, payloadCode, binsource.Fill)
		if lerr != nil {
			return nil, &planningError{Code: codeLoaderSource, Detail: lerr.Error(), Err: lerr}
		}
		if isLoaderPos {
			if chosen == nil {
				s.dbg("retrieve_empty: loader pool for %s has no container for %s, queuing order %d", order.SourceNode, payloadCode, order.ID)
				return &PlanningResult{Queued: true}, nil
			}
			bin = chosen
		}
	}

	// Source-group resolution. When the edge sends order.SourceNode (e.g. a
	// bin_loader claim's InboundSource), restrict the empty-bin search to
	// descendants of that NGRP. Without this a Hopkinsville-style multi-
	// supermarket setup pulls empties from whichever supermarket has the
	// lowest bins.id — including the empty-tote return area instead of the
	// configured pickup. Uses a dedicated group-scoped reader rather than
	// reusing GroupResolver.ResolveRetrieve, because that path applies the
	// payload-match-required semantics of isBinAvailableForRetrieve, which
	// rejects empties (PayloadCode == "" != payloadCode).
	if order.SourceNode != "" {
		sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
		// Accept either a supermarket NGRP or a LANE directly as the empties
		// source container. FindEmptyCompatibleBinInGroup recurses descendants,
		// so a LANE root scopes the search to that lane's own slots. Operators
		// doing a manual empty pull may pick the lane node itself (not just the
		// parent group) — both must resolve to the scoped reader rather than
		// falling through to the global any-zone finder below.
		if err == nil && sourceNode != nil && sourceNode.IsSynthetic &&
			(sourceNode.NodeTypeCode == protocol.NodeClassNGRP || sourceNode.NodeTypeCode == protocol.NodeClassLANE) {
			groupBin, gerr := s.db.FindEmptyCompatibleBinInGroup(payloadCode, sourceNode.ID, excludeNodeID)
			if gerr != nil {
				s.dbg("retrieve_empty: no empty in group %s for payload=%s, queuing order %d",
					order.SourceNode, payloadCode, order.ID)
				return &PlanningResult{Queued: true}, nil
			}
			bin = groupBin
		}
	}

	if bin == nil {
		var err error
		bin, err = s.db.FindEmptyCompatibleBin(payloadCode, preferZone, excludeNodeID)
		if err != nil {
			s.dbg("retrieve_empty: no bin for payload=%s, queuing order %d", payloadCode, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
	}
	s.dbg("retrieve_empty: found bin=%d label=%s at node=%s", bin.ID, bin.Label, bin.NodeName)

	// Last-resort reshuffle. The empty finder now prefers accessible (lane-mouth)
	// empties (bins.AccessibleEmptyOrder), so a bin that lands here buried means
	// EVERY compatible empty was buried — dig this one out rather than fail the
	// order. Before the 2026-06-13 accessibility ordering this fired routinely
	// (the finder picked by bin id, lane-blind); now it is the rare fallback.
	if bin.NodeID != nil {
		accessible, accErr := s.db.IsSlotAccessible(*bin.NodeID)
		if accErr == nil && !accessible {
			slot, slotErr := s.db.GetNode(*bin.NodeID)
			if slotErr == nil && slot.ParentID != nil {
				lane, laneErr := s.db.GetNode(*slot.ParentID)
				if laneErr == nil && lane.NodeTypeCode == protocol.NodeClassLANE {
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
		return nil, asPlanningError(err, err.Error())
	}
	order.BinID = &bin.ID
	if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
		log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
	}
	sourceNode, err := s.db.GetNode(*bin.NodeID)
	if err != nil {
		return nil, &planningError{Code: codeNode, Detail: err.Error(), Err: err}
	}
	order.SourceNode = sourceNode.Name
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d source_node: %v", order.ID, err)
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, &planningError{Code: codeNode, Detail: err.Error(), Err: err}
	}
	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
}

func (s *PlanningService) planBuriedReshuffle(order *orders.Order, buried *BuriedError) (*PlanningResult, *planningError) {
	if s.laneLock.IsLocked(buried.LaneID) {
		return nil, &planningError{Code: codeLaneLocked, Detail: fmt.Sprintf("lane %d is locked by another reshuffle", buried.LaneID)}
	}
	lane, err := s.db.GetNode(buried.LaneID)
	if err != nil || lane.ParentID == nil {
		return nil, &planningError{Code: codeReshuffle, Detail: "cannot determine node group for lane", Err: err}
	}
	plan, err := PlanReshuffle(s.db, buried.Bin, buried.Slot, lane, *lane.ParentID)
	if err != nil {
		return nil, &planningError{Code: codeReshuffle, Detail: fmt.Sprintf("cannot plan reshuffle: %v", err), Err: err}
	}
	if !s.laneLock.TryLock(buried.LaneID, order.ID) {
		return nil, &planningError{Code: codeLaneLocked, Detail: "lane locked concurrently"}
	}
	if err := s.createCompound(order, plan); err != nil {
		s.laneLock.Unlock(buried.LaneID)
		return nil, &planningError{Code: codeReshuffle, Detail: fmt.Sprintf("cannot create compound order: %v", err), Err: err}
	}
	// createCompound already transitioned the parent to Reshuffling via
	// lifecycle.BeginReshuffle and dispatched the first child via the
	// tail AdvanceCompoundOrder call in CreateCompoundChildrenOnly — any
	// dispatch error from that path is surfaced through the createCompound
	// error wrap above. Do NOT add a second advanceCompound here: stacking
	// two advances within milliseconds dispatched a second child before
	// the first left the dock on the 2026-05-27 production reshuffle.
	s.dbg("retrieve: compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))
	return &PlanningResult{Handled: true}, nil
}

func (s *PlanningService) planMove(order *orders.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	if order.SourceNode == "" {
		return nil, &planningError{Code: codeMissingSource, Detail: "move order requires source_node"}
	}
	sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
	if err != nil {
		return nil, &planningError{Code: codeInvalidNode, Detail: fmt.Sprintf("source node %q not found", order.SourceNode), Err: err}
	}
	// Same-node validation must come before the capacity gate: a
	// same-node move is invalid regardless of capacity (would produce
	// a fleet order with src == dst, which the fleet cancels). Failing
	// fast surfaces the bug at submit time rather than letting the
	// order sit queued forever on a "destination occupied" reason that
	// would never clear.
	destPreCheck, dErr := s.db.GetNodeByDotName(order.DeliveryNode)
	if dErr == nil && destPreCheck != nil && sourceNode.ID == destPreCheck.ID {
		return nil, &planningError{Code: codeSameNode, Detail: fmt.Sprintf("source and destination are the same node (%s)", sourceNode.Name)}
	}

	// Phase 4 of bin-transit-state: dropoff-capacity gate. Move orders
	// (returns to NGRP supermarkets, side-cycle L2/U2, manual moves,
	// auto-returns) all flow through this planner; gating here closes
	// the race they otherwise have with concurrent dispatches to the
	// same destination.
	if blocked, reason := CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		s.dbg("move: order %d queued — %s", order.ID, reason)
		if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
			log.Printf("dispatch: set queue_reason for order %d: %v", order.ID, err)
		}
		return &PlanningResult{Queued: true}, nil
	}

	if err := s.lifecycle.MoveToSourcing(order, "planner", "validating move"); err != nil {
		log.Printf("dispatch: planMove order %d → sourcing: %v", order.ID, err)
	}

	// If the source is a synthetic NGRP (supermarket group), resolve to a
	// concrete bin within the group. Without this, ListBinsByNode on the NGRP
	// returns zero bins (they live at child slots, not on the NGRP itself),
	// causing the order to dispatch without a bin claim. On completion the
	// bin's DB location would never update — it'd still show the old slot.
	//
	// We reuse OrderTypeRetrieve semantics: finding the best bin in an NGRP
	// for a move-from-supermarket is the same operation as a retrieve.
	if sourceNode.IsSynthetic && sourceNode.NodeTypeCode == protocol.NodeClassNGRP && s.resolver != nil {
		result, rErr := s.resolver.Resolve(sourceNode, OrderTypeRetrieve, payloadCode, nil)
		if rErr != nil {
			switch class, payload := classifyResolutionError(rErr); class {
			case ResolutionBuried:
				buriedErr := payload.(*BuriedError)
				s.dbg("move: bin %d buried in lane %d, planning reshuffle", buriedErr.Bin.ID, buriedErr.LaneID)
				return s.planBuriedReshuffle(order, buriedErr)
			case ResolutionStructural:
				structErr := payload.(*StructuralError)
				s.dbg("move: STRUCTURAL failure in group %s: %s (falling through to queue)",
					order.SourceNode, structErr.Reason)
				fallthrough
			default:
				s.dbg("move: no source in group %s for payload=%s, queuing order %d", order.SourceNode, payloadCode, order.ID)
				return &PlanningResult{Queued: true}, nil
			}
		}
		if result.Bin != nil {
			remainingUOP := extractRemainingUOP(env)
			if err := s.binManifest.ClaimForDispatch(result.Bin.ID, order.ID, remainingUOP); err != nil {
				return nil, asPlanningError(err, err.Error())
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
				return nil, &planningError{Code: codeNode, Detail: fmt.Sprintf("resolve slot for bin %d: %v", result.Bin.ID, cErr), Err: cErr}
			}
			sourceNode = concreteNode
			s.dbg("move: NGRP resolved bin=%d at %s (remainingUOP=%v)", result.Bin.ID, sourceNode.Name, remainingUOP)
		} else {
			// Resolver returned a node but no specific bin — queue and retry.
			s.dbg("move: NGRP resolved node %s but no bin, queuing order %d", result.Node.Name, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
	} else if bin, bnode, isLoaderPos, lerr := s.sourceFromDedicatedLoader(order.SourceNode, payloadCode, binsource.Drain); lerr != nil {
		return nil, &planningError{Code: codeLoaderSource, Detail: lerr.Error(), Err: lerr}
	} else if isLoaderPos && payloadCode != "" {
		// Dedicated-loader position, part-keyed move: source the loader's whole
		// pool (every home position ∪ the kept-partial buffer), oldest part X
		// first — same as planRetrieve. A move-mode consume cell (swap dispatch
		// issues a move, not a retrieve) reaches a partial parked in the buffer
		// this way. No eligible bin of X → queue; do NOT fall through to the
		// single-node claim, which would only see the position and miss the buffer.
		//
		// Empty payload is excluded on purpose: a payload-less move is a direct
		// relocation of the physical bin sitting AT the position (a manual move,
		// or a true-empty carrier the operator is shuffling), not a part-keyed
		// pool source — pool sourcing keys on the part and can't resolve an empty,
		// so it would queue and then hard-fail on the fulfiller's empty-payload
		// guard. Fall through to the concrete-node claim below, which claims the
		// bin actually parked there regardless of payload (BinUnavailableReason
		// skips the payload check when the order payload is blank) — the same path
		// a move from any non-loader node already uses.
		if bin == nil {
			s.dbg("move: loader pool for %s has no %s, queuing order %d", order.SourceNode, payloadCode, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
		remainingUOP := extractRemainingUOP(env)
		if err := s.binManifest.ClaimForDispatch(bin.ID, order.ID, remainingUOP); err != nil {
			return nil, asPlanningError(err, err.Error())
		}
		order.BinID = &bin.ID
		if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
			log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
		}
		sourceNode = bnode
		s.dbg("move: loader pool sourced bin=%d at %s (remainingUOP=%v)", bin.ID, sourceNode.Name, remainingUOP)
	} else {
		// Concrete source node: claim a bin directly at the node.
		candidates, _ := s.db.ListBinsByNode(sourceNode.ID)
		remainingUOP := extractRemainingUOP(env)
		picked, rejects, raced := claimFirstAvailable(candidates, payloadCode, func(b *bins.Bin) error {
			return s.binManifest.ClaimForDispatch(b.ID, order.ID, remainingUOP)
		})
		if picked == nil {
			detail := fmt.Sprintf("no unclaimed bin at %s for move order %d (evaluated %d bin(s); rejects: [%s])",
				order.SourceNode, order.ID, len(candidates), joinRejects(rejects))
			s.dbg("move: order %d at %s — %s", order.ID, order.SourceNode, detail)
			// Race-loss vs structural-unavailable discrimination (#4).
			// claim_failed is retry-eligible; the caller (planning
			// service plan loop) treats it as a queue signal so the
			// scanner re-tries on the next tick.
			if raced {
				return nil, asPlanningError(nil, detail)
			}
			if payloadCode != "" {
				return nil, &planningError{Code: codeNoPayload, Detail: fmt.Sprintf("no unclaimed %s bin at %s", payloadCode, order.SourceNode)}
			}
			// Safety net: a move order without a claimed bin would silently
			// dispatch to the fleet, but handleOrderCompleted would skip the
			// bin arrival update (BinID == nil). Fail loudly instead.
			return nil, &planningError{Code: codeNoBin, Detail: detail}
		}
		order.BinID = &picked.ID
		if err := s.db.UpdateOrderBinID(order.ID, picked.ID); err != nil {
			s.dbg("move: WARNING order %d UpdateOrderBinID(bin=%d) failed — order.BinID will read NULL on next load: %v",
				order.ID, picked.ID, err)
		}
		s.dbg("move: claimed bin=%d at %s (remainingUOP=%v)", picked.ID, order.SourceNode, remainingUOP)
	}

	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d source_node: %v", order.ID, err)
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, &planningError{Code: codeNode, Detail: err.Error(), Err: err}
	}
	// If the destination is still a synthetic NGRP, resolve a concrete child
	// slot now. This happens when intake (CreateInboundOrder) deferred
	// resolution because the group was full: the order was created against the
	// group and queued by the CheckDropoffCapacity gate above, and by the time
	// it dispatches a slot has freed. Mirrors the synthetic-source resolution
	// earlier in this method. CheckDropoffCapacity already queued the all-full
	// case, so a resolver failure here is a TOCTOU race — re-queue and let the
	// scanner retry rather than failing the order.
	if destNode.IsSynthetic && destNode.NodeTypeCode == protocol.NodeClassNGRP && s.resolver != nil {
		result, rErr := s.resolver.Resolve(destNode, OrderTypeStore, payloadCode, nil)
		if rErr != nil {
			s.dbg("move: dest group %s unresolved at dispatch (%v), queuing order %d", order.DeliveryNode, rErr, order.ID)
			return &PlanningResult{Queued: true}, nil
		}
		s.dbg("move: dest NGRP %s resolved -> %s for order %d", order.DeliveryNode, result.Node.Name, order.ID)
		destNode = result.Node
		order.DeliveryNode = destNode.Name
		if err := s.db.UpdateOrderDeliveryNode(order.ID, destNode.Name); err != nil {
			log.Printf("dispatch: update order %d delivery_node: %v", order.ID, err)
		}
	}
	// Guard: source and destination must differ. A same-node move is physically
	// impossible and would waste a fleet transport order.
	if sourceNode.ID == destNode.ID {
		return nil, &planningError{Code: codeSameNode, Detail: fmt.Sprintf("source and destination are the same node (%s)", sourceNode.Name)}
	}
	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
}

func (s *PlanningService) planStore(order *orders.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	if err := s.lifecycle.MoveToSourcing(order, "planner", "finding storage destination"); err != nil {
		log.Printf("dispatch: planStore order %d → sourcing: %v", order.ID, err)
	}

	// Round-3 follow-up: resolve sourceNode BEFORE FindStorageDestination
	// so we can exclude it from the consolidation pick. Pre-fix the
	// consolidation branch happily returned the source itself when the
	// source still held the bin being stored — produced same-node store
	// orders that pre-Item-C dispatched as ghost moves and post-Item-C
	// queued forever (the destination was always "occupied" by the
	// bin we were trying to move).
	originalDeliveryNode := order.DeliveryNode
	var (
		sourceNode *nodes.Node
		err        error
	)
	if order.SourceNode != "" {
		sourceNode, err = s.db.GetNodeByDotName(order.SourceNode)
		if err != nil {
			return nil, &planningError{Code: codeInvalidNode, Detail: fmt.Sprintf("source node %q not found", order.SourceNode), Err: err}
		}
	} else if originalDeliveryNode != "" {
		sourceNode, err = s.db.GetNodeByDotName(originalDeliveryNode)
		if err != nil {
			return nil, &planningError{Code: codeInvalidNode, Detail: fmt.Sprintf("node %q not found", originalDeliveryNode), Err: err}
		}
	}
	if sourceNode == nil {
		return nil, &planningError{Code: codeMissingSource, Detail: "store order requires a source location"}
	}

	destNode, err := s.db.FindStorageDestination(payloadCode, sourceNode.ID)
	if err != nil {
		return nil, &planningError{Code: codeNoStorage, Detail: "no available storage node found", Err: err}
	}
	s.dbg("store: selected destination=%s for order %d", destNode.Name, order.ID)
	order.DeliveryNode = destNode.Name
	if err := s.db.UpdateOrderDeliveryNode(order.ID, destNode.Name); err != nil {
		log.Printf("dispatch: update order %d delivery_node: %v", order.ID, err)
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
		// TODO(Phase1): decide claim semantics before routing through the seam.
		// planStore's loop guards with `if bin.ClaimedBy == nil` before calling
		// ClaimForDispatch, so a CAS failure here is either a race (same TOCTOU
		// window as other paths) or a pre-filter miss — the two are indistinguishable
		// without a raced-flag. For now the terminal codeNoBin is preserved; Phase 1
		// can add a raced flag here once the seam has reservation semantics.
		return nil, &planningError{Code: codeNoBin, Detail: fmt.Sprintf("no available bin at %s", sourceNode.Name)}
	}
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d source_node: %v", order.ID, err)
	}

	// Round-3 Item C: dropoff-capacity gate AFTER source claim. Sequencing
	// matters — putting the gate before source claim would queue orders
	// that have a bin available but a destination that's currently full,
	// which is correct in isolation but breaks the existing changeover
	// pattern: operators clear a line with N store orders, each expecting
	// to claim one bin and dispatch when capacity opens up. With the gate
	// before the claim, all N orders sit queued without bin claims, so
	// the fulfillment scanner re-races every replay; with the gate after,
	// each order owns its bin and waits politely for storage to open.
	// Mirrors the pattern from planRetrieve/planRetrieveEmpty/planMove,
	// only those run the gate first because their source claim is a
	// separate code path. The gate uses the order ID as
	// excludeOrderID so this order's own pending state doesn't
	// self-collide.
	if blocked, reason := CheckDropoffCapacity(s.db, destNode.Name, order.ID); blocked {
		s.dbg("store: order %d queued — %s", order.ID, reason)
		if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
			log.Printf("dispatch: set queue_reason for order %d: %v", order.ID, err)
		}
		return &PlanningResult{Queued: true}, nil
	}

	return &PlanningResult{SourceNode: sourceNode, DestNode: destNode}, nil
}
