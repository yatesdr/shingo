package dispatch

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

type Dispatcher struct {
	db            *store.DB
	backend       fleet.Backend
	emitter       Emitter
	resolver      NodeResolver
	laneLock      *LaneLock
	stationID     string
	dispatchTopic string
	lifecycle     *LifecycleService
	replies       *ReplySender
	planner       *PlanningService
	binManifest   *service.BinManifestService
	allocator     *Allocator
	// finder is the shared source-finding seam (see source_finder.go). Owned
	// here so every sourcing consumer resolves through the SAME instance.
	finder   *SourceFinder
	DebugLog func(string, ...any)

	// laneGates serializes lane-gate release passes per lane; gateAppendFails
	// debounces the operator-facing queue code for repeated append failures. Both
	// are in-process and crash-volatile by design — see lane_gate_release.go.
	laneGates       *laneGateSerializer
	gateFailMu      sync.Mutex
	gateAppendFails map[int64]int

	// postFindHook is a test-only seam fired by the fulfillment scanner between
	// Find and Claim (the single claim point after the claim-move to the scanner).
	// Nil in production; set via SetPostFindHook for deterministic concurrency tests.
	postFindHook func()
}

func NewDispatcher(db *store.DB, backend fleet.Backend, emitter Emitter, stationID, dispatchTopic string, resolver NodeResolver) *Dispatcher {
	binManifest := service.NewBinManifestService(db, service.EpochAnnounce{
		Topic:       dispatchTopic,
		CoreStation: stationID,
	})
	d := &Dispatcher{
		db:            db,
		backend:       backend,
		emitter:       emitter,
		resolver:      resolver,
		laneLock:      NewLaneLockWithDB(db.DB),
		stationID:     stationID,
		dispatchTopic: dispatchTopic,
		binManifest:   binManifest,

		laneGates:       newLaneGateSerializer(),
		gateAppendFails: make(map[int64]int),
	}
	d.lifecycle = newLifecycleService(db, backend, emitter, resolver, binManifest, d.dbg)
	// A closure rather than the planner itself, because the planner is built a
	// few lines below this one. It is only ever called while handling a
	// request, long after both exist.
	d.lifecycle.serves = func(t protocol.OrderType) bool { return d.planner.Handles(t) }
	d.replies = newReplySender(db, dispatchTopic, stationID, d.dbg)
	// ONE finder, shared by intake planning, the scanner replay (via the
	// planner), the dispatcher's own step resolution, and the allocator. The
	// seam exists so complex sourcing cannot drift from simple sourcing again;
	// two finder instances would be two seams.
	d.finder = NewSourceFinder(db, resolver, d.dbg)
	d.planner = newPlanningService(db, resolver, d.finder, d.laneLock, d.dbg, d.CreateCompoundOrder)
	d.allocator = newAllocator(db, binManifest, d.finder, d.dbg)
	return d
}

func (d *Dispatcher) dbg(format string, args ...any) {
	if fn := d.DebugLog; fn != nil {
		fn(format, args...)
	}
}

func (d *Dispatcher) RegisterPlanner(orderType protocol.OrderType, handler PlanningHandler) {
	d.planner.Register(orderType, handler)
}

// HandleOrderRequest processes a new order from ShinGo Edge.
func (d *Dispatcher) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	stationID := env.Src.Station
	d.dbg("order request: station=%s uuid=%s type=%s payload=%s delivery=%s source=%s",
		stationID, p.OrderUUID, p.OrderType, p.PayloadCode, p.DeliveryNode, p.SourceNode)

	order, payloadCode, lifecycleErr := d.lifecycle.CreateInboundOrder(stationID, p)
	if lifecycleErr != nil {
		if lifecycleErr.Err != nil {
			d.dbg("create inbound order %s: %v", p.OrderUUID, lifecycleErr.Err)
		}
		d.replies.SendError(env, p.OrderUUID, lifecycleErr.Code, lifecycleErr.Detail)
		return
	}

	result, planErr := d.planner.Plan(order, env, payloadCode)
	if planErr != nil {
		if planErr.Err != nil {
			d.dbg("plan order %s (%s): %v", p.OrderUUID, p.OrderType, planErr.Err)
		} else {
			d.dbg("plan order %s (%s): %s", p.OrderUUID, p.OrderType, planErr.Detail)
		}
		// Transient contention clears on its own — a source bin claimed by a
		// concurrent order in the TOCTOU gap (claim_failed), or a buried bin whose
		// lane is mid-reshuffle for another order (lane_locked). Queue the order so
		// the fulfillment scanner retries instead of terminally failing it; multi-
		// window loaders pulling empties in parallel make this contention routine.
		if planErr.Transient() {
			d.dbg("dispatch: %s for order %s — transient contention, queuing for retry", planErr.Code, p.OrderUUID)
			d.queueOrder(order, env, payloadCode)
			return
		}
		d.failOrder(order, env, planErr.Code, planErr.Detail)
		return
	}
	if result == nil || result.Handled {
		return
	}
	// The claim-move to the scanner: every simple order is status-first queued —
	// the scanner is the single claim point (planTransport validated + resolved but
	// never claimed or dispatched inline). queueOrder emits EmitOrderQueued, which
	// synchronously runs the fulfillment scanner (wiring.go), so an immediately-
	// sourceable order still acks dispatched on return and an unsourceable one waits
	// — exactly complex's existing shape. The shadow plan + endpoints on result are
	// substrate for the unified-create follow-up, not consumed here.
	d.queueOrder(order, env, payloadCode)
}

