package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// NodeOrderResult is what an operator's material action returns.
//
// NO cycle_mode. It had no reader anywhere — and worse, it could not have
// been a useful one: a primes-only produce round and a consume downgrade both
// report "simple", so the one discrimination anyone would reach for it to make
// is the one it cannot make. Round 2 already had to key primeNoticeText on the
// swap legs instead and write a test forbidding the cycle_mode reading. A
// field that is unread AND fenced off by a test is not costing a line, it is
// a trap; the round-3 body audit is what settled removing it.
type NodeOrderResult struct {
	Order  *storeorders.Order `json:"order,omitempty"`
	OrderA *storeorders.Order `json:"order_a,omitempty"`
	OrderB *storeorders.Order `json:"order_b,omitempty"`
	// PrimeOrders are additional simple deliveries emitted alongside Order
	// when a press-index empty-station downgrade prime-filled the paired
	// positions. Empty for non-press-index requests and for press-index
	// downgrades where the paired positions were already occupied.
	PrimeOrders   []*storeorders.Order `json:"prime_orders,omitempty"`
	ProcessNodeID int64                `json:"process_node_id"`
}

// primary is the order a caller that reports one order names: the plain order,
// the first leg of a swap, or the first prime of a primes-only round.
func (r *NodeOrderResult) primary() *storeorders.Order {
	switch {
	case r == nil:
		return nil
	case r.Order != nil:
		return r.Order
	case r.OrderA != nil:
		return r.OrderA
	case len(r.PrimeOrders) > 0:
		return r.PrimeOrders[0]
	}
	return nil
}

// RequestNodeMaterial is the OPERATOR entry point for the supply direction.
//
// The trigger matters to the demand episode and cannot be inferred here:
// neither entry point checks AutoReorder or the level, so an operator can
// request on a node the system considers perfectly fine. Tick-driven callers
// use requestNodeMaterialFor with the autoreorder trigger instead.
func (e *Engine) RequestNodeMaterial(nodeID int64, quantity int64) (*NodeOrderResult, error) {
	return e.requestNodeMaterialFor(nodeID, quantity, protocol.EpisodeTriggerOperator)
}

func (e *Engine) requestNodeMaterialFor(nodeID int64, quantity int64, trigger string) (*NodeOrderResult, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if quantity < 1 {
		quantity = 1
	}

	return e.requestNodeFromClaim(node, runtime, claim, quantity, trigger)
}

