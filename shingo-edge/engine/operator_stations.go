package engine

import (
	"fmt"
	"log"
	"strconv"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
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
func (e *Engine) claimOccupancy(claim *processes.NodeClaim) map[string]bool {
	occ := map[string]bool{}
	if claim == nil {
		return occ
	}
	names := []string{claim.CoreNodeName}
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		if claim.PairedCoreNode != "" {
			names = append(names, claim.PairedCoreNode)
		}
		if claim.SecondPairedCoreNode != "" {
			names = append(names, claim.SecondPairedCoreNode)
		}
	}
	if !e.coreClient.Available() {
		log.Printf("[occupied-check] core API not configured, assuming occupied for %v", names)
		for _, n := range names {
			occ[n] = true
		}
		return occ
	}
	bins, _, _ := e.coreClient.FetchNodeBins(names)
	for _, b := range bins {
		occ[b.NodeName] = b.Occupied
	}
	for _, n := range names {
		if _, ok := occ[n]; !ok {
			log.Printf("[occupied-check] node %s: no data from core, assuming occupied", n)
			occ[n] = true
		}
	}
	return occ
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
	occupancy := e.claimOccupancy(claim)

	plan, err := BuildConsumePlan(node, runtime, claim, quantity, occupancy, autoConfirm)
	if err != nil {
		return nil, err
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

	// Bug 3 guard: refuse to start a second swap on top of an in-flight one.
	// Edge-runtime-only — Core anomalies don't shut down the line. See
	// operator_guards.go.
	if plan.Dispatch != nil && plan.Dispatch.RequiresActiveSwapGuard {
		if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
			return nil, err
		}
	}

	// The demand episode is opened HERE — after the plan exists and before any
	// order does. That ordering is the whole reason expected_orders can be the
	// plan's own order count rather than a guess: the system's stated intent,
	// captured once, at the moment it is stated.
	//
	// Everything from here on belongs to this episode, including the primes and
	// both swap legs. Choreography is not demand.
	origin := e.openEpisodeForConsume(node, runtime, claim, plan, trigger)

	return e.applyConsumePlan(node, plan, origin)
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

	if plan.SimpleMove {
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, plan.Quantity, plan.SimpleSource, plan.SimpleDest, plan.AutoConfirm, origin)
		if err != nil {
			return nil, err
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
			e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
		}
		order, err = e.refreshOrderStation(order.ID)
		if err != nil {
			return nil, err
		}
		// Press-index empty-station primes: attributed to the head node
		// for ownership/audit, NOT tracked in runtime slots (those belong
		// to the head's serial-order machinery for swap cycles). Failure
		// of any single prime is logged and surfaced — the head order is
		// already created and we don't roll it back, but we do return the
		// error so the operator sees that priming was incomplete.
		var primes []*storeorders.Order
		for _, p := range plan.PrimePairedPositions {
			po, perr := e.orderMgr.CreateMoveOrder(&nodeID, plan.Quantity, p.Source, p.Dest, plan.AutoConfirm, origin)
			if perr != nil {
				return nil, fmt.Errorf("prime %s: %w", p.Dest, perr)
			}
			refreshed, perr := e.refreshOrderStation(po.ID)
			if perr != nil {
				return nil, perr
			}
			primes = append(primes, refreshed)
		}
		return &NodeOrderResult{Order: order, PrimeOrders: primes, ProcessNodeID: nodeID}, nil
	}

	dispatch := plan.Dispatch
	orderA, err := e.dispatchComplexLeg(nodeID, plan.Quantity, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, "", origin)
	if err != nil {
		return nil, err
	}

	var orderB *storeorders.Order
	if dispatch.StepsB != nil {
		orderB, err = e.dispatchComplexLeg(nodeID, plan.Quantity, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, orderA.UUID, origin)
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
	// nulled by handler_bin_picked_up before release fires; the sibling
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

// ReleaseNodeEmpty releases the active claim's bin as fully consumed
// (qty=1). Wrapper around ReleaseNodePartial for the common case where
// the operator finishes a bin without partial-quantity tracking.
//
// 2026-04-27 v2 direction Phase 3 #11: this surface (ReleaseNodeEmpty,
// ReleaseNodePartial, DeliverNewMaterialForChangeover) was reviewed for
// possible consolidation. All three have production callers via HTTP
// handlers (apiReleaseNodeEmpty, apiReleaseNodePartial,
// apiDeliverNewMaterialForChangeover at handlers_operator_stations.go) plus
// internal calls from the changeover flow (operator_node_changeover.go).
// No deletion warranted; surface is intentional. F' renamed two methods:
// ReleaseNodeIntoProduction → DeliverNewMaterialForChangeover (creates a
// new staged-deliver/restore order, not a /addBlocks release), and
// EmptyNodeForToolChange → EvacuateNode (the wizard's middle step,
// renamed away from "release" so it doesn't compete with the per-node
// Release modal that fires ReleaseOrderWithLineside on the evac order).
func (e *Engine) ReleaseNodeEmpty(nodeID int64) (*storeorders.Order, error) {
	return e.ReleaseNodePartial(nodeID, 1)
}

// ReleaseNodePartial releases the active claim's bin with the given
// quantity consumed. Used both for full releases (via ReleaseNodeEmpty
// with qty=1) and partial-quantity releases when the operator hands off
// a bin that wasn't fully consumed.
//
// Manifest sync threads runtime.RemainingUOPCached to Core. If the cache
// is stale or has been zeroed by a prior release-click on the slot, this
// silently wipes the bin's manifest on Core's claim. Use
// ReleaseNodeWithRemainingUOP when the operator has just declared the
// bin's actual remaining count and the cache shouldn't be trusted.
func (e *Engine) ReleaseNodePartial(nodeID int64, qty int64) (*storeorders.Order, error) {
	return e.releaseNodeInternal(nodeID, qty, nil)
}

// ReleaseNodeWithRemainingUOP is ReleaseNodePartial with an explicit
// remaining-UOP override that supersedes runtime.RemainingUOPCached for
// the manifest sync. Use this from operator paths that prompt for the
// count (Material page Release prompt) so a stale or zeroed cache
// doesn't silently wipe a partial bin at Core.
//
// remainingUOP is the bin's actual remaining count, NOT the order quantity.
// Pass 0 to declare the bin empty (manifest cleared); positive N preserves
// the manifest with that count.
func (e *Engine) ReleaseNodeWithRemainingUOP(nodeID int64, qty int64, remainingUOP int) (*storeorders.Order, error) {
	v := remainingUOP
	return e.releaseNodeInternal(nodeID, qty, &v)
}

func (e *Engine) releaseNodeInternal(nodeID int64, qty int64, overrideRemainingUOP *int) (*storeorders.Order, error) {
	return e.releaseNodeWithClaim(nodeID, qty, overrideRemainingUOP, nil)
}

// releaseNodeWithClaim is releaseNodeInternal with the acting claim supplied by
// the caller, used when the node cannot resolve one by name.
//
// The only such caller is the changeover evacuation of a fanned-out press position.
// A position owns changeover work but has no style_node_claims row under its own
// name, so the by-name resolution here returns nothing and the release refuses
// "no active claim for release" — the same refusal, from a different function,
// that stalls every other action on that position.
//
// fallback is used ONLY when the node resolves no claim of its own; a node that
// has one is unaffected, so no existing path changes. It is safe to write
// through: a position claim carries its PARENT's row id, unlike the loader synth
// whose ID is 0 (see domain.SynthesizePositionClaim and Loader.SynthClaim).
//
// ── THIS DOOR IS NOT LINE-PULL GUARDED, AND THAT IS DELIBERATE ────────────
//
// It is the Material page's Release / Release-partial path, and it takes a bin
// off a position and sends it to the outbound destination WITHOUT asking
// linePullsFrom — the question the other three doors ask (the trunk
// ReleaseOrderWithLineside, the changeover board's per-node click, and the
// plant-wide sweep). Physically it is the same act: a robot lifting the bin a
// position is holding.
//
// OWNER RULING 2026-08-28: it stays unguarded. "It's basically an admin release,
// not guarded like the line" — this button is the material-management door, not
// the button somebody at a running press presses, and the guard's own premise
// (the operator can see the aisle, so the bit never outranks him) is doubly true
// of the person driving this page.
//
// SO THE RULING IS THE THING TO READ FIRST if you are about to wire this into an
// operator flow on a SEQUENTIAL press, where the whole choreography rests on the
// other position taking over before this one is cleared. Guarding it then is a
// change to what the owner decided, not a bug fix — and the guard belongs on the
// wait, not bolted onto a fourth door, which is the lesson the trunk records.
func (e *Engine) releaseNodeWithClaim(nodeID int64, qty int64, overrideRemainingUOP *int, fallback *processes.NodeClaim) (*storeorders.Order, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	if qty < 1 {
		return nil, fmt.Errorf("qty must be at least 1")
	}
	if claim == nil {
		claim = fallback
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim for release", node.Name)
	}
	if claim.OutboundDestination == "" {
		return nil, fmt.Errorf("node %s has no outbound destination configured", node.Name)
	}
	// Manifest sync UOP — operator override (if provided) supersedes cache.
	// The override path is the safe one for the Material page Release flow
	// where the operator has declared the bin's actual count via prompt;
	// the cache fallback is the legacy path used by code paths that don't
	// expose a count input.
	var remainingUOP *int
	if overrideRemainingUOP != nil {
		v := *overrideRemainingUOP
		remainingUOP = &v
	} else if runtime.RemainingUOPCached >= 0 {
		v := runtime.RemainingUOPCached
		remainingUOP = &v
	}
	// ── THE RELEASE IS PART OF THE CIRCLE, NOT A SEPARATE ERRAND ─────────────
	//
	// §R.87: an episode represents its process's FULL circular material handling.
	// Sending the spent bin to its outbound destination is the return leg of the
	// very circle the inbound delivery opened — so it belongs to that cell's
	// episode, and it had been reaching Core carrying nothing.
	//
	// JOIN-ONLY, never mint, for the reason the sequential backfill states: a
	// release is the plant finishing something that was already asked for, not a
	// new ask. If no episode is open the origin is left unstated and Core
	// classifies, which is exactly what happened here before — so this is strictly
	// more attribution and never a guess.
	order, err := e.orderMgr.CreateMoveOrderWithUOP(&nodeID, qty, claim.CoreNodeName, claim.OutboundDestination, remainingUOP, claim.AutoConfirm || e.cfg.Web.AutoConfirm,
		e.cellEpisodeOrigin(node, claim))
	if err != nil {
		return nil, err
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, runtime.StagedOrderID); err != nil {
		e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
	}
	refreshed, err := e.db.GetOrder(order.ID)
	if err != nil {
		e.logFn("station: re-read order %d after runtime update: %v", order.ID, err)
		return order, nil
	}
	order = refreshed

	// L1 (consume-side empty-in) used to fire here too, mirroring the
	// hook in operator_release.go. Removed when Core's wiring_kanban
	// DemandSignal pipeline became the single trigger source for L1 —
	// then removed again when that pipeline itself was deleted
	// (2026-08). Consume empties are operator-driven now. See the
	// side-cycle comment block in operator_release.go.

	return order, nil
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
	if runtime.ActiveClaimID != nil {
		if claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID); err == nil && claim.SwapMode == protocol.SwapModeManualSwap {
			return true, ""
		}
	}

	for _, orderID := range []*int64{runtime.ActiveOrderID, runtime.StagedOrderID} {
		if orderID == nil {
			continue
		}
		order, err := e.db.GetOrder(*orderID)
		if err == nil && !orders.IsTerminal(order.Status) {
			if orderID == runtime.ActiveOrderID {
				return false, "active order in progress"
			}
			return false, "staged order in progress"
		}
	}
	return true, ""
}