// PlanBuriedReshuffle plans and dispatches the reshuffle compound for an order
// whose source resolved BURIED on scanner replay. It is the replay-side twin of
// the intake path in planTransport.
//
// Reshuffle planning cannot live at intake alone. planTransport runs exactly once
// (PlanningService.Register wires it to the three simple order types and nothing
// re-invokes it), but burial is a condition that arises over TIME: an order that
// queued with an accessible source — behind a full destination, or behind
// inventory — can be buried by a later store while it waits. The fulfillment
// scanner is the only thing that looks at it again, so without a planner here
// that order re-queues forever and nothing in the system will ever unbury its
// lane.
//
// PRECONDITION: the caller must have cleared the dropoff-capacity gate. A
// simple-retrieve reshuffle compound IS the delivery — PlanReshuffle's retrieve
// step leaves ToNode nil and compound.go backfills the parent's DeliveryNode —
// and compound children are dispatched by AdvanceCompoundOrder, which never
// re-checks capacity. So planning a reshuffle COMMITS the delivery, and may only
// be done against a destination already known clear. Scanner.tryFulfill checks
// CheckDropoffCapacity before it resolves the source, which is exactly that.
//
// Do NOT advance the compound after this returns: createCompound already
// dispatched the first child. Stacking a second advance is the 2026-05-27
// three-robots-in-one-corridor failure (see planBuriedReshuffle).
//
// An ErrReshuffleWait error means requeue and retry; anything else is structural
// and fails the order, matching intake's disposition on a non-transient planErr.
func (d *Dispatcher) PlanBuriedReshuffle(order *orders.Order, buried *BuriedError) error {
	if _, pe := d.planner.planBuriedReshuffle(order, buried); pe != nil {
		if pe.Transient() {
			return &ReshuffleWaitError{Cause: reshuffleWaitCause(pe.Code), Detail: pe.Detail}
		}
		return pe
	}
	return nil
}

// ReshuffleWaitError is ErrReshuffleWait plus WHICH congestion it was.
//
// The sentinel alone tells a caller to wait; it does not tell the operator
// anything, and the callers were all writing the same blanket
// "reshuffle-congestion" onto the row. Three genuinely different waits — a lane
// reserved for someone else's dig, no free shuffle slot, a blocker bin claimed
// by an order that is carrying it out — arrived indistinguishable, with three
// different releasers and three different answers to "should I go look at it".
type ReshuffleWaitError struct {
	Cause  QueueCause
	Detail string
}

func (e *ReshuffleWaitError) Error() string {
	return fmt.Sprintf("%s: %s", ErrReshuffleWait, e.Detail)
}

func (e *ReshuffleWaitError) Unwrap() error { return ErrReshuffleWait }

// ReshuffleWaitCause reads the cause off a wait error for callers outside this
// package. Falls back to the historical blanket tag, so an error that predates
// the typed shape still lands somewhere honest rather than blank.
func ReshuffleWaitCause(err error) QueueCause {
	var rw *ReshuffleWaitError
	if errors.As(err, &rw) && rw.Cause != "" {
		return rw.Cause
	}
	return CauseReshuffleCongestion
}

// reshuffleWaitCause maps a transient planning code onto the cause tag that goes
// on the row. A code with no mapping falls back to the blanket tag rather than
// writing a blank — but adding a transient code and not adding it here is a gap,
// not a default.
func reshuffleWaitCause(code string) QueueCause {
	switch code {
	case codeLaneLocked:
		return CauseLaneLocked
	case codeNoShuffleSlot:
		return CauseNoShuffleSlot
	case codeBlockerClaimed:
		return CauseDigBlockerClaimed
	case codeReadFailed:
		return CauseReadFailed
	}
	return CauseReshuffleCongestion
}

// ErrReshuffleWait reports planning-time CONGESTION rather than a fault: the
// reshuffle cannot be planned RIGHT NOW, but will be plannable once other work
// clears. Two causes, both routine:
//
//   - the lane is mid-reshuffle for ANOTHER order (lane_locked) — the ordinary
//     shape when two queued orders are buried in the same lane;
//   - there is no free shuffle slot to park the blockers in (no_shuffle_slot) —
//     a slot frees as soon as any other order releases one.
//
// Callers WAIT and retry. They must NOT fail the order: "no room to dig right
// now" is not a broken lane, and demand never terminates for congestion. The
// no-shuffle-slot half used to fail terminally at intake — sim order 21 on the
// 2026-07-10 houseserver run died that way — and the fix belongs here, because it
// is only actually FIXED once the scanner can re-plan on a later tick.
//
// A sentinel (rather than an exported predicate over the unexported
// planningError) so callers outside the package — and their tests — can both
// match it with errors.Is and construct it.
var ErrReshuffleWait = errors.New("reshuffle not plannable yet")

// queueOrder is the wire adapter: queue the order, then tell the Edge station
// that asked for it what happened. Everything except that last sentence is in
// queueOrderInternal.
func (d *Dispatcher) queueOrder(order *orders.Order, env *protocol.Envelope, payloadCode string) {
	d.queueOrderInternal(order, env.Src.Station, payloadCode)
	d.replies.SendUpdate(env, order.EdgeUUID, string(StatusQueued), "awaiting inventory")
}

// queueOrderInternal is the queue tail with no wire envelope in it: mark the
// order queued, write its payload code, and emit the queued event — which
// synchronously runs the fulfillment scanner (wiring.go), so an immediately
// sourceable order is claimed and dispatched before this returns.
//
// Split out from queueOrder because an order Core originates itself has no
// envelope. There is no request to correlate to and nobody waiting on a reply;
// the station is a routing label the emitter needs, not the sender's return
// address. Sending an update against a synthesized envelope would be Core
// answering a question nobody asked, addressed to a correlation ID no Edge is
// tracking — so the reply stays in the adapter above and the queue tail stays
// callable without one.
func (d *Dispatcher) queueOrderInternal(order *orders.Order, stationID, payloadCode string) {
	if err := d.lifecycle.Queue(order, "dispatcher", "awaiting inventory"); err != nil {
		d.dbg("queue order %d: %v", order.ID, err)
	}
	if payloadCode != "" && order.PayloadCode == "" {
		if err := d.db.UpdateOrderPayloadCode(order.ID, payloadCode); err != nil {
			d.dbg("update payload code order %d: %v", order.ID, err)
		}
	}
	d.dbg("queued: order=%d uuid=%s payload=%s delivery=%s", order.ID, order.EdgeUUID, payloadCode, order.DeliveryNode)
	d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
}