// claimOccupancy resolves Core-telemetry occupancy for the head node plus
// any paired positions on the claim. Returns a map keyed by core node
// name. Missing entries (Core unreachable or node not returned) are
// treated as occupied by isOccupied — safe default that suppresses both
// the downgrade and any paired-prime emission so a Core blip can't
// dispatch phantom deliveries.
func (e *Engine) claimOccupancy(claim *processes.NodeClaim) (map[string]bool, spotRead, NodeBinInfo) {
	occ := map[string]bool{}
	if claim == nil {
		return occ, spotRead{}, NodeBinInfo{}
	}
	// The head node unconditionally, EVEN IF BLANK: the map is keyed by the
	// names asked about and isOccupied treats a missing key as occupied, so
	// dropping a blank head here would change a suppressing answer into a
	// permissive one. The extensions come from the claim's geometry, and only
	// for a press — a stale PairedCoreNode on some other mode is not a position
	// this cell occupies and must not be queried.
	names := []string{claim.CoreNodeName}
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		names = append(names, claim.ExtensionPositions()...)
	}
	// A KEEP-STAGED CLAIM ASKS ABOUT ITS SPOT IN THE SAME CALL: one more name, no
	// more round trips. The spot's row is kept whole (whether a bin stands there
	// and what it carries) and stays out of the map, which answers only for the
	// line's own positions.
	asked := names
	if claim.KeepStaged && claim.InboundStaging != "" {
		asked = append(append([]string(nil), names...), claim.InboundStaging)
	}
	// A SINGLE-ROBOT CLAIM ASKS ABOUT ITS OUTBOUND STAGING TOO, in the same call,
	// for a bin a cancelled changeover's leg left parked there (parkAsked). Its
	// row stays out of the map as well.
	if parkAsked(claim) {
		asked = append(append([]string(nil), asked...), claim.OutboundStaging)
	}
	if !e.coreClient.Available() {
		log.Printf("[occupied-check] core API not configured, assuming occupied for %v", names)
		for _, n := range names {
			occ[n] = true
		}
		return occ, spotRead{}, NodeBinInfo{}
	}
	// THE DECISION HERE IS ALREADY RIGHT AND IS NOT CHANGING. The map is filled
	// from the REQUESTED names rather than the returned rows, so a name Core
	// never answered about gets an explicit value, and that value is `occupied`
	// — the suppressing answer. Downstream, BuildConsumePlan only downgrades a
	// swap mode or emits a paired prime when a node reads NOT occupied, so an
	// all-occupied map suppresses both. This is the site the others should copy.
	//
	// What it did not do was say which it was. The flag is read for the log line
	// only: "no data from core" covers a node Core answered nothing about and a
	// read that never left the building, and an over-ordering incident has to be
	// reconstructable from logs. It also pins the invariant here rather than
	// inheriting it from FetchNodeBins returning nil on every failure — if that
	// ever returned partial rows alongside an error, this site would silently
	// start trusting a partial read.
	bins, reachable, ferr := e.coreClient.FetchNodeBins(asked)
	var park NodeBinInfo
	for _, b := range bins {
		if b.NodeName == claim.InboundStaging && claim.KeepStaged && b.NodeName != claim.CoreNodeName {
			continue // the spot: read below, not a line position
		}
		if parkAsked(claim) && b.NodeName == claim.OutboundStaging && b.NodeName != claim.CoreNodeName {
			park = b
			continue
		}
		occ[b.NodeName] = b.Occupied
	}
	for _, n := range names {
		if _, ok := occ[n]; !ok {
			log.Printf("[occupied-check] node %s: occupancy=%s, assuming occupied",
				n, OccupancyOutcome(reachable, ferr))
			occ[n] = true
		}
	}
	return occ, spotOf(claim, bins, e.spotNodeKnown), park
}

// spotNodeKnown reports whether Core has a keep-staged spot's node. Core answers
// an unknown node as present and empty, which would read a typo'd spot as bare
// on every request and send a refill each time. An empty node list is Core not
// heard from yet, not evidence of a typo, so it skips the check, as
// occupancyKnownNodesOnly does for the same reason.
func (e *Engine) spotNodeKnown(name string) bool {
	known := e.CoreNodes()
	return len(known) == 0 || coreNodeKnown(known, name)
}

