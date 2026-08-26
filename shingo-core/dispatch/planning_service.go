package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
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
//
// Every one of these is the SAME STRING as a protocol.TermCode, and used to be
// spelled out a second time here — two vocabularies for one contract, kept equal
// by nobody. They are bound to the protocol constants now, so the compiler holds
// them together and the warning above has something enforcing it.
//
// There used to be one exception, codeLoaderSource, carrying the note "it never
// reaches a terminal row". It did: the finder raised it as a structural outcome,
// which every caller terminal-fails. It is gone now — a failed read of a loader
// pool waits (source_finder.go), so there is nothing left to classify — and with
// it the exception this list had to keep explaining.
//
// This is also where a terminal code on an order row comes from: the planner
// returns one of these, failOrder hands it to lifecycle.Fail, and it lands in
// the row's code column. protocol.TermSameNode looked like a declared value
// with no producer precisely because the producer spelled the string out again
// instead of naming it.
const (
	codeUnknownType   = string(protocol.TermUnknownType)
	codeStructural    = string(protocol.TermStructural)
	codeNode          = string(protocol.TermNodeError)
	codeClaimFailed   = string(protocol.TermClaimFailed)
	codeLaneLocked    = string(protocol.TermLaneLocked)
	codeReshuffle     = string(protocol.TermReshuffleError)
	codeMissingSource = string(protocol.TermMissingSource)
	codeInvalidNode   = string(protocol.TermInvalidNode)
	codeSameNode      = string(protocol.TermSameNode)
	codeNoPayload     = string(protocol.TermNoPayload)
	codeNoBin         = string(protocol.TermNoBin)
	codeNoStorage     = string(protocol.TermNoStorage)
	codeNoSourceBin   = string(protocol.TermNoSourceBin)
	// codeNoShuffleSlot is TRANSIENT: the reshuffle has nowhere to park blockers
	// right now. See ErrNoShuffleSlot.
	codeNoShuffleSlot = string(protocol.TermNoShuffleSlot)
	// codeBlockerClaimed is TRANSIENT: a bin the dig must move is held by an order
	// outside the compound. See store.BlockerClaimedError — the holder is a live
	// order, usually a robot mid-pickup, so the digger waits for it.
	codeBlockerClaimed = string(protocol.TermBlockerClaimed)
	// codeReadFailed is TRANSIENT: a read Core needed did not answer. Distinct
	// from codeInvalidNode, which is the same shape of question answered "there is
	// nothing there" — see read_vs_missing.go for why the two must not share a
	// disposition.
	codeReadFailed = string(protocol.TermReadFailed)
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
//   - no_shuffle_slot: the dig has nowhere to park its blockers right now.
//   - blocker_claimed: a bin the dig must move is held by an order outside the
//     compound.
//
// Failing any of them drops an order that just needed to wait — and multi-window
// loaders pulling empties in parallel make this contention routine. The
// reshuffle/complex dispatch path already queues lane_locked; Transient() makes
// every simple-planner path (retrieve, store, ingest) agree.
//
// They do NOT all clear on the same timescale, and the set must not be read as
// if they did. claim_failed is a lost race and clears in the next moment;
// blocker_claimed waits out a robot's drive time, minutes. Both WAIT — the
// disposition is the same — but a caller that retries on a tight loop is right
// for one and wasteful for the other, and anything that reports on these codes
// should keep them apart rather than average them into "contention".
func (e *planningError) Transient() bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case codeClaimFailed, codeLaneLocked, codeNoShuffleSlot, codeBlockerClaimed, codeReadFailed:
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

func newPlanningService(db *store.DB, resolver NodeResolver, finder *SourceFinder, laneLock *LaneLock, debug func(string, ...any), createCompound func(*orders.Order, *ReshufflePlan) error) *PlanningService {
	s := &PlanningService{
		db:             db,
		resolver:       resolver,
		finder:         finder,
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

// Handles reports whether a planner is registered for an order type — that is,
// whether Core can actually carry out an order of this kind.
//
// It exists so intake can refuse a type before writing a row, using the same
// map Plan consults rather than a second list of valid types written down
// somewhere else. A hand-maintained allow-list would be a copy of this, kept
// equal by nobody, and the first thing to disagree with it would be a planner
// registered at runtime.
func (s *PlanningService) Handles(t protocol.OrderType) bool {
	_, ok := s.handlers[t]
	return ok
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
// re-raises the finder's TermCode verbatim (the codeStructural / codeNode
// strings are the persisted contract intake already used).
func (s *PlanningService) resolveSource(order *orders.Order, intent Intent) (*bins.Bin, *nodes.Node, *PlanningResult, *planningError, bool) {
	res := s.finder.FindSource(order, intent)
	// mapFinderOutcome is the shared admission point (see finder_outcome.go):
	// unknown outcomes fail loudly there, so every arm here is explicit and
	// the old behavior-bearing default (anything-unknown parked as Wait,
	// forever, silently) is gone.
	switch MapFinderOutcome(res) {
	case OutcomeFound:
		return res.Bin, res.Node, nil, nil, true
	case OutcomeReshuffle:
		pr, pe := s.planBuriedReshuffle(order, res.Buried)
		return nil, nil, pr, pe, false
	case OutcomeStructural:
		s.dbg("plan: order %d structural — %s: %s", order.ID, res.TermCode, res.Err)
		return nil, nil, nil, &planningError{Code: res.TermCode, Detail: res.Err.Error(), Err: res.Err}, false
	default: // OutcomeWait — the only remaining member
		s.setQueueReason(order, res.QueueCode, QueueCause(res.QueueCause), res.QueueParams)
		return nil, nil, &PlanningResult{Queued: true}, nil, false
	}
}

// setQueueReason is the planning side's one door onto the queue-reason columns:
// it generates the operator sentence from code+params (via the shared formatter)
// and writes sentence+code+cause together. Mirrors the Dispatcher and Scanner
// helpers so every intake path parks through the same formatter, never free text.
// Best-effort: a failed write is logged and swallowed.
func (s *PlanningService) setQueueReason(order *orders.Order, code protocol.QueueCode, cause QueueCause, params QueueParams) {
	reason := FormatQueueSentence(code, params)
	if order.QueueReason == reason && order.QueueCode == string(code) {
		return
	}
	if err := s.db.SetOrderQueueDetail(order.ID, reason, code, string(cause)); err != nil {
		log.Printf("dispatch: set queue_reason (%s) for order %d: %v", cause, order.ID, err)
		return
	}
	order.QueueReason = reason
	order.QueueCode = string(code)
	order.QueueCause = string(cause)
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
// resolves the source, gates capacity, resolves a move's concrete dest, then
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
	//
	// This gate MUST stay above source resolution. A simple-retrieve reshuffle
	// compound IS the delivery — PlanReshuffle's retrieve step leaves ToNode nil so
	// compound.go defaults it to the parent's DeliveryNode — and compound children
	// are dispatched by AdvanceCompoundOrder, which never re-checks capacity (the
	// scanner skips anything carrying a ParentOrderID). So resolving the source
	// first lets a buried retrieve plan a compound that drives a bin into an
	// occupied line, laundering the deadlock_gate_test invariant through the
	// compound machinery. Gate first, then resolve.
	if blocked, cap := CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		s.dbg("transport: order %d queued — %s", order.ID, cap.Cause)
		s.setQueueReason(order, protocol.QueueWaitingForSlot, QueueCause(cap.Cause), cap.Params)
		return &PlanningResult{Queued: true}, nil
	}

	// Resolve the source through the shared SourceFinder. The dispositions live in
	// resolveSource so intake and scanner-replay cannot drift on them. In particular
	// OutcomeReshuffle returns Handled=true: planBuriedReshuffle has already made
	// THIS order the compound parent (BeginReshuffle → Reshuffling), so the
	// dispatcher must NOT queue it. Queuing it would transition the live compound
	// parent Reshuffling → Queued, and the later CompleteCompound would then attempt
	// the invalid Queued → Confirmed and strand the retrieve forever.
	//
	// Intake is not the only reshuffle planner: an order whose source is accessible
	// here but buried by the time its destination frees is replanned by the
	// fulfillment scanner, which resolves the source behind its own copy of this
	// gate. See Scanner.tryFulfill's OutcomeReshuffle arm.
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

	// ── The synthetic-destination re-resolution ──────────────────────────────
	//
	// AN EMPTY NEEDS THIS AS MUCH AS A MOVE DOES, and until MG2-9 only a move got
	// it. That was a gap, not a scoping decision.
	//
	// Intake resolves a group destination for EVERY order type
	// (lifecycle_service.resolveSyntheticDestination), and deliberately does not
	// fail when the group is full: it leaves the group name on the order and lets
	// it queue, on the promise its own comment makes — "planMove resolves a
	// concrete child at dispatch time". The promise was kept for moves and
	// silently broken for empties, because this branch asked isMove.
	//
	// Nothing downstream covers for it. The fulfillment scanner reads
	// order.DeliveryNode and looks it up with GetNodeByDotName, which FINDS a
	// synthetic group — it is a real row — so there is no error to catch and no
	// second resolver. The order goes to the fleet naming a node no robot can
	// drive to.
	//
	// It is not only the queued path either: a retrieve_empty to a group with
	// free capacity passes CheckDropoffCapacity on the first pass and reaches
	// here with the group name still on it.
	//
	// MAINTAINED-GROUP ASKS DO NOT DEPEND ON THIS. The keeper pre-resolves each
	// ask to a concrete position before admitting it, so this branch is a no-op
	// for them by construction — which is the point of pre-resolving. This fixes
	// the OTHER empties: the ones that arrive from a station naming a group.
	if isMove || isEmpty {
		// If the destination is still a synthetic NGRP, resolve a concrete child slot
		// now. Intake (CreateInboundOrder) deferred it because the group was full;
		// the scanner dispatches to order.DeliveryNode verbatim (it does not resolve
		// NGRPs), so the concrete resolution must land on the order here. On a
		// still-full group this is a TOCTOU race — re-queue and let the scanner retry.
		if destNode.IsSynthetic && destNode.NodeTypeCode == protocol.NodeClassNGRP && s.resolver != nil {
			result, rErr := s.resolver.Resolve(destNode, binresolver.ResolveModeStore, payloadCode, nil, digAskerFor(order))
			if rErr != nil {
				s.dbg("move: dest group %s unresolved (%v), queuing order %d", order.DeliveryNode, rErr, order.ID)
				// AND THE ROW SAYS WHY. This queued the order and wrote nothing, so a
				// move parked on a full destination group was indistinguishable on the
				// row from one nobody had planned yet — and blank is the one wait state
				// no operator and no query can explain. Same cause the complex path uses
				// for the same fact (a step still names a group with no free child), so
				// the two cannot drift.
				s.setQueueReason(order, protocol.QueueWaitingForSlot, CauseNGRPResolve,
					QueueParams{Destination: order.DeliveryNode})
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
		//
		// STAYS MOVE-ONLY. The same argument reads as though it applies to an
		// empty — pick up here, put down here, the fleet cancels src == dst — but
		// turning it into a hard planning error for empties is a REFUSAL this
		// change has no evidence for. A move names its own source, so a same-node
		// move is the operator having asked for something impossible. An empty's
		// source is chosen by the finder, so the same condition would be Core
		// refusing an order over its own selection, and the right answer there is
		// probably to pick a different empty rather than to fail. Widening the
		// resolution is a fix; widening the refusal would be a new behaviour
		// riding along with it.
		if isMove && sourceNode.ID == destNode.ID {
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
	// ASKS A DIFFERENT QUESTION FROM admission, and does not delegate.
	//
	// "May I CLAIM this lane for a dig" is an acquisition precondition. "May this
	// move happen now" is about one leg against the lane as it stands. They are
	// not the same question and the tell is decisive: a buried retrieve is buried
	// BY CONSTRUCTION — BuriedError carries the slot the wanted bin sits behind —
	// so admission's reachability arm would refuse every reshuffle plan with
	// lane-target-buried. A planner that consulted admission could never plan the
	// reshuffle that fixes the burial.
	//
	// Occupancy is wrong here for the same reason at lower stakes: somebody
	// transiently inside a lane is no reason to refuse to PLAN, and the legs get
	// admitted individually anyway.
	//
	// IsLocked rather than DigOwner is right at this site: it fails closed by
	// discarding the read error, and this caller has no way to report one.
	if s.laneLock.IsLocked(buried.LaneID) {
		return nil, &planningError{Code: codeLaneLocked, Detail: fmt.Sprintf("lane %d is locked by another reshuffle", buried.LaneID)}
	}
	// THREE OUTCOMES, NOT TWO. This read used to be `if err != nil ||
	// lane.ParentID == nil`, which filed a database that did not answer under the
	// same terminal as a lane that is not there.
	//
	// Releaser for the park: this planner is reached from the fulfillment
	// scanner's OutcomeReshuffle arm (fulfillment/scanner.go tryFulfill) and from
	// its held-bin dig, so the ordinary retry loop re-drives it — no new
	// subscription, and a read that failed once usually succeeds next time.
	lane, err := s.db.GetNode(buried.LaneID)
	if readFailed(err) {
		s.setQueueReason(order, protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{})
		return nil, &planningError{
			Code:   codeReadFailed,
			Detail: fmt.Sprintf("could not read lane %d, retrying: %v", buried.LaneID, err),
			Err:    err,
		}
	}
	if err != nil || lane == nil {
		return nil, &planningError{
			Code:   codeInvalidNode,
			Detail: configFailureID("lane node", buried.LaneID),
			Err:    err,
		}
	}
	if lane.ParentID == nil {
		return nil, &planningError{
			Code:   codeInvalidNode,
			Detail: fmt.Sprintf("config failure: lane %s is not in a node group, so it has nowhere to park a blocker", lane.Name),
		}
	}
	// THE ASKER IS THE ORDER THAT WILL OWN THIS DIG. planBuriedReshuffle re-parents
	// the demand itself — the TryLock two calls down is taken in order.ID's name —
	// so right of way exempts a lane this same order is already digging (an earlier
	// generation of the same episode) and refuses every other dig's lane.
	plan, err := PlanReshuffle(s.db, buried.Bin, buried.Slot, lane, *lane.ParentID, digAskerFor(order))
	if err != nil {
		// "No free shuffle slot" is CONGESTION, not a fault — a slot frees as soon
		// as any other order clears one. It must wait and retry, never fail: demand
		// does not terminate for congestion (surfaced by sim order 21). Every other
		// planning failure here is real lane geometry and stays terminal.
		// RIGHT OF WAY, asked before the general shortage for the same reason
		// classifyPlanError asks it first: it names an order, and the general arm
		// would file it as a full group. The demand parks with the lane that has to
		// free, and the dig is NOT started — no lock has been taken at this point,
		// which is what makes the refusal free.
		var held *DigParkingHeldError
		if errors.As(err, &held) {
			// Name the dig holding that parking. The lane in this sentence is
			// SOMEBODY ELSE'S — the one right of way refused us — so without the
			// excavation's id the operator has a lane they do not own and no way
			// to tell which dig has to finish.
			// AND WHICH KIND OF HOLDER, because §R.101 put two of them behind one
			// refusal. A reshuffle and a demand sourcing from the lane both remove
			// it from the dig-free pool, and they clear on different events — see
			// CauseDemandHoldsParking. Reporting both as a dig sends the operator
			// looking for an excavation that was never planned.
			parkingCause := CauseDigHoldsParking
			if !held.HolderIsExcavation {
				parkingCause = CauseDemandHoldsParking
			}
			s.setQueueReason(order, protocol.QueueStorageRearranging, parkingCause,
				QueueParams{Lane: held.Lane, Payload: order.PayloadCode,
					DigOrderID: digWaitByLaneName(s.db, s.laneLock, held.Lane)})
			return nil, &planningError{Code: codeNoShuffleSlot, Detail: fmt.Sprintf("cannot plan reshuffle yet: %v", err), Err: err}
		}
		// THE MOUTH'S TWO REFUSALS, BOTH TRANSIENT (§R.96 stage 2). Neither is a
		// full group, and neither is geometry: one is a lane whose entry another
		// order owns, the other is a lane nobody could read. Both wait.
		//
		// codeNoShuffleSlot carries the transience; the CAUSE on the row is what
		// keeps them apart for whoever reads the board, which is the same split
		// right of way takes three lines up.
		var mouthHeld *LaneMouthHeldParkingError
		if errors.As(err, &mouthHeld) {
			s.setQueueReason(order, protocol.QueueStorageRearranging, CauseLaneHeldTraffic,
				QueueParams{Lane: mouthHeld.Lane, Payload: order.PayloadCode})
			return nil, &planningError{Code: codeNoShuffleSlot, Detail: fmt.Sprintf("cannot plan reshuffle yet: %v", err), Err: err}
		}
		var unseen *MouthUnreadableError
		if errors.As(err, &unseen) {
			// CauseLaneHeldUnreadable is already the tree's word for "this is an
			// absence of an answer, not a busy lane" — the exact distinction this
			// error exists to preserve.
			s.setQueueReason(order, protocol.QueueStorageRearranging, CauseLaneHeldUnreadable,
				QueueParams{Payload: order.PayloadCode})
			return nil, &planningError{Code: codeReadFailed, Detail: fmt.Sprintf("cannot count the parking pool: %v", err), Err: err}
		}
		if errors.Is(err, ErrNoShuffleSlot) {
			return nil, &planningError{Code: codeNoShuffleSlot, Detail: fmt.Sprintf("cannot plan reshuffle yet: %v", err), Err: err}
		}
		// ONE SPELLING OF THE READ-FAILURE DISPOSITION, ACROSS BOTH LAYERS. The
		// lane read at the top of this function has parked on a failed read since
		// read_vs_missing.go landed; the PLANNER's own reads — the blockers in front
		// of the slot, the bins in them, the group's children — fell through to
		// codeReshuffle, which is terminal. Same event, same demand, opposite
		// disposition, decided by which SELECT happened to fail (PLAN §R.45).
		//
		// ErrSlotNotInLane is asked first and deliberately: readFailed() is true for
		// any non-nil error that is not sql.ErrNoRows, so the genuine configuration
		// fault would otherwise park forever under a cause nothing can clear.
		if !errors.Is(err, ErrSlotNotInLane) && readFailed(err) {
			s.setQueueReason(order, protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{})
			return nil, &planningError{
				Code:   codeReadFailed,
				Detail: fmt.Sprintf("could not read the lane while planning the dig, retrying: %v", err),
				Err:    err,
			}
		}
		return nil, &planningError{Code: codeReshuffle, Detail: fmt.Sprintf("cannot plan reshuffle: %v", err), Err: err}
	}
	// THE SAME ASKER THE PLANNER GOT, ONE LAYER DOWN. This dig is the demand's
	// own — owner and beneficiary are one order — so a mouth hold it already
	// carries on this lane is UPGRADED to the dig rather than refusing it.
	// Owner-blind, that pairing was not a wait but a wedge: admitMouth refuses
	// one owner two modes on one lane, so a demand that had taken an outbound
	// hold could never dig the lane it was holding, and the answer would never
	// change. Unreachable while the lane gate yields no holds; reachable the
	// moment it does (§R.96 stage 2), which is why it is closed first.
	if !s.laneLock.TryLockFor(buried.LaneID, order.ID, digAskerFor(order)) {
		return nil, &planningError{Code: codeLaneLocked, Detail: "lane locked concurrently"}
	}
	if err := s.createCompound(order, plan); err != nil {
		s.laneLock.Unlock(buried.LaneID, order.ID)
		// A blocker held by an order outside the compound is CONGESTION, and the
		// purest kind: the commonest holder is a dispatched retrieve whose robot is
		// at that moment driving that very bin out of this lane. The blocker is
		// ceasing to exist. Same shape as ErrNoShuffleSlot three lines up — park
		// with a cause, re-plan when the lane changes — and it rides this arm's
		// unlock rather than a second one, so the lane is free for whoever can use
		// it while we wait.
		//
		// The releaser is already wired, and nothing is added for it: the shallow
		// bin's pickup moves it to _TRANSIT and its arrival clears the claim, both
		// of which fire bin events the fulfillment scanner is subscribed to, and
		// the holder going terminal releases through TerminalizeOrder and emits as
		// well. The next scan re-runs findBuriedBlockers against a lane that no
		// longer contains the bin, and the dig plans clean.
		if errors.Is(err, store.ErrBlockerClaimed) {
			s.setQueueReason(order, protocol.QueueStorageRearranging, CauseDigBlockerClaimed,
				QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
			return nil, &planningError{
				Code:   codeBlockerClaimed,
				Detail: fmt.Sprintf("cannot dig yet: %v", err),
				Err:    err,
			}
		}
		return nil, &planningError{Code: codeReshuffle, Detail: fmt.Sprintf("cannot create compound order: %v", err), Err: err}
	}
	// createCompound already transitioned the parent to Reshuffling via
	// lifecycle.BeginReshuffle and dispatched the first child via the
	// tail AdvanceCompoundOrder call in CreateCompoundOrder — any
	// dispatch error from that path is surfaced through the createCompound
	// error wrap above. Do NOT add a second advanceCompound here: stacking
	// two advances within milliseconds dispatched a second child before
	// the first left the dock on the 2026-05-27 production reshuffle.
	s.dbg("retrieve: compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))
	return &PlanningResult{Handled: true}, nil
}