// ReleaseStagedOrders releases both orders of a two-robot swap in a single
// server-side step. Order B (StagedOrderID — the removal robot) is released
// first so it leaves the production node before Order A (ActiveOrderID — the
// delivery robot) arrives from inbound staging.
//
// The claim's SwapMode must be "two_robot" — the method refuses to operate
// on any other mode even if both runtime order slots are populated. The UI
// already gates the button on swap_ready (which checks the claim mode), but
// this is defense-in-depth for direct API callers.
//
// Idempotency: if either tracked order has already moved past "staged" (e.g.
// a concurrent status update already advanced it to in_transit), that leg is
// treated as success so the button behaves predictably under races.
//
// Failure handling is fail-closed: if B's release fails, A is never released.
// If A fails after B succeeded, the error is returned — Order A will remain
// staged and the operator can retry via the standard per-order release, which
// the UI re-renders automatically once swap_ready goes false.
//
// Disposition routing: Order B (evacuation, StagedOrderID slot) gets the
// operator's full disposition — capture, UOP sync, audit-via-CalledBy.
// Order A (supply, ActiveOrderID slot) gets the zero-value
// ReleaseDisposition{} (Mode == "" → nil remainingUOP at Core, no manifest
// action) so we don't re-run capture and don't accidentally clear Order A's
// freshly-loaded supply bin manifest.
//
// Per-leg ReleasableAtCore gating (hop A4-i, 2026-07-23). Each leg fires
// ONLY when Core would accept the release — i.e. the leg is staged or
// in_transit (orders.ReleasableAtCore). Order B (evac) is at "staged" (the
// UI gate ComputeSwapReady guarantees it) and releases. Order A (supply)
// may be anywhere in its choreography; if it has NOT yet reached staged
// (still queued/sourcing/dispatched/acknowledged) it is SKIPPED rather than
// force-flipped to in_transit. The pre-hop shape fanned out unconditionally
// on IsTerminal alone (releaseUnlessTerminal), which optimistically moved a
// not-yet-releasable Edge row to in_transit and then rolled it back when
// Core answered "invalid_state" — the persistent Edge/Core divergence that
// hid the RELEASE button on the Hopkinsville press-index hang (PLN_01/04,
// 2026-07-23). A leg skipped here is re-fired when it later reaches staged
// (hop A4-ii, handleSiblingReleaseRefire) because its sibling already went.
//
// This is the targeted revival of the pre-2026-04-27 auto-release-on-staged
// coordination: the operator's single click still expresses "go" for the
// whole pair, but a leg Core cannot yet accept is deferred, not desynced.
// See shingo_todo.md and the 2026-04-27 retrospective for the interim
// fan-out-regardless design this supersedes.
// ── THE ORDER INSIDE THIS FUNCTION IS PART OF ITS CONTRACT ────────────────
//
// EVERY GATE AND VALIDATION FIRST — anything that can refuse — AND ONLY THEN
// THE SIDE EFFECTS: manifests, count changes, state clears. The two halves are
// separated by a marked line below, and a refusal must be reachable without
// having changed anything.
//
// This is written down because it was got wrong in exactly the way that is
// hard to see: the produce paperwork sat 26 lines above the collision gate, so
// an ADVISORY "not yet, click again" had already shipped the departing bin's
// manifest and zeroed the press's count. Every gate here is advisory by
// design — the operator repeats the click — which is precisely why none of them
// may leave a trace.
//
// Read alongside FinalizeProduceNode and the consume release path, which are
// held to the same order by TestReleasePathsGateBeforeSideEffects.
func (e *Engine) ReleaseStagedOrders(nodeID int64, disp ReleaseDisposition) error {
	// A changeover node whose work is a SINGLE leg — a cleared position's
	// clear-and-refill, one order on one robot — is released through the
	// changeover path. It is not a swap pair and must not be judged as one; the
	// gates below would refuse it for having no sibling, or for having no claim
	// row of its own. See releaseSingleLegChangeoverNode.
	if handled, err := e.releaseSingleLegChangeoverNode(nodeID, disp); handled {
		return err
	}

	// ── GATES AND VALIDATION. Nothing below may mutate anything. ─────────
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return fmt.Errorf("get runtime for node %d: %w", nodeID, err)
	}
	if claim == nil {
		return fmt.Errorf("node %s: no active claim for release", node.Name)
	}
	// findActiveClaim resolves via (active_style_id, core_node_name) — works
	// even when runtime.active_claim_id hasn't been stamped yet (it only
	// gets set on order completion in wiring_completion). Press-index and
	// two_robot share the same R1+R2 release choreography, so both modes
	// are valid here.
	if !claim.SwapMode.IsTwoRobot() {
		return fmt.Errorf("node %s: release-staged requires a two-robot swap mode, got %q", node.Name, claim.SwapMode)
	}

	// Load the active changeover node task so ResolveSwapPair can fall
	// back to task.OldMaterialReleaseOrderID when both runtime pointers
	// are nil. The HMI's ComputeSwapReady predicate already keys on this
	// pointer (store/station_views.go); without the symmetric fallback
	// here, the RELEASE button renders but every click bounces with
	// "no tracked orders to release". Plant 2026-05-11 (SNF2 ALN_001)
	// hit this loop until release was unreachable.
	//
	// Task loading is best-effort: a missing task or absent changeover
	// just means the resolver falls through to the runtime-pointer path,
	// which is the pre-2026-05-12 behavior for non-changeover swaps.
	task := loadReleaseSwapNodeTask(e.db, node)

	// Resolve the swap pair via durable sibling pointer rather than the
	// volatile runtime slots. handler_bin_picked_up nulls runtime.ActiveOrderID
	// when the supply bin leaves the supermarket; pre-2026-05-04 the gate
	// failed any release attempt after that point. Now we accept any
	// non-nil runtime slot, follow the sibling pointer to find the other
	// half, and release both. releaseUnlessTerminal handles already-past-
	// staged orders gracefully so partial states don't block the click.
	evacOrderID, supplyOrderID, err := store.ResolveSwapPair(e.db, runtime, task)
	if err != nil {
		// LOG THE BOUNCE. This returns before any "[orders] release:" line and
		// the Edge logs no HTTP, so a release that fails here used to leave NO
		// trace anywhere — the debug log could not distinguish "the operator
		// never clicked" from "the operator clicked and the resolver refused".
		// That ambiguity is why the Springfield ALN_003 2026-07-31 incident
		// could not be closed from order data alone.
		var stagedPtr, activePtr *int64
		if runtime != nil {
			stagedPtr, activePtr = runtime.StagedOrderID, runtime.ActiveOrderID
		}
		e.logFn("release-staged REFUSED node=%s: %v (runtime staged=%s active=%s, task=%t) — operator clicked and got nothing",
			node.Name, err, orderIDStr(stagedPtr), orderIDStr(activePtr), task != nil)
		return fmt.Errorf("node %s: %w", node.Name, err)
	}
	// -- THE LABELS ARE POSITIONAL; THE DISPOSITION IS NOT ---------------
	//
	// ResolveSwapPair maps staged->evac and active->supply, which is a
	// two_robot assumption and is INVERTED for press-index (R1 clears the
	// press, R2 supplies it - the flip does not change that). The label chosen
	// here decides which leg carries the operator's disposition, and the
	// disposition sets remaining_uop: which bin's manifest Core clears.
	//
	// It does NOT wipe the wrong bin -- the steps-based supply-bin guard
	// downstream suppresses the sync on the real supply leg. It loses the
	// other half: the real EVAC gets the bare disposition, so the bin leaving
	// the press is released with remaining_uop=nil and its manifest is never
	// cleared. Latent while every live press-index claim is produce-role
	// (produce discards remaining_uop for both legs); live the day a consume
	// one exists. See classifySwapLegsBySteps for the full accounting,
	// including the produce-side trigger the inversion silently withholds.
	//
	// So re-derive from the legs' STEPS, using the same discriminator the Edge
	// classifier and Core's dispatch predicates use. ComputeSwapReady keeps
	// the positional resolver: it accepts EITHER staged leg for press-index,
	// so the inversion never reached the button.
	if evacOrderID != nil && supplyOrderID != nil {
		if e2, s2, ok := e.classifySwapLegsBySteps(claim.CoreNodeName, *evacOrderID, *supplyOrderID); ok {
			if e2 != *evacOrderID {
				e.logFn("release-staged node=%s: steps say the pair is inverted relative to the runtime slots - evac=%d supply=%d (slots said evac=%d supply=%d)",
					node.Name, e2, s2, *evacOrderID, *supplyOrderID)
			}
			evacOrderID, supplyOrderID = &e2, &s2
		}
	}
	e.logFn("release-staged node=%s resolved evac=%s supply=%s",
		node.Name, orderIDStr(evacOrderID), orderIDStr(supplyOrderID))

	// ── v1'S SAFETY: NEVER PLACE ONTO A PRESS THAT IS NOT CLEAR YET ──────
	//
	// The supply leg puts a bin ON the process node; the evac leg takes the
	// old one OFF. Releasing the placing leg while its sibling is still coming
	// is the two-bins-on-one-node collision, and it is reachable through the
	// operator's ordinary RELEASE click: ComputeSwapReady shows the button when
	// EITHER leg is staged, and the loop below happily released whichever leg
	// Core would accept.
	//
	// THIS IS WHERE v1 ENFORCES, and it is the whole reason swap_hold.go is
	// untouched. A dispatch-time hold on both legs is a permanent mutual
	// deadlock (SYNTH-round2, 5/5 reviewers); a refused RELEASE is a click the
	// operator repeats a minute later. Under the IndexRobotSupplies flip both
	// legs open with a wait and neither is self-sufficient, so dispatch fails
	// open by design and this is the only thing standing between the flip and
	// a collision.
	//
	// Scoped to press-index. two_robot's release ordering has been in
	// production unchanged for a long time and its supply leg parks at a
	// staging node rather than the press, so widening this would be a change
	// to a mode nobody is working on.
	if err := e.refusePlacingLegWhileSiblingPending(node, claim, evacOrderID, supplyOrderID); err != nil {
		return err
	}

	// ── FROM HERE ON, SIDE EFFECTS. Nothing above this line has changed
	// anything; nothing below it may refuse.
	//
	// The produce paperwork fires FIRST among them, before either release
	// envelope, so Core applies the manifest first (the outbox drains by id).
	// It used to fire above the gate, which meant an ADVISORY refusal —
	// "the other robot has not cleared the press yet, click again" — had
	// already shipped the departing bin's manifest, cleared active_bin_id and
	// zeroed remaining_uop_cached, starting the hold-and-replay window for a
	// bin still sitting on a press that was still making parts into it.
	// Nothing in the gate reads anything the paperwork produces, so the two
	// were only in that order by accident.
	//
	// Changeover-owned pairs are excluded — their manifests belong to the
	// changeover release dispositions, and this pair resolution can be
	// serving a changeover task's legs (the task fallback above).
	if task == nil {
		if err := e.produceIngestAtRelease(node, runtime, claim); err != nil {
			return err
		}
	}

	supplyDisp := ReleaseDisposition{CalledBy: disp.CalledBy}

	// The EVACUATION leg - full disposition. "evac"/"supply" here are the
	// step-derived roles above, not the A/B creation order they used to name:
	// under press-index the evac IS leg A. Gated per-leg on
	// ReleasableAtCore (hop A4-i): a leg still queued/sourcing/dispatched/
	// acknowledged is skipped, not force-flipped to in_transit.
	evacReleased := false
	if evacOrderID != nil {
		released, err := e.releaseIfReleasable(*evacOrderID, "evac", disp)
		if err != nil {
			return err
		}
		evacReleased = released
	}
	// The SUPPLY leg - zero disposition (preserve the supply bin's manifest).
	supplyReleased := false
	if supplyOrderID != nil {
		released, err := e.releaseIfReleasable(*supplyOrderID, "supply", supplyDisp)
		if err != nil {
			return err
		}
		supplyReleased = released
	}

	// hop A4-ii: if exactly one leg released and its sibling was deferred
	// (Core would refuse it right now), remember the deferred leg so it fires
	// when it later reaches staged — its sibling having already gone. The
	// operator's single click expressed "go" for the whole pair; deferring is
	// not dropping. Nothing is recorded when BOTH released (nothing to re-fire)
	// or NEITHER released (no sibling went — re-firing would auto-release with
	// no operator intent behind it).
	if evacReleased && !supplyReleased && supplyOrderID != nil {
		e.rememberDeferredSiblingRelease(*supplyOrderID, supplyDisp)
	}
	if supplyReleased && !evacReleased && evacOrderID != nil {
		e.rememberDeferredSiblingRelease(*evacOrderID, disp)
	}
	return nil
}