// dispatchToFleet sends the order and acks the station that asked. IT RETURNS
// THE REFUSAL RATHER THAN DISPOSING OF IT, and that is the same rule
// handoverToFleet states one layer down: "FAILING THE ORDER IS THE CALLER'S JOB
// … the callers already do it and they do it differently."
//
// It used to call failOrder itself, which made one disposition — terminal —
// binding on both callers, and the two are not alike:
//
//   - THE REDIRECT has a person waiting on a reply. A fleet that will not take
//     the order is the end of that request, and the operator is told.
//   - A COMPOUND LEG has no person and no reply. Failing it fails the parent
//     through the sibling cascade (HandleChildOrderFailure), and the parent IS
//     the demand — so a robot-system blip terminated a demand for congestion,
//     which is the one thing the wait-not-fail rule forbids. `51a97a56` fixed
//     exactly this on the plain path (DispatchDirect) and the dig path, whose
//     only caller is this function, never got it.
func (d *Dispatcher) dispatchToFleet(order *orders.Order, env *protocol.Envelope, sourceNode, destNode *nodes.Node) error {
	if _, err := d.dispatchToFleetCore(order, sourceNode, destNode); err != nil {
		return err
	}
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode.Name)
	return nil
}

// payloadForDispatch answers what the robot is actually being asked to carry.
//
// Two fleet-facing decisions are made from it, both immediately below: the robot
// GROUP — which robots are eligible to take the job — and the advanced load
// sequence. A move order carried no payload at all. Nothing wrote one: the
// operator is not asked, and the planner resolves the source bin without writing
// its payload back. So every move in the plant has been dispatched against the
// vendor's default robot group.
//
// That is a capability question, not a label. A payload that needs a 1500 kg
// robot could be handed to a 600 kg one, and nothing in the system would say so.
//
// When the order does not name a payload, the BIN does. By the time this runs
// the order names its bin — the fulfillment scanner writes bin_id at claim time
// immediately before dispatching, and the bin-move door stamps it at creation —
// so this reads the carrier that is actually about to be picked up. That is why
// this sits here and not in the planner: the planner's resolved bin is advisory
// (its own doc says so; the scanner's re-find is authoritative), and the
// bin-move door never goes through the planner at all. This is the one place
// every simple-transport door passes through, on the way to the only two reads
// that care.
//
// AN EMPTY-INTENT ORDER IS DELIBERATELY EXCLUDED. An empty carrier is generic on
// purpose — Edge ships a blank code so the bin is not pre-tagged and the real
// payload binds at load time (lookupPayloadMeta in shingo-edge documents the same
// rule from the other side). Stamping a leftover code off a carrier that is
// supposed to be empty would re-tag it, and the tag would then pick the robot.
//
// The value is written back to the order, best-effort, so the row says what
// moved — the orders table is where "how much of payload X moves" gets answered.
// A failed write still dispatches with the right group; the reverse would be the
// bad trade.
func (d *Dispatcher) payloadForDispatch(order *orders.Order) string {
	if order.PayloadCode != "" || order.SourceIntent == SourceIntentEmpty {
		return order.PayloadCode
	}
	// Read the bin id fresh. The scanner writes it moments before calling here,
	// and after the snapshot the caller is holding — so the copy in hand is
	// routinely nil when the database already knows the answer.
	binID := order.BinID
	if fresh, err := d.db.GetOrder(order.ID); err == nil && fresh != nil && fresh.BinID != nil {
		binID = fresh.BinID
	}
	if binID == nil {
		return order.PayloadCode
	}
	bin, err := d.db.GetBin(*binID)
	if err != nil || bin == nil || bin.PayloadCode == "" {
		return order.PayloadCode
	}
	if uerr := d.db.UpdateOrderPayloadCode(order.ID, bin.PayloadCode); uerr != nil {
		d.dbg("dispatch: recording payload %q on order %d failed: %v (dispatching with it anyway)",
			bin.PayloadCode, order.ID, uerr)
	} else {
		order.PayloadCode = bin.PayloadCode
	}
	d.dbg("dispatch: order %d carries %s (read from bin %s) — robot group and load sequence follow it",
		order.ID, bin.PayloadCode, bin.Label)
	return bin.PayloadCode
}

// robotGroupForPayload resolves the SEER robot-dispatch group configured on the
// order's payload template (→ rds.SetOrderRequest.Group). An empty code, an
// unknown payload, or a lookup error degrades to "" (the vendor's default robot
// assignment) — a robot-group lookup must never block material flow.
func (d *Dispatcher) robotGroupForPayload(payloadCode string) string {
	if payloadCode == "" {
		return ""
	}
	p, err := d.db.GetPayloadByCode(payloadCode)
	if err != nil || p == nil {
		d.dbg("robot group: payload %q lookup failed (%v) — using vendor default", payloadCode, err)
		return ""
	}
	return p.RobotGroup
}

// loadSequenceForPayload resolves the ordered binTask names for a payload's
// configured advanced load sequence (F4c), or nil when the payload has none (the
// field is empty), the payload is unknown, or the named sequence isn't in the
// registry. A nil result means the load leg emits today's single JackLoad block,
// unchanged. Like robotGroupForPayload it never blocks material flow: any lookup
// failure degrades to nil — config-time validation and RDS's 50001 order-issue
// rejection are the guards, not the dispatch hot path.
func (d *Dispatcher) loadSequenceForPayload(payloadCode string) []string {
	if payloadCode == "" {
		return nil
	}
	p, err := d.db.GetPayloadByCode(payloadCode)
	if err != nil || p == nil || p.AdvancedLoadSequence == "" {
		return nil
	}
	seq, err := d.db.GetLoadSequence(p.AdvancedLoadSequence)
	if err != nil || seq == nil {
		d.dbg("load sequence %q for payload %q not resolvable (%v) — normal load",
			p.AdvancedLoadSequence, payloadCode, err)
		return nil
	}
	return seq.TaskNames
}

