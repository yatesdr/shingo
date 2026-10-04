package engine

import (
	"fmt"
	"log"
	"sync"

	"shingo/protocol"
	ordermgr "shingoedge/orders"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// RequestProduceSwap (formerly FinalizeProduceNode — the /finalize route
// keeps its name) dispatches the order(s) to remove the filled bin and bring
// the next empty. Builds a ProducePlan (pure validation + dispatch shape)
// and then applies it. Swap-mode dispatch shape is shared with consume
// via SwapDispatch — the robot doesn't care whether the bin is filling
// or emptying, the choreography is the same.
//
// This call only REQUESTS robots, in every mode. The count splits at the
// operator's RELEASE (owner, 2026-09-30): the manifest, the ingest and the
// slot clear — the actual "finalize" — happen at the release of the leg that
// takes the bin away (finalizeDepartingProduce), because every part pressed
// between this call and RELEASE still belongs to the departing bin.
// RequestProduceSwap is the OPERATOR entry point for the evacuate direction —
// the mirror of RequestNodeMaterial, with the same dual-trigger shape. Tick-
// driven callers use requestProduceSwapFor with the autoreorder trigger.
func (e *Engine) RequestProduceSwap(nodeID int64) (*NodeOrderResult, error) {
	return e.requestProduceSwapFor(nodeID, protocol.EpisodeTriggerOperator)
}

func (e *Engine) requestProduceSwapFor(nodeID int64, trigger string) (*NodeOrderResult, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	return e.produceRequest(node, runtime, claim, produceAsk{trigger: trigger, finalizes: true})
}

// produceAsk is how the two buttons onto a produce line differ. Everything else
// about them, from the occupancy read to the orders, is one path, so a line
// with no bin, a press with a bare paired position and a keep-staged spot get
// the same answer whichever button was pressed.
type produceAsk struct {
	// trigger is the episode's trigger for a request that finalizes.
	trigger string
	// finalizes is the produce request: it finalizes the filled bin a swap takes
	// away, so the guards about the parts are its own. The press's live part
	// (guardCatidMismatch) must be the style's, and a swap needs parts counted.
	// The empty-bin request asks for an empty whatever is counted and whatever
	// part the press reports, and joins the cell's episode as an operator's ask.
	finalizes bool
}

// produceRequest is the one door onto a produce line's plan. The guards that
// are about sending a robot to the line run for both buttons; the guards that
// are about the parts run only for the request that finalizes (produceAsk).
func (e *Engine) produceRequest(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, ask produceAsk) (*NodeOrderResult, error) {
	// A2 (hop 2026-07-23): refuse outgoing-style relief while a changeover is
	// armed on this process — don't let a produce swap race the cutover.
	if err := e.guardStyleTransition(node, claim); err != nil {
		return nil, err
	}
	// A5 (hop 2026-07-23): refuse outgoing-style relief when the press's live
	// CATID says the wrong part is physically on it — the ground-truth sibling
	// of the changeover guard above.
	if ask.finalizes {
		if err := e.guardCatidMismatch(node, claim); err != nil {
			return nil, err
		}
	}

	// The partial-empty prime reads two things and then writes: what is
	// physically on the cell's positions, and what empties are already on
	// their way to them. Both reads and the create that follows sit inside one
	// per-cell lock so a double-tap cannot fire two empties at one bare
	// position. Cheap: these are operator clicks and autoreorder ticks, not a
	// hot path.
	mu := e.primeNodeLock(claim)
	mu.Lock()
	defer mu.Unlock()

	// Asked again under the lock, for the reason RequestNodeMaterial gives: a
	// changeover start holds a keep-staged line's lock, so this request may be let
	// in only after the changeover is armed.
	if claim.KeepStaged {
		if err := e.guardStyleTransition(node, claim); err != nil {
			return nil, err
		}
	}

	occ, spot, park := e.claimOccupancy(claim)
	occupancy := e.occupancyKnownNodesOnly(occ, node.Name)
	primedPositions, err := e.pairedPositionsAlreadyPrimed(node, claim)
	if err != nil {
		return nil, err
	}

	// See swap_evac_dest.go: the outgoing carrier goes to ITS home, not the
	// requested style's. Blank override = today's behaviour.
	swapClaim := withResidentEvacDest(claim, e.residentEvacDest(runtime, claim))

	plan, err := BuildProducePlan(node, runtime, swapClaim, occupancy, primedPositions)
	if err != nil {
		return nil, err
	}
	// THE COUNT IS THE FINALIZING REQUEST'S QUESTION. It finalizes the filled bin
	// a swap takes away, so a swap with no parts counted is refused. Only a swap:
	// an empty to a bare line and a press's primes take nothing away.
	if ask.finalizes && plan.Dispatch != nil && runtime.RemainingUOPCached <= 0 {
		return nil, fmt.Errorf("node %s has no parts to finalize", node.Name)
	}
	if err := e.planProduceRows(node, runtime, claim, plan, spot); err != nil {
		return nil, err
	}
	if plan.SuppressSwap {
		if len(plan.PrimePairedPositions) == 0 {
			// HOLD: every bare position already has an empty on the way. Refuse
			// BEFORE the episode opens — an episode with expected_orders 0 is
			// noise, and the operator needs a sentence, not a silent success.
			//
			// TYPED, because this refusal is the system working. Rendered as a
			// red error it reads as a fault the operator has to do something
			// about, and the only correct response is to wait. The type is what
			// lets the station render it as a notice instead.
			return nil, &PrimeInFlightError{NodeName: node.Name}
		}
		dests := make([]string, 0, len(plan.PrimePairedPositions))
		for _, p := range plan.PrimePairedPositions {
			dests = append(dests, p.Dest)
		}
		log.Printf("[produce-swap] node %s: head occupied, paired %v bare — priming from %s, no swap this round",
			node.Name, dests, claim.InboundSource)
	}

	// Bug 3 guard: refuse to start a second swap on top of an in-flight one.
	// Edge-runtime-only — Core anomalies don't shut down the line.
	if plan.Dispatch != nil && plan.Dispatch.RequiresActiveSwapGuard {
		if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
			return nil, err
		}
	}

	e.clearStrandedPark(node, claim, park)

	// The evacuate-direction episode, opened after the plan exists and before
	// any order does — same ordering and same reasoning as the consume side.
	//
	// A primes-only round opens the episode HERE TOO, deliberately. A prime is
	// a supply move and this is nominally the evacuate entry point, but the
	// cell episode is direction-agnostic by design (see openEpisodeForProduce
	// below): this path and RequestEmptyBin already join the ONE episode for
	// this cell's circle rather than opening a row each. Splitting a supply
	// episode out for the primes-only round would re-create exactly the
	// two-rows-for-one-cell shape that change removed. expected_orders comes
	// from ProducePlan.OrderCount, which counts the primes, for both buttons.
	//
	// A produce node's level runs the OTHER WAY: it fills toward capacity
	// rather than draining toward a reorder point, so "needs attention" is a
	// HIGH reading. The episode still means one thing — this process needs
	// material moved, in this direction — which is why direction is part of the
	// episode key and not a separate kind.
	var origin ordermgr.Origin
	if ask.finalizes {
		origin = e.openEpisodeForProduce(node, runtime, claim, plan, ask.trigger)
	} else {
		origin = e.requestEmptyOrigin(node, claim, runtime.RemainingUOPCached, plan.OrderCount())
	}

	result, err := e.applyProducePlan(node, runtime, claim, plan, origin)
	if err != nil {
		return nil, err
	}
	e.applySpotPlan(node, claim, plan.Spot, spot, origin)
	return result, nil
}

