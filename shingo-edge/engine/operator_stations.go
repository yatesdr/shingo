package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

type NodeOrderResult struct {
	CycleMode protocol.SwapMode  `json:"cycle_mode"`
	Order     *storeorders.Order `json:"order,omitempty"`
	OrderA    *storeorders.Order `json:"order_a,omitempty"`
	OrderB    *storeorders.Order `json:"order_b,omitempty"`
	// PrimeOrders are additional simple deliveries emitted alongside Order
	// when a press-index empty-station downgrade prime-filled the paired
	// positions. Empty for non-press-index requests and for press-index
	// downgrades where the paired positions were already occupied.
	PrimeOrders   []*storeorders.Order `json:"prime_orders,omitempty"`
	ProcessNodeID int64                `json:"process_node_id"`
}

func (e *Engine) RequestNodeMaterial(nodeID int64, quantity int64) (*NodeOrderResult, error) {
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

	return e.requestNodeFromClaim(node, runtime, claim, quantity)
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
	bins, _ := e.coreClient.FetchNodeBins(names)
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
func (e *Engine) requestNodeFromClaim(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, quantity int64) (*NodeOrderResult, error) {
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

	return e.applyConsumePlan(node, plan)
}

// applyConsumePlan is the impure half of the consume-request pipeline:
// it issues the move order or planned complex order(s), records the
// runtime-orders linkage, and re-reads the resulting orders. Direction-
// specific glue around the shared SwapDispatch.
func (e *Engine) applyConsumePlan(node *processes.Node, plan *ConsumePlan) (*NodeOrderResult, error) {
	nodeID := node.ID

	if plan.SimpleMove {
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, plan.Quantity, plan.SimpleSource, plan.SimpleDest, plan.AutoConfirm)
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
			po, perr := e.orderMgr.CreateMoveOrder(&nodeID, plan.Quantity, p.Source, p.Dest, plan.AutoConfirm)
			if perr != nil {
				return nil, fmt.Errorf("prime %s: %w", p.Dest, perr)
			}
			refreshed, perr := e.refreshOrderStation(po.ID)
			if perr != nil {
				return nil, perr
			}
			primes = append(primes, refreshed)
		}
		return &NodeOrderResult{CycleMode: protocol.SwapModeSimple, Order: order, PrimeOrders: primes, ProcessNodeID: nodeID}, nil
	}

	dispatch := plan.Dispatch
	orderA, err := e.dispatchComplexLeg(nodeID, plan.Quantity, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, "")
	if err != nil {
		return nil, err
	}

	var orderB *storeorders.Order
	if dispatch.StepsB != nil {
		orderB, err = e.dispatchComplexLeg(nodeID, plan.Quantity, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, orderA.UUID)
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
		return &NodeOrderResult{CycleMode: dispatch.CycleMode, Order: orderA, ProcessNodeID: nodeID}, nil
	}
	return &NodeOrderResult{CycleMode: dispatch.CycleMode, OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
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
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	if qty < 1 {
		return nil, fmt.Errorf("qty must be at least 1")
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
	order, err := e.orderMgr.CreateMoveOrderWithUOP(&nodeID, qty, claim.CoreNodeName, claim.OutboundDestination, remainingUOP, claim.AutoConfirm || e.cfg.Web.AutoConfirm)
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
	// DemandSignal pipeline became the single trigger source for L1.
	// See the same explanation in operator_release.go's side-cycle
	// comment block.

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
	// press-index changeover never fans out, so its indexed-over seats own no
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
// Status tolerance: both releases fire unconditionally as long as the order
// is non-terminal. Order B is at "staged" (the UI gate computeSwapReady
// guarantees it). Order A may be anywhere in its choreography — staged,
// in_transit, even acknowledged — we don't care. Manager.ReleaseOrder
// sends the OrderRelease envelope to Core; Core's HandleOrderRelease
// dispatches the post-wait blocks to the fleet, which appends them. If the
// robot hasn't reached the (bare) wait yet, the blocks queue and the robot
// continues straight through. If it's already there, blocks dispatch
// immediately. Either way the operator's intent ("go") propagates to both
// legs without needing the auto-release-on-staged coordination dance.
//
// This shape replaces the pre-2026-04-27 design where each release required
// the order to be at "staged" and a separate handleAutoReleaseOnStaged hook
// had to fire when the late sibling arrived. That coordination layer was
// fragile (depended on Order A's bare wait reliably reaching staged via
// the seerrds adapter, which it does not always do) and accumulated a
// cluster of patches and predicates. See shingo_todo.md and the 2026-04-27
// retrospective for context.
func (e *Engine) ReleaseStagedOrders(nodeID int64, disp ReleaseDisposition) error {
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
		return fmt.Errorf("node %s: %w", node.Name, err)
	}

	// Fix D: the deferred produce paperwork fires HERE, before either release
	// envelope, so Core applies the manifest first (outbox drains by id).
	// Changeover-owned pairs are excluded — their manifests belong to the
	// changeover release dispositions, and this pair resolution can be
	// serving a changeover task's legs (the task fallback above).
	if task == nil {
		if err := e.produceIngestAtRelease(node, runtime, claim); err != nil {
			return err
		}
	}

	// Order B (evacuation) — full disposition.
	if evacOrderID != nil {
		if err := e.releaseUnlessTerminal(*evacOrderID, "B", disp); err != nil {
			return err
		}
	}
	// Order A (supply) — zero disposition (preserve supply bin manifest).
	if supplyOrderID != nil {
		if err := e.releaseUnlessTerminal(*supplyOrderID, "A", ReleaseDisposition{CalledBy: disp.CalledBy}); err != nil {
			return err
		}
	}
	return nil
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

// releaseUnlessTerminal calls ReleaseOrderWithLineside on any non-terminal
// order. Replaces the pre-2026-04-27 releaseIfStaged which only fired on
// orders currently at "staged" status. The new contract: as long as the
// order isn't already finished (confirmed / failed / cancelled), fan out
// the release. Manager.ReleaseOrder + Core's HandleOrderRelease handle the
// envelope mechanics regardless of where the order is in its lifecycle.
//
// See shingo_todo.md for the pre-dispatch edge case (order has no
// VendorOrderID yet — in practice doesn't happen because Robot B reaching
// staged means both orders have been dispatched, but worth a guard
// eventually).
func (e *Engine) releaseUnlessTerminal(orderID int64, label string, disp ReleaseDisposition) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order %s (%d): %w", label, orderID, err)
	}
	if orders.IsTerminal(order.Status) {
		e.logFn("two-robot release: order %s (%d) status=%q is terminal — skipping", label, orderID, order.Status)
		return nil
	}
	if err := e.ReleaseOrderWithLineside(orderID, disp); err != nil {
		return fmt.Errorf("release order %s (%d): %w", label, orderID, err)
	}
	return nil
}