// dispatchToFleetCore contains the shared fleet dispatch sequence: generate
// vendor order ID, build the plan-shaped blocks, create the fleet order (no-wait,
// Complete=true single-shot), update vendor state, transition lifecycle, emit
// event. Both dispatchToFleet (Kafka/envelope path) and DispatchDirect
// (UI/scanner path) call this core.
//
// A simple order's plan [pickup@src, dropoff@dst] is a 2-block no-wait staged
// order — the same shape the complex tail builds, just Complete=true. The blocks
// come from buildTransportPlan (the plan-builder the simple planner emits) via
// stepsToBlocks, so simple and complex share one create primitive (CreateOrder);
// the only difference is the Complete flag. blockId/goodsId differ from the old
// dedicated transport primitive, but SEER acts only on location + binTask
// (blockId/goodsId are cosmetic) — both preserved here.
//
// It is also the single fleet-create seam for every plain order, which is why the
// gate_choreography valve branches HERE rather than in the scanner: routing on the
// destination at the one create site is what makes "every lane-bound order ships
// unsealed" structurally true, instead of true-for-the-callers-we-remembered. Both
// callers (Kafka/envelope and UI/scanner) inherit it.
func (d *Dispatcher) dispatchToFleetCore(order *orders.Order, sourceNode, destNode *nodes.Node) (string, error) {
	payloadCode := d.payloadForDispatch(order)

	// BUILD THE PLAN, THEN SPLICE IT. One rule for every order type: a plain
	// order has no steps_json at this point, so its plan is built here and then
	// goes through the SAME spliceLaneWait a coordinated order's authored plan
	// goes through at complex_dispatch.go. The valve stopped authoring.
	//
	// The two conditions that used to sit here -- `gated && !order.Coordinated`
	// on the destination, and a mirror of it on the source -- are gone. They were
	// INERT: a coordinated order never reaches this function (the scanner branches
	// on IsCoordinated to DispatchPreparedComplex, whose create is
	// dispatchComplexToFleet), so deleting them changes nothing and is tidying
	// rather than the mechanism. The mechanism is the splice, and it is installed
	// at both create sites.
	//
	// No-op for every ungated path, which is every lane at both plants: the walk
	// resolves each step's lane and finds no gated group, so the plan comes back
	// unchanged and the sealed path below runs byte-identically.
	// THE DESTINATION-DEFERRED LEG BUILDS A DIFFERENT PLAN, AND THE SAME VALVE
	// SHIPS IT.
	//
	// A dig leg whose destination is chosen at release has no dropoff step to
	// build, so its plan is [pickup, wait@shallowest-slot] and it must go out
	// UNSEALED — a sealed order ending in a Wait block is a robot nothing can ever
	// append to. That is true whether or not the lane carries a gate mark, which
	// is why the dwell supplies its own lane target: the mark decides where an
	// INBOUND robot waits and has nothing to say about an outbound one.
	//
	// When the lane IS marked, the splice below inserts the inbound wait ahead of
	// the pickup and returns that mark as the first gate — the create stops
	// outside, the robot is admitted, enters, lifts, and dwells at the second
	// wait. Two waits, one plan, released independently: the multi-gate shape rule
	// 2 already builds.
	plan, dwellTarget, dwelling, err := d.digDwellPlan(order, sourceNode)
	if err != nil {
		return "", err
	}
	if !dwelling {
		plan = buildTransportPlan(sourceNode.Name, destNode.Name, order.SourceIntent == SourceIntentEmpty)
	}
	spliced, target, gated, err := d.spliceLaneWait(plan)
	if err != nil {
		return "", err
	}
	if dwelling && !gated {
		// An unmarked dug lane: the splice found nothing to gate, and the dwell is
		// the reason this order is unsealed.
		target, gated = dwellTarget, true
	}
	// A no-op on this path today: the junction is written only for multi-bin
	// complex orders and this builds a fresh single-bin transport plan. It is
	// called anyway because the rule belongs to the SPLICE, not to one caller's
	// current order shape — the wedge this repairs came from exactly that kind of
	// "cannot happen here" reasoning going stale.
	//
	// THE CAUSE GOES ON THE ROW HERE, because neither caller downstream can name
	// this one. A re-index failure is a database error; both of this function's
	// callers treat any error from it as a FLEET refusal, which is the vocabulary
	// they were built for — the scanner parks under fleet-unavailable (a wait, so
	// wait-not-fail holds, but the sentence is wrong), and the redirect door
	// fails the row because a person is waiting on the reply. Writing the real
	// cause at the moment it happens is the only place the truth is still known.
	if rErr := d.reindexOrderBinsForSplice(order.ID, spliced); rErr != nil {
		log.Printf("dispatch: order %d — could not re-index its junction onto the spliced plan: %v",
			order.ID, rErr)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{})
		return "", rErr
	}
	if gated {
		return d.dispatchGated(order, target, spliced, payloadCode, d.loadSequenceForPayload(payloadCode))
	}

	vendorOrderID := mintVendorOrderID(order.ID)

	// F4c: expand the load leg into the payload's configured binTask sequence
	// (nil for an unconfigured payload → byte-identical single JackLoad block).
	blocks := stepsToBlocks(vendorOrderID, plan, 0, d.loadSequenceForPayload(payloadCode))
	// The order's own priority, and nothing else. Core used to add a depth-derived
	// lane boost here; that was deleted (see lane_gate.go). Lane entries are
	// first-come-first-serve until priority is something the system plans around.
	priority := order.Priority
	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   priority,
		RobotGroup: d.robotGroupForPayload(payloadCode),
		Complete:   true, // no-wait: the fleet completes the order once its 2 blocks finish
	}

	// payload= and robot_group= are on this line because together they are the
	// capability decision: which part the job carries, and therefore which robots
	// may take it. A blank group is the vendor default — any robot — which is the
	// right answer for an unrestricted part and the wrong one for a heavy part
	// whose payload never made it onto the order. Distinguishable only if both
	// are recorded.
	d.dbg("fleet dispatch: order=%d vendor_id=%s from=%s to=%s priority=%d payload=%q robot_group=%q",
		order.ID, vendorOrderID, sourceNode.Name, destNode.Name, priority,
		payloadCode, req.RobotGroup)

	// HOLD B, THE TAKE HALF, ON THE PLAIN PATH — the other half of the
	// unification. Admission now asks "is anyone inside this lane" for plain
	// orders (admission.go, skipsForPlainEntry), and that question is meaningless
	// while the asker never appears in the answer: before this, TakeLaneOccupancy
	// had exactly one caller (compound.go), so the only presence Core recorded was
	// a reshuffle leg's.
	//
	// HERE and not in the scanner, because this is the fleet-create seam every
	// plain order passes through — both callers inherit it, the same reason the
	// gate valve branches here. Core's own decision to send IS the entry moment
	// (TakeLaneOccupancy), and this is where that decision becomes irreversible.
	//
	// BEFORE the handover, so there is no window where a robot is committed and
	// its presence unrecorded — a lane whose occupancy could not be WRITTEN would
	// read empty to the next order, which is the same collision from the other
	// side. The failure arms below are what make taking first safe.
	// Record the presence, then claim, commit and name it — commitToFleet
	// (fleet_handover.go) is the seam every arm goes through, and both rules that
	// used to be spelled out here live in it now: take before the handover, and
	// release on every failure except a lost CAS. The pre-dispatch terminal
	// re-read that used to sit here is absorbed by the CAS, which asks a strictly
	// stronger question atomically.
	if err := d.commitToFleet(order, req, "dispatcher", sourceNode, destNode); err != nil {
		return "", err
	}

	d.dbg("order %d dispatched as %s (%s -> %s)", order.ID, vendorOrderID, sourceNode.Name, destNode.Name)
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode.Name, destNode.Name)
	return vendorOrderID, nil
}