// SwapPairNotReadyError refuses a RELEASE that would drop a bin onto a press
// its sibling has not cleared yet.
//
// ADVISORY: nothing is broken and nothing needs fixing. The other robot is on
// its way, and the operator's only correct action is to click again once it
// arrives. Rendered red it reads as a fault to escalate; the Advisory() marker
// is what makes the station show it as a notice (round 2's PrimeInFlightError
// pattern, keyed on behaviour so the handler needed no change).
type SwapPairNotReadyError struct {
	NodeName     string
	SiblingState string
}

func (e *SwapPairNotReadyError) Error() string {
	return fmt.Sprintf("node %s: the other robot has not cleared the press yet (%s) — "+
		"release again once it is staged", e.NodeName, e.SiblingState)
}

// Advisory marks this as the system working rather than a fault.
func (e *SwapPairNotReadyError) Advisory() bool { return true }

// refusePlacingLegWhileSiblingPending is v1's collision guard. See the call
// site for why it lives at RELEASE and not at dispatch.
//
// A TERMINAL SIBLING IS NOT PENDING. If it already ran — or was cancelled, or
// was skipped because the press was found empty — nothing is coming to collide
// with, and refusing then would strand the other leg forever with no sibling
// that can ever stage. Same for a sibling that is itself releasable: both legs
// go on this click, in the safe order.
//
// BOTH POSITIONS, NOT JUST THE HEAD. The guard used to ask only "does this leg
// place a bin at CoreNodeName", which is the front position, and that is only half
// of an unflipped press-index swap:
//
//	R1  wait@front, pickup front, dropoff outbound, pickup inbound, dropoff BACKFILL
//	R2  wait@paired, pickup paired, dropoff FRONT [, pickup second, dropoff paired]
//
// R2 places at the front and R1 places at the backfill position — and it is R2 that
// lifts the on-deck carrier OFF that position. So releasing R1 while R2 was still
// queued sent a robot to set a bin down on a position nothing had cleared, and the
// front-position-only question could not see it. Under the IndexRobotSupplies flip
// R1 places nowhere on the press, which is why the flipped case was never the
// one at risk and why widening the question rather than adding a second guard
// is what keeps the two cases answered the same way.
//
// The positions come from the claim's own geometry, so a 3-position press is
// covered by the same walk.
func (e *Engine) refusePlacingLegWhileSiblingPending(
	node *processes.Node, claim *processes.NodeClaim, evacOrderID, supplyOrderID *int64,
) error {
	if claim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return nil
	}
	if supplyOrderID == nil || evacOrderID == nil {
		// One-legged: there is no sibling to wait for.
		return nil
	}
	positions := pressPositionNodes(claim)
	// TWO ARMS, AND THEY ARE NOT SYMMETRIC.
	//
	// The supply leg is the placing leg BY THE CALLER'S OWN CLASSIFICATION —
	// classifySwapLegsBySteps labelled it that because it sets a bin down on
	// the front position — so that arm needs no further evidence and asks for none.
	// It is the original guard, unchanged.
	//
	// The evac arm is the addition, and it must prove itself from the steps:
	// unflipped it also places, at the backfill position, and flipped it places
	// nowhere on the press. Requiring the evidence is what keeps the flipped
	// case releasable. A leg whose steps cannot be read simply does not earn a
	// refusal on this arm — which cannot re-open the original hole, because the
	// supply arm above never depended on steps in the first place.
	for i, arm := range [][2]int64{
		{*supplyOrderID, *evacOrderID},
		{*evacOrderID, *supplyOrderID},
	} {
		legID, siblingID := arm[0], arm[1]
		leg, err := e.db.GetOrder(legID)
		if err != nil {
			// REFUSE, do not wave through. This is a collision guard: a read it
			// cannot complete is a question it cannot answer, and the two
			// answers do not cost the same. A wrong refusal is an operator
			// clicking again in a minute; a wrong pass is two bins on one position.
			e.logFn("release-staged HELD node=%s: cannot read leg %d to check for a collision: %v",
				node.Name, legID, err)
			return &SwapPairNotReadyError{NodeName: node.Name, SiblingState: "unreadable"}
		}
		if !orders.ReleasableAtCore(leg.Status) {
			// Not going anywhere on this click anyway; the per-leg gate handles
			// it and the deferral remembers it.
			continue
		}
		sibling, err := e.db.GetOrder(siblingID)
		if err != nil {
			e.logFn("release-staged HELD node=%s: cannot read sibling %d to check for a collision: %v",
				node.Name, siblingID, err)
			return &SwapPairNotReadyError{NodeName: node.Name, SiblingState: "unreadable"}
		}
		if orders.IsTerminal(sibling.Status) || orders.ReleasableAtCore(sibling.Status) {
			continue
		}
		position := claim.CoreNodeName
		if i == 1 {
			if position = e.legPlacesAtAnyPosition(legID, positions); position == "" {
				continue // sets nothing down on the press — the flipped R1
			}
		}
		e.logFn("release-staged HELD node=%s: leg %d is staged and would place a bin at %s, "+
			"but leg %d is %q and has not cleared it",
			node.Name, leg.ID, position, sibling.ID, sibling.Status)
		return &SwapPairNotReadyError{NodeName: node.Name, SiblingState: string(sibling.Status)}
	}
	return nil
}