// planProduceRows reads the line's rows once, for what the keep-staged spot has
// coming and leaving and for the empty-line plan's guard, and applies both. Only
// a keep-staged claim with a known spot, or an empty-line plan, pays the read.
func (e *Engine) planProduceRows(
	node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, plan *ProducePlan, spot spotRead,
) error {
	spotKnown := claim.KeepStaged && spot.known
	if !spotKnown && !plan.SimpleMove {
		return nil
	}
	if plan.SimpleMove {
		if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
			return err
		}
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		return fmt.Errorf("node %s: cannot tell what is on its way to it (%w) — the next request will re-ask", node.Name, err)
	}
	if spotKnown {
		leaving := spotLeaving(rows, claim.InboundStaging, claim.CoreNodeName)
		planSpotForProduce(plan, claim, spot.lessLeaving(leaving), spotComing(rows, claim))
	}
	if !plan.SimpleMove {
		return nil
	}
	if err := positionWorkedBy(node, claim, rows); err != nil {
		return err
	}
	log.Printf("[produce-swap] node %s is empty (no bin), sending an empty from %s instead of a %s swap",
		node.Name, plan.SimpleSource, claim.SwapMode)
	return nil
}

// applyProduceEmptyLine creates the empty-line plan's one order: the spare on the
// spot moved to the line, named as the empty it is, or an empty retrieved from
// the inbound source. It takes the line's active slot, as the consume side's
// delivery does, so a second request waits for it.
func (e *Engine) applyProduceEmptyLine(node *processes.Node, claim *processes.NodeClaim, plan *ProducePlan, origin ordermgr.Origin) (*NodeOrderResult, error) {
	nodeID := node.ID
	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm
	var order *orders.Order
	var err error
	if plan.FromSpot {
		order, err = e.orderMgr.CreateMoveOrderCarryingTo(&nodeID, plan.SimpleSource, claim.CoreNodeName, "", autoConfirm, origin)
	} else {
		order, err = e.orderMgr.CreateRetrieveOrder(&nodeID, true, 1, claim.CoreNodeName, plan.SimpleSource, "",
			"standard", claim.PayloadCode, autoConfirm, false, origin)
	}
	if err != nil {
		return nil, err
	}
	if err := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &order.ID); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
	}
	// A press's bare paired positions get their empties alongside, outside the
	// runtime slots, as the consume side's downgrade primes do. The line's
	// empty is already on its way; a failed prime is returned, not rolled back.
	primes, err := e.createProducePrimes(node, claim, plan.PrimePairedPositions, origin)
	if err != nil {
		return nil, err
	}
	return &NodeOrderResult{Order: order, PrimeOrders: primes, ProcessNodeID: nodeID}, nil
}