// DispatchDirect dispatches an order to the fleet without a protocol envelope.
// Used for orders created internally (e.g. direct orders from the UI) and
// from the fulfillment scanner after a bin claim resolves.
//
// Callers reach this function with the order in one of three states:
//   - pending  — direct creation paths (engine.CreateBinMove, reached from the
//     operator's manual-order screen and the /test-orders direct tab) jump
//     straight from intake to dispatch.
//     We bridge through queued to satisfy the state machine.
//   - sourcing — fulfillment.Scanner moves the order to sourcing once a bin
//     is found; sourcing → dispatched is a valid edge.
//   - queued   — pre-dispatch holding state for a fully-resolved order.
//
// Returns the vendor order ID on success.
func (d *Dispatcher) DispatchDirect(order *orders.Order, sourceNode, destNode *nodes.Node) (string, error) {
	// Bridge pending → queued before dispatching. The lifecycle's Dispatch
	// method only accepts queued/sourcing as source states; direct-creation
	// callers leave the order in pending. validTransitions allows
	// pending → queued explicitly as the fast-path edge for callers that
	// already know the destination.
	if order.Status == protocol.StatusPending {
		if err := d.lifecycle.Queue(order, "dispatcher", "direct dispatch"); err != nil {
			d.dbg("order %d → queued: %v", order.ID, err)
		}
	}

	// IT DOES NOT TERMINALIZE ON A FLEET REFUSAL, and that is the fix rather than
	// an omission. This used to call lifecycle.Fail here, which made the caller's
	// disposition unreachable: `failed` has no outgoing edges
	// (protocol.validTransitions), so the fulfillment scanner's documented
	// rollback — "override back to sourcing since this is a transient fleet issue,
	// not a permanent failure" — was an illegal transition, logged and swallowed.
	// Every fleet rejection on the highest-traffic dispatch path killed the order
	// while two call sites read as though they had re-queued it, and a rejection
	// on a compound leg took the whole dig and its parent down with it through the
	// sibling cascade.
	//
	// A fleet that will not take an order right now is congestion — the vocabulary
	// already says so (protocol.QueueFleetUnavailable, "Robot system not
	// responding — retrying"). Whether THIS caller can wait is the caller's to
	// know: the scanner parks and retries, the bin-move door has a person waiting
	// and fails the row itself. Returning the error is what lets them differ.
	vendorOrderID, err := d.dispatchToFleetCore(order, sourceNode, destNode)
	if err != nil {
		// IT DOES UNDO ITS OWN MOVE, THOUGH. handoverToFleet's CAS claims the order
		// by transitioning it to `dispatched` BEFORE the create, and documents that
		// it leaves it there for "its caller to fail". This IS that caller, and not
		// terminalizing cannot mean walking away: `dispatched` with no vendor id is
		// an order nothing tracks (loadActiveOrders selects on a non-empty
		// vendor_order_id) until the stuck sweep finds it.
		//
		// So the disposition becomes `sourcing` — live, re-dispatchable, and in the
		// acquiring set — and the caller decides from there. The scanner's
		// MoveToSourcing then lands as an idempotent no-op rather than the illegal
		// transition it used to be, and the bin-move door's explicit fail still
		// works, because sourcing → failed is legal and dispatched → failed was too.
		//
		// NOT ON A LOST CAS. IsConcurrentTransition means another caller owns this
		// order now; rolling back its status would clobber whatever it is doing.
		// Same rule, for the same reason, as the occupancy release in
		// dispatchToFleetCore.
		if !IsConcurrentTransition(err) && order.Status == protocol.StatusDispatched {
			if rbErr := d.lifecycle.MoveToSourcing(order, "dispatcher", "fleet refused the create"); rbErr != nil {
				log.Printf("dispatch: order %d could not be moved back to sourcing after a fleet "+
					"refusal: %v (it is `dispatched` with no vendor order; the stuck sweep is the backstop)",
					order.ID, rbErr)
			}
		}
		return "", err
	}

	return vendorOrderID, nil
}

