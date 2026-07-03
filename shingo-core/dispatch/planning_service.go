package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
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
	finder      *SourceFinder
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
		finder:         NewSourceFinder(db, resolver, debug),
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

// resolveSource runs the shared SourceFinder for one intent and maps its
// non-Found outcomes to caller return values. On OutcomeFound it returns the bin
// + its node with proceed=true; on every other outcome it returns the
// queue/reshuffle/terminal result with proceed=false, so the caller can:
//
//	source, sourceNode, pr, pe, ok := s.resolveSource(order, intent)
//	if !ok { return pr, pe }
//
// The disposition (queue vs reshuffle vs terminal) lives in the finder, so
// intake and scanner-replay can no longer drift on it. OutcomeWait now writes
// queue_reason (intake used to queue silently on no-source); OutcomeStructural
// re-raises the finder's TermCode verbatim (the codeStructural/codeLoaderSource/
// codeNode strings are the persisted contract intake already used).
func (s *PlanningService) resolveSource(order *orders.Order, intent Intent) (*bins.Bin, *nodes.Node, *PlanningResult, *planningError, bool) {
	res := s.finder.FindSource(order, intent)
	switch res.Outcome {
	case OutcomeFound:
		return res.Bin, res.Node, nil, nil, true
	case OutcomeReshuffle:
		pr, pe := s.planBuriedReshuffle(order, res.Buried)
		return nil, nil, pr, pe, false
	case OutcomeStructural:
		s.dbg("plan: order %d structural — %s: %s", order.ID, res.TermCode, res.Err)
		return nil, nil, nil, &planningError{Code: res.TermCode, Detail: res.Err.Error(), Err: res.Err}, false
	default: // OutcomeWait
		s.dbg("plan: order %d queued — %s", order.ID, res.QueueReason)
		if err := s.db.SetOrderQueueReason(order.ID, res.QueueReason); err != nil {
			log.Printf("dispatch: set queue_reason for order %d: %v", order.ID, err)
		}
		return nil, nil, &PlanningResult{Queued: true}, nil, false
	}
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

	// Source finding is delegated to the shared SourceFinder (NGRP resolver →
	// dedicated-loader pool → plant-wide FIFO, with the exclude-dest and no-
	// fall-through scoping baked in) so intake and scanner-replay can't drift.
	source, sourceNode, pr, pe, ok := s.resolveSource(order, IntentFull)
	if !ok {
		return pr, pe
	}

	s.dbg("retrieve: source bin=%d payload=%s node=%s", source.ID, payloadCode, sourceNode.Name)
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

	// Source finding is delegated to the shared SourceFinder (dedicated-loader
	// Fill pool → group/lane-scoped empty → plant-wide empty, with the last-
	// resort buried→reshuffle baked in as tier 6) so intake and scanner-replay
	// can't drift on the scoping that isolates multi-supermarket empties.
	bin, sourceNode, pr, pe, ok := s.resolveSource(order, IntentEmpty)
	if !ok {
		return pr, pe
	}
	s.dbg("retrieve_empty: found bin=%d label=%s at node=%s", bin.ID, bin.Label, sourceNode.Name)

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

	// Source finding via the shared SourceFinder. The move tiers live there —
	// NGRP resolver (concrete child slot), dedicated-loader Drain pool, and the
	// concrete-node candidate — so intake and scanner-replay pick the same bin
	// and can't drift. The finder is pure; the claim happens here.
	//
	// Behavior changes vs the old inline branches (intended, ratified with the
	// A6 fix direction — a queued move should hold-and-retry, not spuriously
	// terminal-fail):
	//   - the concrete-node tier is find-first-available-then-claim (was
	//     claimFirstAvailable's within-call multi-try); a claim race re-queues
	//     via claim_failed and the next scanner tick picks another candidate.
	//   - no available bin at the source node now QUEUES and holds-and-retries
	//     (was terminal codeNoBin/codeNoPayload) — demand is operator-driven and
	//     never evaporates, so the bin is expected to arrive.
	//   - a structural NGRP-source error stays TERMINAL, failing with the
	//     structural detail (was a move-specific fall-through-queue) — a config
	//     or human error never self-heals, so it must fail loudly rather than sit
	//     queued. The finder maps ResolutionStructural -> OutcomeStructural and
	//     resolveSource raises it as a terminal planningError, unifying move's
	//     disposition with planRetrieve.
	source, resolvedSource, pr, pe, ok := s.resolveSource(order, IntentFull)
	if !ok {
		return pr, pe
	}
	remainingUOP := extractRemainingUOP(env)
	if err := s.binManifest.ClaimForDispatch(source.ID, order.ID, remainingUOP); err != nil {
		return nil, asPlanningError(err, err.Error())
	}
	order.BinID = &source.ID
	if err := s.db.UpdateOrderBinID(order.ID, source.ID); err != nil {
		log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
	}
	// The finder returns the concrete slot (resolved NGRP child, loader
	// position, or concrete node) — the actual pickup location handleOrderCompleted
	// needs, never the NGRP name.
	sourceNode = resolvedSource
	s.dbg("move: sourced bin=%d at %s (remainingUOP=%v)", source.ID, sourceNode.Name, remainingUOP)

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
