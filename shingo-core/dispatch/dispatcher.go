package dispatch

import (
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

type Dispatcher struct {
	db               *store.DB
	backend          fleet.Backend
	emitter          Emitter
	resolver         NodeResolver
	laneLock         *LaneLock
	stationID        string
	dispatchTopic    string
	lifecycle        *LifecycleService
	replies          *ReplySender
	planner          *PlanningService
	binManifest      *service.BinManifestService
	allocator        *Allocator
	restoreListeners *restoreRegistry
	laneHolds        *laneHoldRegistry
	DebugLog         func(string, ...any)

	// postFindHook is a test-only seam fired by the fulfillment scanner between
	// Find and Claim (the single claim point after the claim-move to the scanner).
	// Nil in production; set via SetPostFindHook for deterministic concurrency tests.
	postFindHook func()
}

func NewDispatcher(db *store.DB, backend fleet.Backend, emitter Emitter, stationID, dispatchTopic string, resolver NodeResolver) *Dispatcher {
	binManifest := service.NewBinManifestService(db)
	d := &Dispatcher{
		db:               db,
		backend:          backend,
		emitter:          emitter,
		resolver:         resolver,
		laneLock:         NewLaneLock(),
		stationID:        stationID,
		dispatchTopic:    dispatchTopic,
		binManifest:      binManifest,
		restoreListeners: newRestoreRegistry(),
		laneHolds:        newLaneHoldRegistry(),
	}
	d.lifecycle = newLifecycleService(db, backend, emitter, resolver, binManifest, d.dbg)
	d.replies = newReplySender(db, dispatchTopic, stationID, d.dbg)
	d.planner = newPlanningService(db, resolver, d.laneLock, d.dbg, d.CreateCompoundOrder)
	d.allocator = newAllocator(db, binManifest, d.dbg)
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
			return fmt.Errorf("%w: %s", ErrReshuffleWait, pe.Detail)
		}
		return pe
	}
	return nil
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
// now" is not a broken lane (D18-Q4 wait-not-fail). The no-shuffle-slot half used
// to fail terminally at intake — sim order 21 on the 2026-07-10 houseserver run
// died that way — and D79's reshuffle-disposition rider assigned the fix here,
// because it is only actually FIXED once the scanner can re-plan on a later tick.
//
// A sentinel (rather than an exported predicate over the unexported
// planningError) so callers outside the package — and their tests — can both
// match it with errors.Is and construct it.
var ErrReshuffleWait = errors.New("reshuffle not plannable yet")

func (d *Dispatcher) queueOrder(order *orders.Order, env *protocol.Envelope, payloadCode string) {
	if err := d.lifecycle.Queue(order, "dispatcher", "awaiting inventory"); err != nil {
		d.dbg("queue order %d: %v", order.ID, err)
	}
	if payloadCode != "" && order.PayloadCode == "" {
		if err := d.db.UpdateOrderPayloadCode(order.ID, payloadCode); err != nil {
			d.dbg("update payload code order %d: %v", order.ID, err)
		}
	}
	d.dbg("queued: order=%d uuid=%s payload=%s delivery=%s", order.ID, order.EdgeUUID, payloadCode, order.DeliveryNode)
	d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, env.Src.Station, payloadCode)
	d.replies.SendUpdate(env, order.EdgeUUID, string(StatusQueued), "awaiting inventory")
}