// requestNodeFromClaim constructs orders using style_node_claims routing.
// Builds a ConsumePlan (pure validation + dispatch shape) and applies it.
// If the node is physically empty (no bin per Core telemetry), the planner
// downgrades any non-simple swap mode to a simple move — there is nothing
// to swap out.
func (e *Engine) requestNodeFromClaim(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, quantity int64, trigger string) (*NodeOrderResult, error) {
	// A2 (hop 2026-07-23): refuse outgoing-style relief while a changeover is
	// armed on this process — don't let a produce/consume swap race the cutover.
	if err := e.guardStyleTransition(node, claim); err != nil {
		return nil, err
	}
	// A5 (hop 2026-07-23): refuse outgoing-style relief when the press's live
	// CATID says the wrong part is physically on it — the ground-truth sibling
	// of the changeover guard above.
	if err := e.guardCatidMismatch(node, claim); err != nil {
		return nil, err
	}

	autoConfirm := false
	if claim != nil {
		autoConfirm = claim.AutoConfirm || e.cfg.Web.AutoConfirm
	}
	// ONE CELL, ONE DECISION AT A TIME. The occupancy read, the plan and the
	// apply run under the cell's prime lock, as the produce request does: a
	// keep-staged spot's refills are counted from rows this request is about to
	// write, and a second request or the level keeper's floor deciding in the
	// same moment has to see them.
	mu := e.primeNodeLock(claim)
	mu.Lock()
	defer mu.Unlock()

	// A CHANGEOVER CAN ARM WHILE THIS REQUEST WAITS FOR THE CELL. A changeover
	// start holds a keep-staged line's prime lock from its pre-dispatch cancel to
	// its last spot order, so a request that passed the guard above can be let in
	// only after the changeover is armed. Asked again under the lock, it is
	// refused like any request against an armed changeover instead of building an
	// outgoing-style swap that no changeover leg owns. Keep-staged lines only:
	// theirs is the lock a start holds.
	if claim.KeepStaged {
		if err := e.guardStyleTransition(node, claim); err != nil {
			return nil, err
		}
	}
	inbound, err := e.guardLineRequest(node, runtime, claim)
	if err != nil {
		return nil, err
	}

	// Read through occupancyKnownNodesOnly, as the produce request reads it: Core
	// answers a node it does not have as present and empty, and a head or paired
	// position named wrong would read bare on every request.
	occ, spot, park := e.claimOccupancy(claim)
	occupancy := e.occupancyKnownNodesOnly(occ, node.Name)

	// The evac leg lifts whatever is ON the cell, which is not always the style
	// being requested — see swap_evac_dest.go. Blank override = today's behaviour.
	swapClaim := withResidentEvacDest(claim, e.residentEvacDest(runtime, claim))

	plan, err := BuildConsumePlan(node, runtime, swapClaim, quantity, occupancy, inbound, autoConfirm)
	if err != nil {
		return nil, err
	}
	if claim.KeepStaged && spot.known {
		coming, leaving, cerr := e.readSpotComing(node, claim)
		if cerr != nil {
			return nil, fmt.Errorf("node %s: cannot tell what is on its way to %s (%w) — the next request will re-ask",
				node.Name, claim.InboundStaging, cerr)
		}
		planSpotForConsume(plan, claim, spot.lessLeaving(leaving), coming)
	}
	if plan.DowngradedFromSwapMode != "" {
		// THE DOWNGRADE IS THE ONE DECISION THAT IGNORES WHAT THIS CELL ALREADY
		// HAS IN FLIGHT, and it is the decision that mints a second delivery into
		// a position a robot is on its way to fill. Gate it here, before the log
		// line below, so "downgrading … to simple delivery" keeps meaning what it
		// says: the position is bare AND nothing is coming.
		//
		// Here and not in BuildConsumePlan because the planner is pure over
		// (node, runtime, claim, occupancy) and the witness is a DB read. This
		// function already holds what the guard needs.
		if err := e.guardPositionSpokenFor(node, runtime, claim); err != nil {
			return nil, err
		}
		if len(plan.PrimePairedPositions) > 0 {
			dests := make([]string, 0, len(plan.PrimePairedPositions))
			for _, p := range plan.PrimePairedPositions {
				dests = append(dests, p.Dest)
			}
			log.Printf("[request-material] node %s empty + paired empty: priming %v alongside %s delivery (downgraded from %s)",
				node.Name, dests, claim.CoreNodeName, plan.DowngradedFromSwapMode)
		} else {
			log.Printf("[request-material] node %s is empty (no bin), downgrading %s to simple delivery", node.Name, plan.DowngradedFromSwapMode)
		}
	}

	if plan.SuppressSwap {
		if err := e.guardPairedPrimes(node, runtime, claim, plan.PrimePairedPositions); err != nil {
			return nil, err
		}
	}

	// Do not ARM A PAIR INTO A SOURCE ALREADY KNOWN TO BE DRY. This is the
	// Springfield 2026-07-21 churn's fix at its cause: two legs that cannot
	// source were created hundreds of times per changeover, and every mechanism
	// downstream — including the spare that is now deleted — was coping with
	// orders that should never have existed. Refusing here creates nothing, so
	// nothing churns; the level keeper re-asks. A pair only: a single-robot swap
	// is one order and waits for stock.
	if plan.Dispatch != nil && plan.Dispatch.StepsB != nil {
		if err := e.guardSourceKnownDry(node, claim); err != nil {
			return nil, err
		}
	}

	e.clearStrandedPark(node, claim, park)

	// The demand episode is opened HERE — after the plan exists and before any
	// order does. That ordering is the whole reason expected_orders can be the
	// plan's own order count rather than a guess: the system's stated intent,
	// captured once, at the moment it is stated.
	//
	// Everything from here on belongs to this episode, including the primes and
	// both swap legs. Choreography is not demand.
	origin := e.openEpisodeForConsume(node, runtime, claim, plan, trigger)

	result, err := e.applyConsumePlan(node, plan, origin)
	if err != nil {
		return nil, err
	}
	e.applySpotPlan(node, claim, plan.Spot, spot, origin)
	return result, nil
}