// createProducePrimes creates one retrieve-empty per bare paired position. A
// retrieve, not a move: a move is a full-intent local relocation of the bin AT a
// concrete source node, so it would hunt a FULL bin in what is an empties pool.
// The merged auto-confirm signal: one policy for both directions of the cell.
func (e *Engine) createProducePrimes(node *processes.Node, claim *processes.NodeClaim, primes []SimplePrime, origin ordermgr.Origin) ([]*orders.Order, error) {
	nodeID := node.ID
	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm
	var out []*orders.Order
	for _, p := range primes {
		// No re-read: CreateRetrieveOrder already returns the stored row and
		// nothing below rewrites it.
		po, err := e.orderMgr.CreateRetrieveOrder(&nodeID, true, 1,
			p.Dest, p.Source, "", "standard", claim.PayloadCode,
			autoConfirm, false, origin)
		if err != nil {
			return nil, fmt.Errorf("prime %s: %w", p.Dest, err)
		}
		out = append(out, po)
	}
	return out, nil
}

// PrimeInFlightError says a press-index swap was refused because the empty it
// needs is already on its way. It is ADVISORY: nothing is wrong, nothing needs
// fixing, and the next press of the button after the bin lands will run the
// swap.
//
// A distinct type rather than a message the UI matches on. The station has to
// decide a colour, and deciding it by substring is how a reworded sentence
// silently turns an all-clear back into a red alarm.
type PrimeInFlightError struct {
	NodeName string
}