// pressPositionNodes lists the physical positions of a press-index cell, front
// first. Empty names are dropped, so a 2-position press yields two.
func pressPositionNodes(claim *processes.NodeClaim) []string {
	out := make([]string, 0, 3)
	for _, n := range []string{claim.CoreNodeName, claim.PairedCoreNode, claim.SecondPairedCoreNode} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// legPlacesAtAnyPosition returns the first position this order sets a bin down on, or
// "" if it sets one down on none of them.
//
// Steps are the only truth for a complex order — delivery_node is a display
// value on these. An unreadable or undecodable list answers "no position", which is
// deliberately NOT the fail-closed direction of the status reads in the caller:
// this only ever decides the EVAC arm, the arm that did not exist before, and
// the supply arm's guarantee never depended on steps. So a missing steps list
// costs exactly the coverage that was never there, rather than stranding a
// release the previous guard would have allowed.
func (e *Engine) legPlacesAtAnyPosition(orderID int64, positions []string) string {
	stepsJSON, err := e.db.GetOrderStepsJSON(orderID)
	if err != nil {
		return ""
	}
	steps, err := decodeSteps(stepsJSON)
	if err != nil {
		return ""
	}
	for _, position := range positions {
		if legPlacesBinAt(steps, position) {
			return position
		}
	}
	return ""
}

// rememberDeferredSiblingRelease records a two-robot leg whose consolidated
// RELEASE was skipped because Core would refuse it right now, so
// handleSiblingReleaseRefire can fire it once it reaches staged. Only records a
// GENUINELY deferred leg — non-terminal AND not-yet-releasable. A terminal leg
// has nothing to re-fire (and would leak the map entry, since it never reaches
// staged); an already-releasable leg was released on this same click.
func (e *Engine) rememberDeferredSiblingRelease(orderID int64, disp ReleaseDisposition) {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return
	}
	if orders.IsTerminal(order.Status) || orders.ReleasableAtCore(order.Status) {
		return
	}
	e.pendingSiblingReleaseMu.Lock()
	if e.pendingSiblingRelease == nil {
		e.pendingSiblingRelease = make(map[int64]ReleaseDisposition)
	}
	e.pendingSiblingRelease[orderID] = disp
	e.pendingSiblingReleaseMu.Unlock()
	e.logFn("two-robot release: leg %d (%s) deferred — sibling already released; will re-fire when it reaches staged",
		orderID, order.Status)
}