// checkOwnership verifies the envelope sender owns the order.
// Core-role senders (e.g. UI-initiated actions) are always allowed.
//
// A COMPOUND CHILD IS NEVER STATION-COMMANDABLE. `ParentOrderID != nil` means
// Core created this order as a step of something it is running — a reshuffle leg
// — and no station has standing to release, cancel, redirect or file a receipt
// against one. All four handlers behind getOwnedOrder inherit this.
//
// It is needed because station_id does NOT mean what the comparison below assumes
// for such an order. A leg inherits its parent's station_id (compound.go), and the
// parent is a real station-originated retrieve, so the comparison passes. And a
// station can genuinely hold a row for a leg: the reconcile heals an Edge with
// every active order for the asking station (CoreDataService.unlistedFor →
// ListActiveOrdersByStation), and that query carries no parent filter, so legs are
// projected down and Edge creates rows for them.
//
// station_id is doing three jobs — authorization, addressing, attribution — and
// only the first is wrong here. Hence a separate structural discriminator rather
// than blanking the column: blanking would lose the audit actor on the compound
// Fail/Cancel paths, and a new originating-station column would duplicate
// origin_id/origin_class, which is already copied parent→child.
func (d *Dispatcher) checkOwnership(env *protocol.Envelope, order *orders.Order) bool {
	if env.Src.Role == protocol.RoleCore {
		return true
	}
	if order.ParentOrderID != nil {
		return false
	}
	return env.Src.Station == order.StationID
}

// getOwnedOrder fetches an order by UUID and checks ownership. Returns the
// order and true on success, or nil and false if the order was not found or
// the sender does not own it (with appropriate logging in both cases).
// Callers handle the false case with their own error response.
func (d *Dispatcher) getOwnedOrder(env *protocol.Envelope, orderUUID string) (*orders.Order, bool) {
	order, err := d.db.GetOrderByUUID(orderUUID)
	if err != nil {
		d.dbg("order %s not found: %v", orderUUID, err)
		return nil, false
	}
	if !d.checkOwnership(env, order) {
		d.dbg("station %s does not own order %s (owner: %s)", env.Src.Station, orderUUID, order.StationID)
		return nil, false
	}
	return order, true
}

// HandleOrderCancel processes a cancellation request from ShinGo Edge.
func (d *Dispatcher) HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel) {
	stationID := env.Src.Station
	d.dbg("cancel request: station=%s uuid=%s reason=%s", stationID, p.OrderUUID, p.Reason)

	order, ok := d.getOwnedOrder(env, p.OrderUUID)
	if !ok {
		d.replies.SendError(env, p.OrderUUID, "not_found", "order not found or access denied")
		return
	}
	if order.Status == StatusCancelled {
		d.dbg("cancel request: uuid=%s already cancelled", p.OrderUUID)
		return
	}

	// THE PARENT GOES FIRST, and the order is load-bearing rather than stylistic.
	//
	// It used to cancel the children first. Every child cancel fires
	// EventOrderCancelled SYNCHRONOUSLY, and the lane-clearing subscribers react to
	// it — RedriveHeldCompoundLegs calls AdvanceCompoundOrder on the very compound
	// being torn down. Those observers found a parent that still read
	// `reshuffling` with half its legs cancelled: a half-torn-down compound that
	// looks exactly like a live one.
	//
	// Harmless while the only reaction was to re-admit a leg. It stopped being
	// harmless with the dissolve disposition: the redrive admitted the next leg,
	// hit a reachability refusal, DISSOLVED the dig, and the terminal arm then
	// raced the parent's own cancel to a `failed` finish. An operator asked for
	// cancelled and got failed.
	//
	// Cancelling the parent first makes the teardown atomic from every observer's
	// point of view: the first thing anyone can see is a parent that has left
	// `reshuffling`, which is precisely what the dissolve and the terminal arm
	// check before doing anything. Nothing is lost by the swap — the children are
	// still cancelled unconditionally on the next line, so their fleet orders are
	// still cancelled, their bins still unclaimed, and the lane still released.
	//
	// The cascade is UNCONDITIONAL, and stays that way. It used to be gated on
	// order.Status == StatusReshuffling, which is brittle: once a complex parent
	// resumes to Queued (lifecycle.ResumeCompound), a cancel against the Queued
	// parent skipped the cascade and orphaned still-running children.
	// cancelCompoundChildren is idempotent for non-compound orders —
	// ListChildOrders returns an empty slice and the loop no-ops — so the
	// unconditional call costs one extra SELECT per cancel of a plain order.
	d.lifecycle.CancelOrder(order, stationID, p.Reason)
	d.cancelCompoundChildren(order, stationID, p.Reason)
	d.replies.SendCancelled(env, p.OrderUUID, p.Reason)
}

// HandleOrderReceipt processes a delivery confirmation from ShinGo Edge.
func (d *Dispatcher) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	stationID := env.Src.Station
	d.dbg("delivery receipt: station=%s uuid=%s type=%s count=%d", stationID, p.OrderUUID, p.ReceiptType, p.FinalCount)

	order, ok := d.getOwnedOrder(env, p.OrderUUID)
	if !ok {
		return
	}
	if _, err := d.lifecycle.ConfirmReceipt(order, stationID, p.ReceiptType, p.FinalCount); err != nil {
		d.dbg("complete order %d: %v", order.ID, err)
		return
	}
}