// releaseIfReleasable is releaseUnlessTerminal plus Core's OWN release
// precondition (orders.ReleasableAtCore). It reports whether the release was
// actually queued, so a caller can count the skip rather than miscount it as
// a release.
//
// Use this on the DEFERRED release paths — the ones that fire from an event
// rather than an operator click, where nothing upstream guarantees the target
// leg has reached staged. releaseUnlessTerminal remains correct for the
// operator's consolidated two-robot release, which deliberately fans out to
// both legs regardless of where each is in its choreography.
//
// Without this, a supply leg still at queued/sourcing/dispatched/acknowledged
// gets an OrderRelease envelope Core refuses with "invalid_state", while
// Manager.ReleaseOrderWithDisposition has already transitioned the Edge row to
// in_transit — a persistent Edge/Core divergence. Reachable today via
// press-index self-sufficient shapes; it becomes the NORMAL path once
// pool-sourced supply legs can sit in a wait state.
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

// AbortNodeOrders cancels all non-terminal orders tracked in a node's
// runtime state and clears the runtime order references.
func (e *Engine) AbortNodeOrders(nodeID int64) {
	runtime, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || runtime == nil {
		return
	}
	for _, orderID := range []*int64{runtime.ActiveOrderID, runtime.StagedOrderID} {
		if orderID == nil {
			continue
		}
		order, err := e.db.GetOrder(*orderID)
		if err != nil || orders.IsTerminal(order.Status) {
			continue
		}
		if err := e.orderMgr.AbortOrder(order.ID); err != nil {
			log.Printf("abort node orders: order %s on node %d: %v", order.UUID, nodeID, err)
		}
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, nil, nil); err != nil {
		e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
	}
}