func (e *PrimeInFlightError) Error() string {
	return fmt.Sprintf("node %s: an empty bin is already inbound to the index position — "+
		"the swap will run once it lands", e.NodeName)
}

// Advisory reports that this refusal is the system behaving correctly rather
// than a fault. The handler keys on the behaviour, not on the concrete type,
// so a second advisory refusal later needs no handler change.
func (e *PrimeInFlightError) Advisory() bool { return true }

// primeNodeLock returns the per-cell prime mutex, creating it on first use.
// Keyed by the claim's CORE node name so every process_node row that shares
// one physical cell serialises against the same lock — the in-flight count it
// protects is itself scoped by delivery node, and a shared core node carries
// many process_node rows for one slot.
func (e *Engine) primeNodeLock(claim *processes.NodeClaim) *sync.Mutex {
	key := ""
	if claim != nil {
		key = claim.CoreNodeName
	}
	m, _ := e.primeResv.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// pairedPositionsAlreadyPrimed reports which of the claim's paired positions
// already have a non-terminal empty inbound, so a second request while the
// first prime is still travelling adds nothing. Reuses the same in-flight
// count RequestEmptyBin uses for its one-slot anti-spam guard, scoped by
// delivery node for the same reason.
//
// FAILS CLOSED. A read error means we do not know what is inbound, and
// priming on that is how a position collects a carrier it has no room for; a
// refused request is a click the operator can repeat.
func (e *Engine) pairedPositionsAlreadyPrimed(node *processes.Node, claim *processes.NodeClaim) (map[string]bool, error) {
	if claim == nil || claim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return nil, nil
	}
	primed := map[string]bool{}
	for _, pos := range claim.ExtensionPositions() {
		n, err := e.countActiveOrdersAtNode(pos, func(o orders.Order) bool { return o.RetrieveEmpty })
		if err != nil {
			return nil, fmt.Errorf("node %s: check inbound empties at paired position %s: %w", node.Name, pos, err)
		}
		if n > 0 {
			primed[pos] = true
		}
	}
	return primed, nil
}

// occupancyKnownNodesOnly re-reads an "empty" telemetry answer as occupied
// when the node it names is not one Core knows.
//
// Core reports an unknown node as a PRESENT entry with Occupied=false
// (shingo-core/www/handlers_telemetry.go) — "there is no bin at a place that
// does not exist", which is true and is the wrong sentence to prime on. A
// typo'd or scenesync-reaped PairedCoreNode would read bare forever and take a
// carrier every cycle. The missing-entry default already covers a node Core
// declined to answer about; this covers the node Core answered about and does
// not have.
//
// AND IT MUST KNOW WHETHER IT HAD THE INPUT TO CHECK. An EMPTY node set is not
// evidence that a name is wrong — a fresh Edge, a restart or a Kafka gap all
// present that way, and Core answers node-bins over a different transport than
// the node-list sync. So an empty set SKIPS the check and says so, rather than
// suppressing every prime during the startup window and handing the cell back
// the un-sourceable swap this whole path exists to prevent. Same reading as
// coreNodeNameIsUnknown in www/handlers_process_nodes.go.
func (e *Engine) occupancyKnownNodesOnly(occ map[string]bool, nodeName string) map[string]bool {
	known := e.CoreNodes()
	if len(known) == 0 {
		log.Printf("[occupied-check] node %s: core node list is EMPTY, so the line's positions could not be "+
			"checked against Core's plant — reading telemetry as-is. This is not a pass: Core has "+
			"not been heard from.", nodeName)
		return occ
	}
	out := make(map[string]bool, len(occ))
	for name, occupied := range occ {
		out[name] = occupied
		if occupied || coreNodeKnown(known, name) {
			continue
		}
		log.Printf("[occupied-check] node %s: position %q is not a node Core knows (%d known) — reading it "+
			"as occupied, no delivery or prime. Check the spelling against the node picker, or sync nodes if Core "+
			"has just been reconfigured.", nodeName, name, len(known))
		out[name] = true
	}
	return out
}