// ╔═══════════════════════════════════════════════════════════════════════════╗
// ║  STUB — NEVER TESTED.                                                     ║
// ║                                                                           ║
// ║  Redirect is NOT an RDS feature. RDS exposes create, terminate, get,      ║
// ║  set-priority, set-label, add-blocks and mark-complete — there is no      ║
// ║  modify-destination call anywhere in its API (docs/rds-api-reference.md). ║
// ║  Core fakes one: PrepareRedirect TERMINATES the live vendor order         ║
// ║  (lifecycle_service.go → seerrds/adapter.go → POST /terminate) and then   ║
// ║  dispatchToFleet creates a brand new one.                                 ║
// ║                                                                           ║
// ║  MID-TRANSIT IT IS ALMOST CERTAINLY BROKEN. The re-issued plan is         ║
// ║  buildTransportPlan(ORIGINAL source, new dest) = [pickup@source,          ║
// ║  dropoff@newDest] (plan_simple.go). Once a robot has picked up, the bin   ║
// ║  is on the deck and the source slot is EMPTY — so the new order tells a   ║
// ║  loaded robot to go fetch a bin from a node that no longer has one.       ║
// ║                                                                           ║
// ║  The only coverage is TestRedirect_MidTransit, which runs against the     ║
// ║  SIMULATOR and asserts exactly one thing: that the bin CLAIM survived.    ║
// ║  It never checks the robot reaches the new destination and never inspects ║
// ║  the re-issued blocks. A green suite says nothing about this working.     ║
// ║                                                                           ║
// ║  Reachable, so it is not dead code: Edge's apiRedirectOrder →             ║
// ║  Manager.RedirectOrder queues the message, and Edge no-ops the reply.     ║
// ║                                                                           ║
// ║  BEFORE RELYING ON THIS: decide whether redirect is pre-dispatch only     ║
// ║  (coherent — nothing picked up yet, terminate+recreate is honest), and if ║
// ║  mid-transit is wanted, the re-issued plan must drop the pickup step and  ║
// ║  carry the already-held bin. Do not treat the passing test as evidence.   ║
// ╚═══════════════════════════════════════════════════════════════════════════╝
//
// HandleOrderRedirect processes a redirect request from ShinGo Edge.
func (d *Dispatcher) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	d.dbg("redirect: uuid=%s new_dest=%s", p.OrderUUID, p.NewDeliveryNode)

	order, ok := d.getOwnedOrder(env, p.OrderUUID)
	if !ok {
		d.replies.SendError(env, p.OrderUUID, "not_found", "order not found or access denied")
		return
	}

	// THE CIRCUIT BREAKER. Redirect is only coherent BEFORE the job reaches the
	// fleet.
	//
	// Pre-dispatch, a redirect is an edit: nothing has been sent, no robot is
	// committed, no bin has moved, and changing delivery_node is just changing a
	// column before the plan is built from it. That case is fine and still works.
	//
	// After dispatch it is not an edit, it is a terminate-and-recreate against a
	// running robot, and the recreate is wrong: buildTransportPlan re-issues
	// [pickup@ORIGINAL source, dropoff@new dest], so a robot that has already
	// picked up is told to fetch a bin from a slot it already emptied. RDS has no
	// modify-destination call to do this properly with — see the box above.
	//
	// Refused rather than repaired, deliberately, and refused HERE rather than
	// deeper: PrepareRedirect's first act is to terminate the vendor order, so by
	// the time any lower layer could notice, the robot's job is already gone.
	// Repairing it means deciding what a redirect of a loaded robot should even
	// mean (drop the pickup and carry the held bin), and that is a design question
	// with no answer yet, not a missing line.
	//
	// invalid_state for the same reason the coordinated arm below uses it: Edge
	// treats that code as recoverable, so the operator sees a refusal and the Edge
	// mirror survives. Any other code terminalizes the Edge row.
	if !protocol.IsPreDispatch(order.Status) {
		d.dbg("redirect refused: order %d is %s — past the point where a redirect is an edit",
			order.ID, order.Status)
		d.replies.SendError(env, p.OrderUUID, "invalid_state",
			"this order has already been sent to the fleet and cannot be redirected; cancel it and re-issue")
		return
	}

	// A COORDINATED ORDER CANNOT BE REDIRECTED, and this is not a lane rule.
	// PrepareRedirect below is destructive — it cancels the vendor order and moves
	// the row to sourcing — and what follows it is dispatchToFleetCore, which builds
	// a two-block transport from the source/delivery COLUMNS. An Edge-authored step
	// plan does not survive that trip whether or not a lane is involved; its waits,
	// its intermediate legs and its ordering are simply not read.
	//
	// With gating on it also destroys the plan on disk: a lane-slot destination
	// takes the store valve, which overwrites steps_json with [pickup, wait@gate,
	// dropoff]. The order's own station wait becomes a gate wait and its
	// choreography is gone.
	//
	// Refusing rather than guarding the valve is deliberate. A !Coordinated guard on
	// the valve alone would make this fall through to UNGATED dispatch — a robot
	// entering a gate_choreography lane with no gate at all, which is the failure the
	// gate exists to prevent. That trades a plan bug for a lane-safety bug.
	//
	// Refused HERE and not at Edge's API because whether a destination is a gated
	// lane is Core's configuration; asking Edge to know it would put one decision in
	// two places. invalid_state for the same reason the release fence uses it: Edge
	// treats that code as recoverable and any other code terminalizes the Edge row.
	if order.Coordinated {
		d.dbg("redirect refused: order %d is coordinated — redirect would discard its step plan", order.ID)
		d.replies.SendError(env, p.OrderUUID, "invalid_state",
			"this order carries a multi-step plan and cannot be redirected; cancel it and re-issue")
		return
	}

	sourceNode, newDest, err := d.lifecycle.PrepareRedirect(order, p.NewDeliveryNode)
	if err != nil {
		if err.Error() == "no source node for redirect" {
			d.replies.SendError(env, p.OrderUUID, "redirect_failed", err.Error())
			return
		}
		if sourceNode == nil || newDest == nil {
			d.dbg("redirect dest %q not found: %v", p.NewDeliveryNode, err)
			d.replies.SendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("redirect destination %q not found", p.NewDeliveryNode))
			return
		}
		// Any other prepare failure (source + dest resolved, but PrepareRedirect
		// still errored) → generic redirect_failed.
		d.replies.SendError(env, p.OrderUUID, "redirect_failed", err.Error())
		return
	}
	if newDest == nil {
		d.dbg("redirect dest %q not found (post-prepare): %v", p.NewDeliveryNode, err)
		d.replies.SendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("redirect destination %q not found", p.NewDeliveryNode))
		return
	}

	// MAY THIS MOVE HAPPEN NOW. A redirect points a live order at a node it was
	// never admitted for, so its original admission answers nothing about the new
	// destination — and this path went straight from "the operator typed a node"
	// to the fleet. Redirect a bin into a lane slot and a robot entered a corridor
	// nothing had asked about.
	//
	// EntryHeldBin: a redirected order is in flight, so it holds its bin and never
	// called the finder for it. Same caller shape as the scanner's held-bin path.
	//
	// The park is safe here BECAUSE PrepareRedirect already left the order in
	// `sourcing`: that is in the acquiring set, the order still holds its bin, and
	// the scanner's held-bin path re-asks this question on every lane-clearing
	// event. The redirect is not lost, it is queued — which is the only honest
	// answer available, since PrepareRedirect has already cancelled the vendor
	// order and there is no un-redirecting.
	if admitted, cause, laneName, aerr := d.AcquireLanesForOrder(order, sourceNode, newDest, EntryHeldBin); aerr != nil || !admitted {
		if aerr != nil {
			log.Printf("dispatch: redirect for order %d — lane admission could not be read: %v (holding)", order.ID, aerr)
			cause, laneName = CauseLaneAcquireError, newDest.Name
		}
		if lerr := d.ReleaseLanesForOrder(order.ID); lerr != nil {
			log.Printf("dispatch: release lanes for held redirect %d: %v", order.ID, lerr)
		}
		d.setQueueReason(order, protocol.QueueWaitingForSlot, cause, QueueParams{Destination: laneName})
		d.dbg("redirect: order %d held at its new destination (%s)", order.ID, cause)
		d.replies.SendUpdate(env, order.EdgeUUID, string(StatusQueued), order.QueueReason)
		return
	}

	if err := d.dispatchToFleet(order, env, sourceNode, newDest); err != nil {
		// The redirect's disposition is unchanged: a person typed this node and is
		// waiting on the reply, so a fleet refusal ends the request rather than
		// parking it. PrepareRedirect has already cancelled the vendor leg, so
		// there is nothing left in flight to reconcile.
		d.failOrder(order, env, "fleet_failed", err.Error())
	}
}

