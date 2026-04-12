package engine

import (
	"fmt"
	"log"

	"shingoedge/orders"
	"shingoedge/store"
)

type NodeOrderResult struct {
	CycleMode     string       `json:"cycle_mode"`
	Order         *store.Order `json:"order,omitempty"`
	OrderA        *store.Order `json:"order_a,omitempty"`
	OrderB        *store.Order `json:"order_b,omitempty"`
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

	return e.requestNodeFromClaim(node, runtime, claim, quantity)
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
func (e *Engine) requestNodeFromClaim(node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState, claim *store.StyleNodeClaim, quantity int64) (*NodeOrderResult, error) {
	nodeID := node.ID

	// If the node is not physically occupied, skip swap choreography and just deliver.
	// This handles cases where a bin was removed manually (e.g. sent to quality hold).
	if claim.SwapMode != "simple" && !e.nodeIsOccupied(claim.CoreNodeName) {
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		log.Printf("[request-material] node %s is empty (no bin), downgrading %s to simple delivery", node.Name, claim.SwapMode)
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, quantity, claim.InboundSource, claim.CoreNodeName)
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
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, quantity, "", steps) // "" = removal, no UOP reset
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
		stepsA, stepsB := BuildTwoRobotSwapSteps(claim)
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, quantity, claim.CoreNodeName, stepsA)
		if err != nil {
			return nil, err
		}
		orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, quantity, "", stepsB)
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
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, quantity, claim.InboundSource, claim.CoreNodeName)
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

func (e *Engine) ReleaseNodeEmpty(nodeID int64) (*store.Order, error) {
	return e.ReleaseNodePartial(nodeID, 1)
}

func (e *Engine) ReleaseNodePartial(nodeID int64, qty int64) (*store.Order, error) {
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
	order, err := e.orderMgr.CreateMoveOrderWithUOP(&nodeID, qty, claim.CoreNodeName, claim.OutboundDestination, remainingUOP)
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