// coreNodeKnown resolves a name against Core's synced node set, falling back
// to the bare child name. Core sends group children as "Group.CHILD";
// SetCoreNodes normally trims that on ingestion, but keeps the qualified form
// when two children collide on one bare name.
func coreNodeKnown(known map[string]protocol.NodeInfo, name string) bool {
	if _, ok := known[name]; ok {
		return true
	}
	for full := range known {
		if bareNodeName(full) == name {
			return true
		}
	}
	return false
}

// openEpisodeForProduce opens or joins the evacuate-direction episode.
//
// Best-effort: observability must never fail an evacuation. A full bin gets
// taken away whether or not we could record why.
func (e *Engine) openEpisodeForProduce(
	node *processes.Node, runtime *processes.RuntimeState,
	claim *processes.NodeClaim, plan *ProducePlan, trigger string,
) ordermgr.Origin {
	remaining := 0
	if runtime != nil {
		remaining = runtime.RemainingUOPCached
	}
	// DISCRETIONARY on this side means an operator called a swap on a bin the
	// system does not consider full. Same rule as the consume side: flag it,
	// conclude nothing. The count may be wrong, the capacity may be wrong, or
	// the operator may be clearing the line for a reason the system cannot see.
	discretionary := trigger == protocol.EpisodeTriggerOperator &&
		claim.UOPCapacity > 0 && remaining < claim.UOPCapacity

	// No direction argument — see openCellEpisode. This path's claim is a produce
	// one, and RequestEmptyBin's claim is the SAME produce claim: both now open
	// or join the one episode for this cell's circle, where before this site
	// opened an evacuate row and that one opened a supply row for the same cell.
	originID, _, err := e.openCellEpisode(
		node.ProcessID, claim, trigger,
		plan.OrderCount(), remaining, discretionary,
	)
	if err != nil {
		e.logFn("demand_episode: open %s episode node=%s: %v", claim.Role, node.Name, err)
	}
	if originID == "" {
		// No episode, so nothing to attach to. Say NOTHING rather than
		// guessing: an unstated class lets Core classify, where a wrong one
		// here would be indistinguishable from a real answer.
		return ordermgr.Origin{}
	}
	return ordermgr.Attached(originID)
}