// openEpisodeForConsume opens or joins the supply-direction episode for a
// consume request.
//
// Best-effort throughout: observability must never fail a material request. A
// cell that needs a bin gets its bin whether or not we could record why.
func (e *Engine) openEpisodeForConsume(
	node *processes.Node, runtime *processes.RuntimeState,
	claim *processes.NodeClaim, plan *ConsumePlan, trigger string,
) orders.Origin {
	remaining := 0
	if runtime != nil {
		remaining = runtime.RemainingUOPCached
	}
	// DISCRETIONARY: an operator asked on a node the system reads as fine.
	// Either the ledger is wrong, or the reorder point is too low, or the
	// operator knows something the count does not. FLAG, DO NOT CONCLUDE —
	// this records that it happened and says nothing about who was right.
	discretionary := trigger == protocol.EpisodeTriggerOperator &&
		claim.ReorderPoint > 0 && remaining > claim.ReorderPoint

	// No direction argument: the claim carries the role, and this path's claim is
	// a consume one. Passing the word here was how the backfill site came to pass
	// the wrong word at its own.
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
		return orders.Origin{}
	}
	return orders.Attached(originID)
}

// applyConsumePlan is the impure half of the consume-request pipeline:
// it issues the move order or planned complex order(s), records the
// runtime-orders linkage, and re-reads the resulting orders. Direction-
// specific glue around the shared SwapDispatch.
func (e *Engine) applyConsumePlan(node *processes.Node, plan *ConsumePlan, origin orders.Origin) (*NodeOrderResult, error) {
	nodeID := node.ID

	if plan.SuppressSwap {
		primes, err := e.createConsumePrimes(nodeID, plan, origin)
		if err != nil {
			return nil, err
		}
		return &NodeOrderResult{PrimeOrders: primes, ProcessNodeID: nodeID}, nil
	}
	if plan.SimpleMove {
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, plan.Quantity, plan.SimpleSource, plan.SimpleDest, plan.AutoConfirm, origin)
		if err != nil {
			return nil, err
		}
		if err := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &order.ID); err != nil {
			e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
		}
		order, err = e.refreshOrderStation(order.ID)
		if err != nil {
			return nil, err
		}
		// The head order is already created and is not rolled back if a prime
		// fails; the error is returned so the operator sees that priming was
		// incomplete.
		primes, err := e.createConsumePrimes(nodeID, plan, origin)
		if err != nil {
			return nil, err
		}
		return &NodeOrderResult{Order: order, PrimeOrders: primes, ProcessNodeID: nodeID}, nil
	}

	dispatch := plan.Dispatch
	// BOTH UUIDS BEFORE EITHER CREATE, as the produce and changeover doors do.
	// This door minted leg A's uuid inside its create, so A reached Core naming
	// no sibling and was a solo order on its own intake pass: the pair rule never
	// saw it. Creation order is unchanged — A first, so Core is asked for A first.
	uuidA, uuidB := orders.NewOrderUUID(), ""
	if dispatch.StepsB != nil {
		uuidB = orders.NewOrderUUID()
	}
	sibA, sibB := coreSiblings(dispatch.StepsA, dispatch.StepsB, uuidA, uuidB)
	orderA, err := e.dispatchPairedLeg(nodeID, plan.Quantity, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, sibA, uuidA, origin)
	if err != nil {
		return nil, err
	}

	var orderB *storeorders.Order
	if dispatch.StepsB != nil {
		orderB, err = e.dispatchPairedLeg(nodeID, plan.Quantity, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, sibB, uuidB, origin)
		if err != nil {
			return nil, err
		}
	}

	var orderBID *int64
	if orderB != nil {
		orderBID = &orderB.ID
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, orderBID); err != nil {
		e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
	}
	// Durable supply ↔ evac linkage. The runtime slots above can be
	// dropped by completion-time cleanup (per-order terminal clear, or a
	// changeover cancel) before release fires; the sibling
	// pointer survives so ReleaseStagedOrders and the supply guard can
	// still identify the pair.
	//
	// Return-error on failure: ComputeSwapReady's order-graph predicate
	// keys on the sibling pointer. A silent linkage miss here would
	// leave the operator with a pair the system can't recognize as
	// coordinated — swap_ready stays false, modal shows WAITING FOR
	// OTHER ROBOT with no escape. Aborting is the safer failure mode
	// because orderA/orderB are still recoverable via admin orders.
	if orderB != nil {
		if err := e.db.LinkOrderSiblings(orderA.ID, orderB.ID); err != nil {
			return nil, fmt.Errorf("link order siblings %d↔%d: %w", orderA.ID, orderB.ID, err)
		}
	}

	orderA, err = e.refreshOrderStation(orderA.ID)
	if err != nil {
		return nil, err
	}
	if orderB != nil {
		orderB, err = e.refreshOrderStation(orderB.ID)
		if err != nil {
			return nil, err
		}
	}

	if orderB == nil {
		return &NodeOrderResult{Order: orderA, ProcessNodeID: nodeID}, nil
	}
	return &NodeOrderResult{OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}

