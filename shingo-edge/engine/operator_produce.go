package engine

import (
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// FinalizeProduceNode locks the current UOP count as the manifest and
// dispatches the appropriate order(s) to remove the filled bin and bring
// the next empty. Builds a ProducePlan (pure validation + dispatch shape)
// and then applies it. Swap-mode dispatch shape is shared with consume
// via SwapDispatch — the robot doesn't care whether the bin is filling
// or emptying, the choreography is the same.
func (e *Engine) FinalizeProduceNode(nodeID int64) (*NodeOrderResult, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}

	plan, err := BuildProducePlan(node, runtime, claim, e.cfg.Web.AutoConfirm, time.Now())
	if err != nil {
		return nil, err
	}

	// Bug 3 guard: refuse to start a second swap on top of an in-flight one.
	// Runs BEFORE setProduceManifest so we don't burn an ingest order on a
	// node that's about to be rejected. Edge-runtime-only — Core anomalies
	// don't shut down the line.
	if plan.Dispatch != nil && plan.Dispatch.RequiresActiveSwapGuard {
		if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
			return nil, err
		}
	}

	return e.applyProducePlan(node, runtime, claim, plan)
}

// applyProducePlan is the impure half of the produce-finalize pipeline:
// it manifests the filled bin, dispatches the planned complex orders,
// resets node UOP, and re-reads the resulting orders. Direction-specific
// glue around the shared SwapDispatch.
func (e *Engine) applyProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, plan *ProducePlan) (*NodeOrderResult, error) {
	nodeID := node.ID

	ingestOrder, err := e.dispatchProduceIngest(nodeID, node, claim, plan)
	if err != nil {
		return nil, err
	}

	if plan.SimpleOnly {
		// Simple mode: the ingest order is the operator's "active" order.
		e.resetProduceRuntime(nodeID, runtime, &ingestOrder.ID, nil)
		return &NodeOrderResult{CycleMode: protocol.SwapModeSimple, Order: ingestOrder, ProcessNodeID: nodeID}, nil
	}
	_ = ingestOrder // tracked at Core via the manifest; not the active order for complex modes

	dispatch := plan.Dispatch
	orderA, err := e.dispatchComplexLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA)
	if err != nil {
		return nil, err
	}

	var orderB *orders.Order
	if dispatch.StepsB != nil {
		orderB, err = e.dispatchComplexLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB)
		if err != nil {
			return nil, err
		}
	}

	var orderBID *int64
	if orderB != nil {
		orderBID = &orderB.ID
	}
	e.resetProduceRuntime(nodeID, runtime, &orderA.ID, orderBID)
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
		return &NodeOrderResult{CycleMode: dispatch.CycleMode, Order: orderA, ProcessNodeID: nodeID}, nil
	}
	return &NodeOrderResult{CycleMode: dispatch.CycleMode, OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}

// dispatchProduceIngest creates the ingest order from the plan's manifest.
// All produce modes (simple and complex) issue this so Core has the part
// count for the bin sitting at the process node.
func (e *Engine) dispatchProduceIngest(nodeID int64, node *processes.Node, claim *processes.NodeClaim, plan *ProducePlan) (*orders.Order, error) {
	return e.orderMgr.CreateIngestOrder(
		&nodeID,
		claim.PayloadCode,
		"", // bin label resolved by core from node contents
		node.CoreNodeName,
		plan.Manifest[0].Quantity,
		plan.Manifest,
		plan.AutoConfirmIngest,
		plan.ProducedAtRFC3339,
	)
}

// dispatchComplexLeg issues a single complex order with the right auto-
// confirm wiring. Direction-agnostic — produce passes quantity=1 (the
// bin), consume passes the operator-requested quantity. processNodeName
// is the line node both legs of a swap belong to (= claim.CoreNodeName);
// threaded into ComplexOrderRequest.ProcessNode so Core picks the line
// bin for order.BinID.
func (e *Engine) dispatchComplexLeg(nodeID int64, quantity int64, steps []protocol.ComplexOrderStep, deliveryNode, processNodeName string, autoConfirm bool) (*orders.Order, error) {
	if autoConfirm {
		return e.orderMgr.CreateComplexOrderWithAutoConfirm(&nodeID, quantity, "", processNodeName, steps)
	}
	return e.orderMgr.CreateComplexOrder(&nodeID, quantity, deliveryNode, processNodeName, steps)
}

// resetProduceRuntime is the produce-side analog of consume's release
// click: cache flips to 0 (the next empty bin's UOP is 0 by definition),
// cached_bin_id flips to nil (the empty bin's id isn't known yet — Core
// resolves it at the supermarket and we'll learn it at delivery), and
// the active/staged order pointers stamp the dispatched legs.
//
// PLC tick gate: with cached_bin_id=nil and active_bin_id still pointing
// at the previous filled bin (or nil after pickup), the gap detector
// keeps PLC ticks from incrementing the cache during the gap. Delivery
// of the new empty bin sets cached_bin_id=active_bin_id=binID and
// resumes increments.
//
// Errors are logged only — the order(s) already shipped, so failing
// here would leave the caller with no actionable recovery.
func (e *Engine) resetProduceRuntime(nodeID int64, runtime *processes.RuntimeState, activeID, stagedID *int64) {
	_ = runtime // kept for parity with consume call site shape; cache write doesn't need claim id
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.ClearCache(nodeID); err != nil {
			log.Printf("produce: set runtime cache for node %d: %v", nodeID, err)
		}
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, activeID, stagedID); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
	}
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