// applyProducePlan is the impure half of the produce-finalize pipeline:
// it manifests the filled bin, dispatches the planned complex orders,
// resets node UOP, and re-reads the resulting orders. Direction-specific
// glue around the shared SwapDispatch.
func (e *Engine) applyProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, plan *ProducePlan, origin ordermgr.Origin) (*NodeOrderResult, error) {
	nodeID := node.ID
	if plan.SimpleMove {
		return e.applyProduceEmptyLine(node, claim, plan, origin)
	}

	// Primes-only round: fill the bare paired position(s) and mint nothing
	// else. No manifest (there is no departing bin), no dispatch, and NO
	// runtime order slots — those belong to the head node's serial-order
	// machinery for swap cycles, and a prime is not a swap. Same treatment the
	// consume side's downgrade primes get.
	//
	// A retrieve, not a move: a move is a full-intent local relocation of the
	// bin AT a concrete source node, so it would hunt a FULL bin in what is an
	// empties pool. RetrieveEmpty is the intent that matches.
	if plan.SuppressSwap {
		primes, err := e.createProducePrimes(node, claim, plan.PrimePairedPositions, origin)
		if err != nil {
			return nil, err
		}
		return &NodeOrderResult{PrimeOrders: primes, ProcessNodeID: nodeID}, nil
	}

	// No paperwork here, in any mode: the bin keeps filling until the
	// operator's RELEASE, which finalizes it (finalizeDepartingProduce). The
	// runtime ORDER pointers stamp below (release resolution and the
	// swap_ready gate depend on them).
	// Produce always has a swap mode now (BuildProducePlan errors otherwise), so
	// Dispatch is always set.
	dispatch := plan.Dispatch
	// BOTH UUIDs BEFORE EITHER CREATE. Each leg goes in already naming its
	// partner, so neither is ever unpaired and creation order stops being a
	// correctness input — see CreateComplexOrderPaired.
	//
	// Creation ORDER is unchanged and still deliberate: the outbox drains
	// strictly ORDER BY id, so A is the leg Core is asked for first.
	uuidA, uuidB := ordermgr.NewOrderUUID(), ""
	if dispatch.StepsB != nil {
		uuidB = ordermgr.NewOrderUUID()
	}
	sibA, sibB := coreSiblings(dispatch.StepsA, dispatch.StepsB, uuidA, uuidB)
	orderA, err := e.dispatchPairedLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, sibA, uuidA, origin)
	if err != nil {
		return nil, err
	}

	var orderB *orders.Order
	if dispatch.StepsB != nil {
		orderB, err = e.dispatchPairedLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, sibB, uuidB, origin)
		if err != nil {
			return nil, err
		}
	}

	var orderBID *int64
	if orderB != nil {
		orderBID = &orderB.ID
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, orderBID); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
	}
	if orderB != nil {
		// Return-error on failure: see comment in
		// operator_stations.go:LinkOrderSiblings call site.
		if err := e.db.LinkOrderSiblings(orderA.ID, orderB.ID); err != nil {
			return nil, fmt.Errorf("link order siblings %d↔%d: %w", orderA.ID, orderB.ID, err)
		}
	}

	orderA, err = e.refreshOrder(orderA.ID)
	if err != nil {
		return nil, err
	}
	if orderB != nil {
		orderB, err = e.refreshOrder(orderB.ID)
		if err != nil {
			return nil, err
		}
	}

	if orderB == nil {
		return &NodeOrderResult{Order: orderA, ProcessNodeID: nodeID}, nil
	}
	return &NodeOrderResult{OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}

// dispatchComplexLeg issues a single complex order with the right auto-
// confirm wiring. Direction-agnostic — produce passes quantity=1 (the
// bin), consume passes the operator-requested quantity. processNodeName
// is the line node both legs of a swap belong to (= claim.CoreNodeName);
// threaded into ComplexOrderRequest.ProcessNode so Core picks the line
// bin for order.BinID.
// dispatchComplexLeg creates one leg of a swap.
//
// THE ORIGIN IS ON THE SIGNATURE AND PASSED BY THE CALLER, never resolved
// inside. This function is SHARED with origin-less callers, and a lookup here
// would have to guess which episode a leg belongs to from the node — which is
// exactly the read-time attribution the stamp-forward rule exists to avoid.
// The caller knows, because the caller opened the episode.
//
// Both legs of a pair are given the SAME origin: one fire of the plan is one
// demand served by two rows.
// dispatchPairedLeg is dispatchComplexLeg for a leg whose uuid was minted with
// its partner's. Same wiring; the only difference is that the uuid arrives
// rather than being invented inside the create.
func (e *Engine) dispatchPairedLeg(nodeID int64, quantity int64, steps []protocol.ComplexOrderStep, deliveryNode, processNodeName string, autoConfirm bool, siblingUUID, orderUUID string, origin ordermgr.Origin) (*orders.Order, error) {
	dn := deliveryNode
	if autoConfirm {
		dn = ""
	}
	return e.orderMgr.CreateComplexOrderPaired(&nodeID, quantity, dn, processNodeName, steps, autoConfirm, "", siblingUUID, orderUUID, origin)
}

// refreshOrder re-reads an order after the runtime-orders write so the
// caller sees the updated process_node_id linkage in the response.
func (e *Engine) refreshOrder(orderID int64) (*orders.Order, error) {
	o, err := e.db.GetOrder(orderID)
	if err != nil {
		log.Printf("produce: re-read order %d after runtime update: %v", orderID, err)
		return nil, fmt.Errorf("re-read order %d: %w", orderID, err)
	}
	return o, nil
}
