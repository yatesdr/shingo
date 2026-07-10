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

	plan, err := BuildProducePlan(node, runtime, claim, time.Now())
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

	if err := e.dispatchProduceIngest(node, claim, plan); err != nil {
		return nil, err
	}

	// Produce always has a swap mode now (BuildProducePlan errors otherwise), so
	// Dispatch is always set.
	dispatch := plan.Dispatch
	orderA, err := e.dispatchComplexLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, "")
	if err != nil {
		return nil, err
	}

	var orderB *orders.Order
	if dispatch.StepsB != nil {
		// Removal/evac leg carries the supply leg's UUID so Core can pair
		// them at intake (before this leg's dispatch claims the line bin).
		orderB, err = e.dispatchComplexLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, orderA.UUID)
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

// dispatchProduceIngest stamps Core's bin manifest with the produced count.
// Produce is always manifest-only: the swap's complex order carries the bin, so
// a local ingest order would only be a phantom for the abort fan-out to cancel
// (the "not_found" bug). Fire-and-forget via QueueIngestManifest — no local
// order, no reply on success.
func (e *Engine) dispatchProduceIngest(node *processes.Node, claim *processes.NodeClaim, plan *ProducePlan) error {
	return e.orderMgr.QueueIngestManifest(
		claim.PayloadCode,
		"", // bin label resolved by core from node contents
		node.CoreNodeName,
		plan.Manifest[0].Quantity,
		plan.Manifest,
		plan.ProducedAtRFC3339,
	)
}

// dispatchComplexLeg issues a single complex order with the right auto-
// confirm wiring. Direction-agnostic — produce passes quantity=1 (the
// bin), consume passes the operator-requested quantity. processNodeName
// is the line node both legs of a swap belong to (= claim.CoreNodeName);
// threaded into ComplexOrderRequest.ProcessNode so Core picks the line
// bin for order.BinID.
func (e *Engine) dispatchComplexLeg(nodeID int64, quantity int64, steps []protocol.ComplexOrderStep, deliveryNode, processNodeName string, autoConfirm bool, siblingUUID string) (*orders.Order, error) {
	dn := deliveryNode
	if autoConfirm {
		dn = ""
	}
	return e.orderMgr.CreateComplexOrderSibling(&nodeID, quantity, dn, processNodeName, steps, autoConfirm, "", siblingUUID)
}

// resetProduceRuntime is the produce-side analog of consume's release
// click: the full bin is finalized and shipped, so the slot is "done" for
// counting. Under hold-and-replay we clear active_bin_id, which puts the
// tick path into hold mode — any parts produced before the next empty bin
// lands accumulate in pending_uop_delta and replay onto the new bin when
// its OrderDelivered seeds active_bin_id (count 0 for a fresh empty) +
// epoch. The active/staged order pointers stamp the dispatched legs.
//
// Errors are logged only — the order(s) already shipped, so failing
// here would leave the caller with no actionable recovery.
func (e *Engine) resetProduceRuntime(nodeID int64, runtime *processes.RuntimeState, activeID, stagedID *int64) {
	if e.inventoryDelta != nil {
		// Clear the active bin (slot is empty after finalize → ticks hold)
		// and zero the count (the next empty bin starts at 0). Preserves
		// the claim so the next delivery binds against it.
		var claimID *int64
		if runtime != nil {
			claimID = runtime.ActiveClaimID
		}
		if err := e.inventoryDelta.ClearActiveAndReset(nodeID, claimID); err != nil {
			log.Printf("produce: clear active bin for node %d: %v", nodeID, err)
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
