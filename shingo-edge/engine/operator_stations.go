package engine

import (
	"fmt"
	"log"

	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

type NodeOrderResult struct {
	CycleMode     string       `json:"cycle_mode"`
	Order         *storeorders.Order `json:"order,omitempty"`
	OrderA        *storeorders.Order `json:"order_a,omitempty"`
	OrderB        *storeorders.Order `json:"order_b,omitempty"`
	ProcessNodeID int64        `json:"process_node_id"`
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

	result, err := e.requestNodeFromClaim(node, runtime, claim, quantity)
	if err != nil || result == nil {
		return result, err
	}

	// Side-cycle (L1): if a manual_swap loader exists for this payload,
	// create a parallel "empty-in" order tracked at the loader so the
	// loader operator's UI surfaces the demand directly. Without this the
	// loader sees nothing while the line's request runs against the
	// supermarket. See SHINGO_TODO.md "Bin loader as active workflow
	// participant" for design context. L2 (filled-out to supermarket) is
	// created on the loader operator's CONFIRM — see wiring_completion.go.
	//
	// Consumer line nodes only: PayloadCode here is what the LINE wants.
	// The loader fills empties with this payload — a clean producer/consumer
	// pair. Producer nodes (the loaders themselves) skip this; their
	// finalize path doesn't go through RequestNodeMaterial.
	if claim.Role == "consume" {
		e.MaybeCreateLoaderEmptyIn(claim.PayloadCode)
	}

	return result, nil
}

// findActiveClaim looks up the style node claim for a process node based on
// the process's active style and the node's core_node_name.
// nodeIsOccupied checks Core's telemetry to see if a physical bin is at the node.
// Returns true if occupied OR if Core is unreachable (safe default — assume bin present).
func (e *Engine) nodeIsOccupied(coreNodeName string) bool {
	if !e.coreClient.Available() {
		log.Printf("[occupied-check] core API not configured, assuming occupied")
		return true
	}
	bins, _ := e.coreClient.FetchNodeBins([]string{coreNodeName})
	if len(bins) == 0 {
		log.Printf("[occupied-check] node %s: no data from core, assuming occupied", coreNodeName)
		return true
	}
	log.Printf("[occupied-check] node %s: occupied=%v bin_label=%q", coreNodeName, bins[0].Occupied, bins[0].BinLabel)
	return bins[0].Occupied
}

// requestNodeFromClaim constructs orders using style_node_claims routing.
// If the node is physically empty (no bin per Core telemetry), a simple move
// order is created regardless of swap mode — there is nothing to swap out.
func (e *Engine) requestNodeFromClaim(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, quantity int64) (*NodeOrderResult, error) {
	nodeID := node.ID

	// If the node is not physically occupied, skip swap choreography and just deliver.
	// This handles cases where a bin was removed manually (e.g. sent to quality hold).
	if claim.SwapMode != "simple" && !e.nodeIsOccupied(claim.CoreNodeName) {
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		log.Printf("[request-material] node %s is empty (no bin), downgrading %s to simple delivery", node.Name, claim.SwapMode)
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, quantity, claim.InboundSource, claim.CoreNodeName, claim.AutoConfirm || e.cfg.Web.AutoConfirm)
		if err != nil {
			return nil, err
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
			e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshed, err := e.db.GetOrder(order.ID)
		if err != nil {
			e.logFn("station: re-read order %d after runtime update: %v", order.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", order.ID, err)
		}
		order = refreshed
		return &NodeOrderResult{CycleMode: "simple", Order: order, ProcessNodeID: nodeID}, nil
	}

	switch claim.SwapMode {
	case "sequential":
		steps := BuildSequentialRemovalSteps(claim)
		orderA, err := e.orderMgr.CreateComplexOrderWithAutoConfirm(&nodeID, quantity, "", steps) // "" = removal, no UOP reset
		if err != nil {
			return nil, err
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, nil); err != nil {
			e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshedA, err := e.db.GetOrder(orderA.ID)
		if err != nil {
			e.logFn("station: re-read order %d after runtime update: %v", orderA.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", orderA.ID, err)
		}
		orderA = refreshedA
		return &NodeOrderResult{CycleMode: "sequential", Order: orderA, ProcessNodeID: nodeID}, nil

	case "two_robot":
		if claim.InboundStaging == "" {
			return nil, fmt.Errorf("node %s: two-robot swap requires inbound staging node", node.Name)
		}
		// Bug 3 guard: refuse to start a second swap on top of an in-flight one.
		// Edge-runtime-only — Core anomalies don't shut down the line. See
		// operator_guards.go.
		if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
			return nil, err
		}
		stepsA, stepsB := BuildTwoRobotSwapSteps(claim)
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, quantity, claim.CoreNodeName, stepsA)
		if err != nil {
			return nil, err
		}
		orderB, err := e.orderMgr.CreateComplexOrderWithAutoConfirm(&nodeID, quantity, "", stepsB)
		if err != nil {
			return nil, err
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, &orderB.ID); err != nil {
			e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshedA, err := e.db.GetOrder(orderA.ID)
		if err != nil {
			e.logFn("station: re-read order %d after runtime update: %v", orderA.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", orderA.ID, err)
		}
		orderA = refreshedA
		refreshedB, err := e.db.GetOrder(orderB.ID)
		if err != nil {
			e.logFn("station: re-read order %d after runtime update: %v", orderB.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", orderB.ID, err)
		}
		orderB = refreshedB
		return &NodeOrderResult{CycleMode: "two_robot", OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil

	case "single_robot":
		if claim.InboundStaging == "" || claim.OutboundStaging == "" {
			return nil, fmt.Errorf("node %s: single-robot swap requires inbound and outbound staging nodes", node.Name)
		}
		steps := BuildSingleSwapSteps(claim)
		order, err := e.orderMgr.CreateComplexOrder(&nodeID, quantity, claim.CoreNodeName, steps)
		if err != nil {
			return nil, err
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
			e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshed, err := e.db.GetOrder(order.ID)
		if err != nil {
			e.logFn("station: re-read order %d after runtime update: %v", order.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", order.ID, err)
		}
		order = refreshed
		return &NodeOrderResult{CycleMode: "single_robot", Order: order, ProcessNodeID: nodeID}, nil

	default: // "simple"
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, quantity, claim.InboundSource, claim.CoreNodeName, claim.AutoConfirm || e.cfg.Web.AutoConfirm)
		if err != nil {
			return nil, err
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
			e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshed, err := e.db.GetOrder(order.ID)
		if err != nil {
			e.logFn("station: re-read order %d after runtime update: %v", order.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", order.ID, err)
		}
		order = refreshed
		return &NodeOrderResult{CycleMode: "simple", Order: order, ProcessNodeID: nodeID}, nil
	}
}

// ReleaseNodeEmpty releases the active claim's bin as fully consumed
// (qty=1). Wrapper around ReleaseNodePartial for the common case where
// the operator finishes a bin without partial-quantity tracking.
//
// 2026-04-27 v2 direction Phase 3 #11: this surface (ReleaseNodeEmpty,
// ReleaseNodePartial, ReleaseNodeIntoProduction) was reviewed for
// possible consolidation. All three have production callers via HTTP
// handlers (apiReleaseNodeEmpty, apiReleaseNodePartial,
// apiReleaseNodeIntoProduction at handlers_operator_stations.go) plus
// internal calls from the changeover flow (operator_node_changeover.go).
// No deletion warranted; surface is intentional.
func (e *Engine) ReleaseNodeEmpty(nodeID int64) (*storeorders.Order, error) {
	return e.ReleaseNodePartial(nodeID, 1)
}

// ReleaseNodePartial releases the active claim's bin with the given
// quantity consumed. Used both for full releases (via ReleaseNodeEmpty
// with qty=1) and partial-quantity releases when the operator hands off
// a bin that wasn't fully consumed.
func (e *Engine) ReleaseNodePartial(nodeID int64, qty int64) (*storeorders.Order, error) {
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
	// Thread the current remaining UOP so Core can atomically sync/clear
	// the bin's manifest when it claims the bin for this move order.
	var remainingUOP *int
	if runtime.RemainingUOP >= 0 {
		v := runtime.RemainingUOP
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
	return order, nil
}

func (e *Engine) ConfirmNodeManifest(nodeID int64) error {
	// DEPRECATED: Manifest confirmation moved to ConfirmDelivery (order-level).
	// The operator HMI now calls /api/confirm-delivery/{orderID} directly.
	// This endpoint is kept for backward compatibility with older HMI clients.
	log.Printf("engine: ConfirmNodeManifest called for node %d — this endpoint is deprecated, use ConfirmDelivery", nodeID)
	return nil
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
	node, err := e.db.GetProcessNode(nodeID)
	if err == nil {
		if _, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
			return false, "changeover in progress"
		}
	}
	runtime, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || runtime == nil {
		return true, "" // no runtime state = available
	}

	// manual_swap nodes use a multi-order queue — skip the serial order constraint.
	if runtime.ActiveClaimID != nil {
		if claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID); err == nil && claim.SwapMode == "manual_swap" {
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
	runtime, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		return fmt.Errorf("get runtime for node %d: %w", nodeID, err)
	}
	if runtime == nil || runtime.ActiveOrderID == nil || runtime.StagedOrderID == nil {
		return fmt.Errorf("node %d: expected two tracked orders for two-robot release", nodeID)
	}
	if runtime.ActiveClaimID == nil {
		return fmt.Errorf("node %d: no active claim for two-robot release", nodeID)
	}
	claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil {
		return fmt.Errorf("node %d: load active claim: %w", nodeID, err)
	}
	if claim == nil || claim.SwapMode != "two_robot" {
		mode := "<nil>"
		if claim != nil {
			mode = claim.SwapMode
		}
		return fmt.Errorf("node %d: release-staged requires two_robot swap, got %q", nodeID, mode)
	}

	// Order B (evacuation) — full disposition.
	if err := e.releaseUnlessTerminal(*runtime.StagedOrderID, "B", disp); err != nil {
		return err
	}
	// Order A (supply) — zero disposition (preserve supply bin manifest).
	if err := e.releaseUnlessTerminal(*runtime.ActiveOrderID, "A", ReleaseDisposition{CalledBy: disp.CalledBy}); err != nil {
		return err
	}
	return nil
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