func (d *Dispatcher) dispatchToFleet(order *orders.Order, env *protocol.Envelope, sourceNode, destNode *nodes.Node) {
	vendorOrderID, err := d.dispatchToFleetCore(order, sourceNode, destNode)
	if err != nil {
		d.failOrder(order, env, "fleet_failed", err.Error())
		return
	}

	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode.Name)
	_ = vendorOrderID
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
func (d *Dispatcher) dispatchToFleetCore(order *orders.Order, sourceNode, destNode *nodes.Node) (string, error) {
	vendorOrderID := fmt.Sprintf("%s%d-%s", VendorIDPrefix, order.ID, uuid.New().String()[:8])

	plan := buildTransportPlan(sourceNode.Name, destNode.Name, order.SourceIntent == SourceIntentEmpty)
	blocks := stepsToBlocks(vendorOrderID, plan, 0)
	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   order.Priority,
		RobotGroup: d.robotGroupForPayload(order.PayloadCode),
		Complete:   true, // no-wait: the fleet completes the order once its 2 blocks finish
	}

	d.dbg("fleet dispatch: order=%d vendor_id=%s from=%s to=%s priority=%d",
		order.ID, vendorOrderID, sourceNode.Name, destNode.Name, order.Priority)

	if _, err := d.backend.CreateOrder(req); err != nil {
		d.dbg("fleet create order failed: %v", err)
		return "", err
	}

	d.dbg("order %d dispatched as %s (%s -> %s)", order.ID, vendorOrderID, sourceNode.Name, destNode.Name)

	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		d.dbg("update order %d vendor: %v", order.ID, err)
	}
	if err := d.lifecycle.Dispatch(order, vendorOrderID, "dispatcher"); err != nil {
		d.dbg("order %d → dispatched: %v", order.ID, err)
	}

	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode.Name, destNode.Name)
	return vendorOrderID, nil
}

// DispatchDirect dispatches an order to the fleet without a protocol envelope.
// Used for orders created internally (e.g. direct orders from the UI) and
// from the fulfillment scanner after a bin claim resolves.
//
// Callers reach this function with the order in one of three states:
//   - pending  — direct creation paths (engine.CreateDirectOrder,
//     www/spot handlers) jump straight from intake to dispatch.
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

	vendorOrderID, err := d.dispatchToFleetCore(order, sourceNode, destNode)
	if err != nil {
		if failErr := d.lifecycle.Fail(order, order.StationID, "fleet_failed", err.Error()); failErr != nil {
			d.dbg("fail order %d: %v", order.ID, failErr)
		}
		return "", err
	}

	return vendorOrderID, nil
}

// checkOwnership verifies the envelope sender owns the order.
// Core-role senders (e.g. UI-initiated actions) are always allowed.
func (d *Dispatcher) checkOwnership(env *protocol.Envelope, order *orders.Order) bool {
	if env.Src.Role == protocol.RoleCore {
		return true
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

	// Cascade cancellation to all children before cancelling the parent.
	// This ensures in-flight children have their fleet orders cancelled,
	// bins unclaimed, and the lane lock released.
	//
	// Pre-fix this was gated on order.Status == StatusReshuffling. The
	// guard is brittle: once a complex parent resumes from Reshuffling
	// back to Queued (see lifecycle.ResumeCompound), a cancel against
	// the Queued parent skipped the cascade and orphaned still-running
	// children. cancelCompoundChildren is already idempotent for non-
	// compound orders — ListChildOrders returns an empty slice and the
	// loop no-ops — so the unconditional call costs one extra SELECT
	// per cancel of a non-compound order.
	d.cancelCompoundChildren(order, stationID, p.Reason)

	d.lifecycle.CancelOrder(order, stationID, p.Reason)
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

// HandleOrderRedirect processes a redirect request from ShinGo Edge.
func (d *Dispatcher) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	d.dbg("redirect: uuid=%s new_dest=%s", p.OrderUUID, p.NewDeliveryNode)

	order, ok := d.getOwnedOrder(env, p.OrderUUID)
	if !ok {
		d.replies.SendError(env, p.OrderUUID, "not_found", "order not found or access denied")
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
	d.dispatchToFleet(order, env, sourceNode, newDest)
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

// Lifecycle returns the dispatcher's lifecycle service for external use (e.g. auto-confirm).
func (d *Dispatcher) Lifecycle() *LifecycleService { return d.lifecycle }
