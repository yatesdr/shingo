package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
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

	// Plan is the order-builder plan (a resolvedStep list) for a plan-shaped simple
	// family. It is EMITTED here but NOT consumed by dispatch and NOT persisted in
	// this step: the plain path builds its fleet request from the order columns
	// (SourceNode/DeliveryNode/BinID), and every StepsJSON reader is IsCoordinated-
	// gated, so a simple plan has no reader. Persisting + consuming the plan is the
	// follow-up where the dispatch tail unifies and reads it; the discriminator is
	// the order.Coordinated column, never steps-presence. A differential test pins
	// the plan fleet-equivalent to the transport tail. nil for non-dispatch
	// dispositions (queued/handled), which carry no plan.
	Plan []resolvedStep
}

type planningError struct {
	Code   string
	Detail string
	Err    error
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
	msg := e.Detail
	if e.Err != nil {
		msg = e.Err.Error()
	}
	// Code is a persisted, compared contract (see the type doc) — carry it in the
	// error text so a logged/wrapped planningError names its code.
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, msg)
	}
	return msg
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

// PlanningService validates + resolves a simple order at intake and QUEUES it —
// the claim-move to the fulfillment scanner made the scanner the single bin
// claimer, so the planner no longer claims, syncs manifests, or transitions the
// order to sourcing. Its remaining intake-only jobs are the shared capacity gate,
// move's named-source validations + concrete-dest resolution, and pivoting a
// buried source to a reshuffle compound (reshuffle planning lives at intake; the
// scanner only re-queues). It therefore no longer depends on the bin-manifest or
// lifecycle services.
type PlanningService struct {
	db       *store.DB
	resolver NodeResolver
	finder   *SourceFinder
	laneLock *LaneLock
	debug    func(string, ...any)

	createCompound func(parentOrder *orders.Order, plan *ReshufflePlan) error

	handlers map[protocol.OrderType]PlanningHandler
}