// createConsumePrimes creates one delivery of a full per bare paired position
// of a press: attributed to the head node for ownership and audit, NOT tracked
// in runtime slots (those belong to the head's serial-order machinery for swap
// cycles), as the produce side's primes are.
func (e *Engine) createConsumePrimes(nodeID int64, plan *ConsumePlan, origin orders.Origin) ([]*storeorders.Order, error) {
	var primes []*storeorders.Order
	for _, p := range plan.PrimePairedPositions {
		po, err := e.orderMgr.CreateMoveOrder(&nodeID, plan.Quantity, p.Source, p.Dest, plan.AutoConfirm, origin)
		if err != nil {
			return nil, fmt.Errorf("prime %s: %w", p.Dest, err)
		}
		refreshed, err := e.refreshOrderStation(po.ID)
		if err != nil {
			return nil, err
		}
		primes = append(primes, refreshed)
	}
	return primes, nil
}

// refreshOrderStation re-reads an order after the runtime-orders write
// using the consume side's e.logFn diagnostic surface.
func (e *Engine) refreshOrderStation(orderID int64) (*storeorders.Order, error) {
	o, err := e.db.GetOrder(orderID)
	if err != nil {
		e.logFn("station: re-read order %d after runtime update: %v", orderID, err)
		return nil, fmt.Errorf("re-read order %d: %w", orderID, err)
	}
	return o, nil
}