// HandleOrderIngest processes an ingest request: an audited manifest-only write
// that records + confirms the produced count on the target bin. It dispatches
// nothing — the produce-store leg went with the retired simple-produce mode.
func (d *Dispatcher) HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest) {
	stationID := env.Src.Station
	d.dbg("ingest: station=%s uuid=%s payload=%s bin=%s source=%s", stationID, p.OrderUUID, p.PayloadCode, p.BinLabel, p.SourceNode)

	// Ingest is manifest-only: Core records the produced count via the audited
	// manifest write; there is nothing to dispatch.
	if lifecycleErr := d.lifecycle.ApplyIngestManifest(p); lifecycleErr != nil {
		// A rejected produce-finalize is an inventory-integrity event: the
		// operator finished a bin but Core did not record it. The Edge-side
		// SendError alone leaves nothing in the Core log with debug off, so the
		// dropped count is unforensicable after the fact. Log it loudly first.
		log.Printf("dispatch: ingest REJECTED station=%s uuid=%s payload=%s bin=%s source=%s: [%s] %s",
			stationID, p.OrderUUID, p.PayloadCode, p.BinLabel, p.SourceNode, lifecycleErr.Code, lifecycleErr.Detail)
		d.replies.SendError(env, p.OrderUUID, lifecycleErr.Code, lifecycleErr.Detail)
		return
	}
}

func (d *Dispatcher) failOrder(order *orders.Order, env *protocol.Envelope, errorCode, detail string) {
	stationID := env.Src.Station
	if err := d.lifecycle.Fail(order, stationID, errorCode, detail); err != nil {
		d.dbg("fail order %d: %v", order.ID, err)
	}
	d.sendError(env, order.EdgeUUID, errorCode, detail)
}

// SetPostFindHook installs a test-only hook the fulfillment scanner fires between
// Find and Claim — the single claim point after the claim-move to the scanner. Used
// for deterministic concurrency testing (a claim race must re-queue, never drop).
func (d *Dispatcher) SetPostFindHook(fn func()) {
	d.postFindHook = fn
}

// PostFindHook fires the installed find→claim hook (a no-op when none is set).
// Satisfies fulfillment.Dispatcher so the scanner can invoke it at its single
// claim point without importing the test seam directly.
func (d *Dispatcher) PostFindHook() {
	if d.postFindHook != nil {
		d.postFindHook()
	}
}

// LaneLock returns the dispatcher's lane lock for external use.
func (d *Dispatcher) LaneLock() *LaneLock { return d.laneLock }

// LaneForNode reports the lane a node sits in, or nil when the node is not a
// lane slot. A read error is returned, never swallowed — a caller that cannot
// tell whether a node is in a lane must fail closed, not assume it is not.
//
// Exported for the ONE caller that must answer this question WITHOUT admission:
// the manual robot-move command (www/handlers_robots.go), which creates no order
// and so has nothing to park, nothing to release, and nothing to wake. Its rule
// is geometry rather than state — a lane destination is refused outright — and
// that rule needs the lane fact and nothing else. Any caller that has an order
// should be asking admission instead.
func (d *Dispatcher) LaneForNode(nodeID int64) (*nodes.Node, error) {
	return d.db.LaneForNode(nodeID)
}

// Lifecycle returns the dispatcher's lifecycle service for external use (e.g. auto-confirm).
func (d *Dispatcher) Lifecycle() *LifecycleService { return d.lifecycle }

// EnableFutilityDetector installs the rate-per-tuple detector (futility.go).
// A no-op when cfg.Enabled is false. Wired from the composition root after
// config load, like DebugLog — NewDispatcher takes no config, and the
// thresholds must come from YAML rather than a constant here.
func (d *Dispatcher) EnableFutilityDetector(cfg FutilityConfig, logFn func(string, ...any)) *FutilityDetector {
	det := NewFutilityDetector(cfg, logFn, d.db)
	d.lifecycle.futility = det
	return det
}