// loadReleaseSwapNodeTask fetches the active changeover node task for
// this node, used as the third-fallback evac pointer by
// store.ResolveSwapPair. Best-effort: no active changeover, no node
// task, or any DB error → return nil and let the resolver fall through
// to the runtime-pointer path.
func loadReleaseSwapNodeTask(db *store.DB, node *processes.Node) *processes.NodeTask {
	co, err := db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || co == nil {
		return nil
	}
	task, err := db.GetChangeoverNodeTaskByNode(co.ID, node.ID)
	if err != nil {
		return nil
	}
	return task
}

// releaseIfReleasable calls ReleaseOrderWithLineside on a non-terminal order
// ONLY when Core's own release precondition (orders.ReleasableAtCore) holds —
// i.e. the leg is staged or in_transit. It reports whether the release was
// actually queued, so a caller can count the skip rather than miscount it as
// a release.
//
// This is the single gate for BOTH release surfaces:
//   - the DEFERRED paths that fire from an event rather than an operator click
//     (HandleBinPickedUp's changeover supply auto-release), where nothing
//     upstream guarantees the target leg has reached staged; and
//   - the operator's consolidated two-robot release (ReleaseStagedOrders),
//     which since hop A4-i (2026-07-23) also gates per-leg here instead of
//     fanning out unconditionally on IsTerminal alone.
//
// Without this, a leg still at queued/sourcing/dispatched/acknowledged gets an
// OrderRelease envelope Core refuses with "invalid_state", while
// Manager.ReleaseOrderWithDisposition has already transitioned the Edge row to
// in_transit — a persistent Edge/Core divergence, the one that hid the RELEASE
// button on the Hopkinsville press-index hang. A leg skipped here re-fires when
// it later reaches staged (handleSiblingReleaseRefire), scoped to a pair whose
// sibling already released.
func (e *Engine) releaseIfReleasable(orderID int64, label string, disp ReleaseDisposition) (bool, error) {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return false, fmt.Errorf("get order %s (%d): %w", label, orderID, err)
	}
	if orders.IsTerminal(order.Status) {
		e.logFn("deferred release: order %s (%d) status=%q is terminal — skipping", label, orderID, order.Status)
		return false, nil
	}
	if !orders.ReleasableAtCore(order.Status) {
		e.logFn("deferred release: order %s (%d) status=%q is not releasable at Core (needs staged or in_transit) — skipping, will release when it stages",
			label, orderID, order.Status)
		return false, nil
	}
	if err := e.ReleaseOrderWithLineside(orderID, disp); err != nil {
		return false, fmt.Errorf("release order %s (%d): %w", label, orderID, err)
	}
	return true, nil
}

// (AbortNodeOrders removed 2026-07-28. Its only caller was
// StartProcessChangeover, which used it to cancel every in-flight order on the
// nodes a changeover was about to touch. On a press-index swap those orders are
// frequently carrying the empty carriers the changeover's own index legs must
// pick up, so cancelling them mid-delivery deadlocks the changeover it was
// meant to clear the way for. StartProcessChangeover now refuses and names the
// blocking order instead — see nodesWithOrdersInFlight.)

// orderIDStr renders a nullable order id for diagnostic logs: the number, or
// "nil". Exists so the release-staged trace can show WHICH pointers the
// resolver had — a bounced release is otherwise indistinguishable from a click
// that never happened (Springfield ALN_003, 2026-07-31).
func orderIDStr(id *int64) string {
	if id == nil {
		return "nil"
	}
	return strconv.FormatInt(*id, 10)
}