func newPlanningService(db *store.DB, resolver NodeResolver, laneLock *LaneLock, debug func(string, ...any), createCompound func(*orders.Order, *ReshufflePlan) error) *PlanningService {
	s := &PlanningService{
		db:             db,
		resolver:       resolver,
		finder:         NewSourceFinder(db, resolver, debug),
		laneLock:       laneLock,
		debug:          debug,
		createCompound: createCompound,
		handlers:       make(map[protocol.OrderType]PlanningHandler),
	}
	// One planTransport folds the three simple families (retrieve,
	// retrieve_empty, move). The handler map is type-keyed, but planTransport
	// reads order.SourceIntent (the label→data field), so one handler serves all
	// three types.
	s.Register(OrderTypeRetrieve, s.planTransport)
	s.Register(OrderTypeRetrieveEmpty, s.planTransport)
	s.Register(OrderTypeMove, s.planTransport)
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

// planTransport is the single planner for the three "simple" transport families —
// retrieve, retrieve_empty, and move — folded into one. It parameterizes on
// order.SourceIntent (the label→data field stamped once at intake by
// SourceIntentForType): SourceIntentEmpty sources an empty carrier (IntentEmpty,
// Empty pickup step); SourceIntentLocal is a node-local move (an explicit branch
// carrying move's own source_node/same-node validations and synthetic-NGRP-dest
// resolution); SourceIntentFull is a payload-matched retrieve.
//
// The claim-move to the scanner: intake does NOT claim the bin — the fulfillment
// scanner is the SINGLE claim point (the model complex has run since birth:
// status-first queued → scanner claims at dispatch). planTransport validates,
// gates capacity, resolves the source, resolves a move's concrete dest, then
// QUEUES; the scanner re-finds + claims + reserves + dispatches. Source resolution
// STAYS at intake for two dispositions the scanner cannot produce: a BURIED source
// pivots to a reshuffle compound (reshuffle planning lives at intake — the scanner
// only re-queues), and a WAIT/STRUCTURAL outcome sets the queue reason / terminal
// error. On Found the resolved sourceNode is ADVISORY (for the shadow plan); the
// scanner's re-find is authoritative. The one datum the scanner cannot recompute —
// the operator's declared release-correction count (RemainingUOP, carried only by a
// move) — is persisted onto the order so the scanner's claim seeds the same
// manifest sync.
func (s *PlanningService) planTransport(order *orders.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
	isEmpty := order.SourceIntent == SourceIntentEmpty
	isMove := order.SourceIntent == SourceIntentLocal

	// Persist the operator's declared release-correction count onto the order so the
	// scanner — the single claim point, which has no envelope — seeds the same
	// atomic claim+manifest-sync (bin_manifest.ClaimForDispatch: nil=plain claim,
	// >0 syncs, <=0 clears). In practice only a move carries it
	// (CreateMoveOrderWithUOP → OrderRequest.RemainingUOP); retrieve carries none,
	// and an empty carrier forces nil (the bin is already empty). Bridge column:
	// the unified-create follow-up carries the count in the persisted plan and this
	// write retires.
	var remainingUOP *int
	if !isEmpty {
		remainingUOP = extractRemainingUOP(env)
	}
	order.RemainingUOP = remainingUOP
	if err := s.db.UpdateOrderRemainingUOP(order.ID, remainingUOP); err != nil {
		log.Printf("dispatch: persist remaining_uop for order %d: %v", order.ID, err)
	}

	// move's named-source validations are load-bearing and MUST run BEFORE the
	// shared capacity gate: a missing/same-node move is invalid regardless of
	// capacity (it would produce a fleet order with src == dst, which the fleet
	// cancels). Failing fast surfaces the bug at submit time rather than letting
	// the order sit queued forever on a reason that would never clear.
	if isMove {
		if order.SourceNode == "" {
			return nil, &planningError{Code: codeMissingSource, Detail: "move order requires source_node"}
		}
		moveSrc, err := s.db.GetNodeByDotName(order.SourceNode)
		if err != nil {
			return nil, &planningError{Code: codeInvalidNode, Detail: fmt.Sprintf("source node %q not found", order.SourceNode), Err: err}
		}
		if destPreCheck, dErr := s.db.GetNodeByDotName(order.DeliveryNode); dErr == nil && destPreCheck != nil && moveSrc.ID == destPreCheck.ID {
			return nil, &planningError{Code: codeSameNode, Detail: fmt.Sprintf("source and destination are the same node (%s)", moveSrc.Name)}
		}
	}

	// Phase 4 of bin-transit-state: shared dropoff-capacity gate. Self-exclusion
	// (order.ID) keeps the order's own pending row out of the in-flight tally.
	// Blocked → queue; the scanner replays when slot vacancy fires.
	if blocked, reason := CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		s.dbg("transport: order %d queued — %s", order.ID, reason)
		if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
			log.Printf("dispatch: set queue_reason for order %d: %v", order.ID, err)
		}
		return &PlanningResult{Queued: true}, nil
	}

	// Resolve the source through the shared SourceFinder — but do NOT claim (the
	// claim-move to the scanner: the scanner is the single claimer). We resolve here
	// for the two intake-only dispositions the scanner cannot produce: a buried
	// source pivots to a reshuffle compound (resolveSource → planBuriedReshuffle),
	// and a wait/structural outcome sets the queue reason / terminal error. The bin
	// is discarded — the scanner re-finds and claims authoritatively.
	intent := IntentFull
	if isEmpty {
		intent = IntentEmpty
	}
	_, sourceNode, pr, pe, ok := s.resolveSource(order, intent)
	if !ok {
		return pr, pe
	}
	s.dbg("transport: order %d source resolvable at node=%s (intent=%q) — queuing for scanner claim", order.ID, sourceNode.Name, order.SourceIntent)

	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return nil, &planningError{Code: codeNode, Detail: err.Error(), Err: err}
	}

	if isMove {
		// If the destination is still a synthetic NGRP, resolve a concrete child slot
		// now. Intake (CreateInboundOrder) deferred it because the group was full;
		// the scanner dispatches to order.DeliveryNode verbatim (it does not resolve
		// NGRPs), so the concrete resolution must land on the order here. On a
		// still-full group this is a TOCTOU race — re-queue and let the scanner retry.
		if destNode.IsSynthetic && destNode.NodeTypeCode == protocol.NodeClassNGRP && s.resolver != nil {
			result, rErr := s.resolver.Resolve(destNode, OrderTypeStore, payloadCode, nil)
			if rErr != nil {
				s.dbg("move: dest group %s unresolved (%v), queuing order %d", order.DeliveryNode, rErr, order.ID)
				return &PlanningResult{Queued: true}, nil
			}
			s.dbg("move: dest NGRP %s resolved -> %s for order %d", order.DeliveryNode, result.Node.Name, order.ID)
			destNode = result.Node
			order.DeliveryNode = destNode.Name
			if err := s.db.UpdateOrderDeliveryNode(order.ID, destNode.Name); err != nil {
				log.Printf("dispatch: update order %d delivery_node: %v", order.ID, err)
			}
		}
		// A same-node move is physically impossible and would waste a fleet order.
		if sourceNode.ID == destNode.ID {
			return nil, &planningError{Code: codeSameNode, Detail: fmt.Sprintf("source and destination are the same node (%s)", sourceNode.Name)}
		}
	}

	// The claim-move to the scanner: status-first queued. The scanner claims (with
	// the persisted RemainingUOP), reserves the dropoff, and dispatches — the single
	// claim point. The reserve asymmetry (only move reserved at intake) closes here
	// by DELETION: intake reserves nothing; the scanner reserves for every plain
	// family. The shadow plan rides the queued disposition (advisory; the scanner
	// re-resolves. The follow-up persists + consumes it — see PlanningResult.Plan).
	return &PlanningResult{
		Queued:     true,
		SourceNode: sourceNode,
		DestNode:   destNode,
		Plan:       buildTransportPlan(sourceNode.Name, destNode.Name, isEmpty),
	}, nil
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