// CanAcceptOrders reports whether a process node can accept new orders.
// Returns false with a human-readable reason if the node is unavailable.
// Consolidates all availability checks: active/staged order, changeover.
//
// For manual_swap nodes, the serial order constraint (ActiveOrderID/StagedOrderID)
// is skipped — manual_swap uses a multi-order queue where multiple non-terminal
// orders are allowed simultaneously. The changeover check still applies.
func (e *Engine) CanAcceptOrders(nodeID int64) (bool, string) {
	// Check changeover first — applies regardless of runtime state.
	//
	// Scope the gate to nodes actually PARTICIPATING in the changeover. A node
	// that is not part of it — e.g. a bin loader that only supplies empties to
	// the line — must stay available; gating on the whole process wrongly
	// blocked the loader from calling an empty bin during a changeover on a
	// press sharing its process (the Springfield field report).
	//
	// PARTICIPANTS, not tasks. The task set is too narrow: a same-bin-type
	// press-index changeover never fans out, so its indexed-over positions own no
	// task at all and were left OPEN to unrelated dispatch while the index
	// motion was about to place a bin on them. Two bins on one node — the
	// catastrophic family. Participants are the superset that includes them.
	//
	// FAIL POSTURE IS DELIBERATELY HYBRID, and the split is where the cost
	// asymmetry inverts:
	//
	//   - Outer lookups (GetProcessNode, GetActiveProcessChangeover) stay
	//     byte-identical FAIL-OPEN. An error there is indistinguishable from
	//     "no changeover running", which is the overwhelmingly common case and
	//     the PLC-tick path; closing it would idle the plant on a transient
	//     read blip. This is also the Springfield-regression surface, untouched.
	//   - Once an active changeover IS resolved, the participant lookup fails
	//     CLOSED. A false "unavailable" there costs a blocked action during a
	//     changeover window — transient, visible, now panel-named. A false
	//     "available" is the two-bins case. The lookup is a single indexed
	//     point query against a table written at plan time, so an error means
	//     the Edge DB is failing — not a state in which to admit robot traffic
	//     to a node that may be about to receive a bin.
	node, err := e.db.GetProcessNode(nodeID)
	if err == nil {
		if co, coErr := e.db.GetActiveProcessChangeover(node.ProcessID); coErr == nil && co != nil {
			isParticipant, role, pErr := e.db.IsChangeoverParticipant(node.ProcessID, node.CoreNodeName)
			if pErr != nil {
				log.Printf("WARN CanAcceptOrders: participant lookup failed for node %s during active changeover %d: %v — failing CLOSED",
					node.CoreNodeName, co.ID, pErr)
				return false, "changeover in progress (participant lookup failed)"
			}
			if isParticipant {
				if role == domain.ParticipantRoleIndexedOver {
					// Distinct reason: this node owns no task, so an operator
					// looking at it has nothing to work and would otherwise have
					// no idea why it is refusing.
					return false, "changeover in progress (indexed-over position)"
				}
				return false, "changeover in progress"
			}
		}
		// A window/position of a Core-owned loader uses the multi-order queue even
		// without a per-style manual_swap claim (Core-owned loader refactor): mirror
		// the claim-based shortcut below so a synth-claim loader node isn't held to
		// the serial single-order constraint.
		if l, lerr := e.loaders().LoaderForNode(domain.NodeID(node.CoreNodeName)); lerr == nil && l != nil {
			return true, ""
		}
	}
	runtime, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || runtime == nil {
		return true, "" // no runtime state = available
	}

	// manual_swap nodes use a multi-order queue — skip the serial order constraint.
	//
	// SwapMode IS CONFIGURATION, so it is read from the node's configured claim
	// rather than by dereferencing active_claim_id. That pointer is written by
	// eighteen paths and can be stale, dangling, or nil, and none of those states
	// says anything about how this window swaps; a node whose claim row was
	// deleted would silently lose its multi-order queue and start refusing the
	// operator's second tap. claimAtNode answers the configuration question from
	// the style the process is running, or from the node's Core loader.
	if claim := e.claimAtNode(node); claim.IsLoaderNode() {
		return true, ""
	}

	// orderWorksTheCell, not !IsTerminal: a DEPARTED leg is still a live order
	// but is no longer this cell's business — it is a robot carrying a bin away
	// from a cell whose positions are all filled. See leg_departure.go.
	//
	// The reason strings are unchanged. An operator refused here is being told
	// which slot is holding them, and that has not moved.
	for _, orderID := range []*int64{runtime.ActiveOrderID, runtime.StagedOrderID} {
		if orderID == nil {
			continue
		}
		order, err := e.db.GetOrder(*orderID)
		if err == nil && orderWorksTheCell(order) {
			if orderID == runtime.ActiveOrderID {
				return false, "active order in progress"
			}
			return false, "staged order in progress"
		}
	}
	return true, ""
}

// (AbortNodeOrders removed 2026-07-28. Its only caller was
// StartProcessChangeover, which used it to cancel every in-flight order on the
// nodes a changeover was about to touch. On a press-index swap those orders are
// frequently carrying the empty carriers the changeover's own index legs must
// pick up, so cancelling them mid-delivery deadlocks the changeover it was
// meant to clear the way for. StartProcessChangeover now refuses and names the
// blocking order instead — see nodesWithOrdersInFlight.)
