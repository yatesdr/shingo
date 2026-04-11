package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/store"
)

// FinalizeProduceNode locks the current UOP count as the manifest and dispatches
// the appropriate order(s) to remove the filled bin and bring the next empty.
// Swap mode dispatch mirrors consume's RequestNodeMaterial but in the produce
// direction. Simple mode creates a bare ingest order. Sequential/single_robot/
// two_robot modes first set the manifest via ingest metadata, then dispatch
// complex orders using the same step builders as consume.
func (e *Engine) FinalizeProduceNode(nodeID int64) (*NodeOrderResult, error) {
	node, runtime, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != "produce" {
		return nil, fmt.Errorf("node %s is not a produce node", node.Name)
	}
	if runtime.RemainingUOP <= 0 {
		return nil, fmt.Errorf("node %s has no parts to finalize", node.Name)
	}

	switch claim.SwapMode {
	case "sequential":
		return e.finalizeProduceSequential(node, runtime, claim)
	case "single_robot":
		return e.finalizeProduceSingleRobot(node, runtime, claim)
	case "two_robot":
		return e.finalizeProduceTwoRobot(node, runtime, claim)
	default: // "simple" or ""
		return e.finalizeProduceSimple(node, runtime, claim)
	}
}

// setProduceManifest creates an ingest order that sets the manifest on the bin
// at Core. Used by all produce swap modes before dispatching the complex order.
func (e *Engine) setProduceManifest(nodeID int64, node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState, claim *store.StyleNodeClaim) (*store.Order, error) {
	manifest := []protocol.IngestManifestItem{
		{
			PartNumber:  claim.PayloadCode,
			Quantity:    int64(runtime.RemainingUOP),
			Description: claim.PayloadCode,
		},
	}
	producedAt := time.Now().UTC().Format(time.RFC3339)
	return e.orderMgr.CreateIngestOrder(
		&nodeID,
		claim.PayloadCode,
		"", // bin label resolved by core from node contents
		node.CoreNodeName,
		int64(runtime.RemainingUOP),
		manifest,
		e.cfg.Web.AutoConfirm,
		producedAt,
	)
}

// finalizeProduceSimple handles simple mode: bare ingest order, no swap.
func (e *Engine) finalizeProduceSimple(node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState, claim *store.StyleNodeClaim) (*NodeOrderResult, error) {
	nodeID := node.ID
	order, err := e.setProduceManifest(nodeID, node, runtime, claim)
	if err != nil {
		return nil, err
	}

	// Reset the node UOP to 0 — ready for next empty bin
	_ = e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0)
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)

	return &NodeOrderResult{CycleMode: "simple", Order: order, ProcessNodeID: nodeID}, nil
}

// finalizeProduceSequential handles sequential mode: removal complex order
// (pre-position, wait, pickup filled, dropoff to outbound). Backfill (deliver
// next empty) is auto-created by handleSequentialBackfill when Order A goes
// in_transit — same wiring as consume.
func (e *Engine) finalizeProduceSequential(node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState, claim *store.StyleNodeClaim) (*NodeOrderResult, error) {
	nodeID := node.ID

	// Manifest the filled bin first
	ingestOrder, err := e.setProduceManifest(nodeID, node, runtime, claim)
	if err != nil {
		return nil, err
	}
	_ = ingestOrder // ingest order tracked for auditing but not as the "active" order

	// Build and dispatch the removal complex order
	steps := BuildSequentialRemovalSteps(claim)
	orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, "", steps)
	if err != nil {
		return nil, err
	}

	// Reset UOP and track the complex order
	_ = e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0)
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, nil)
	orderA, _ = e.db.GetOrder(orderA.ID)
	return &NodeOrderResult{CycleMode: "sequential", Order: orderA, ProcessNodeID: nodeID}, nil
}

// finalizeProduceSingleRobot handles single-robot swap: 10-step all-in-one
// complex order that removes the filled bin and delivers the next empty.
func (e *Engine) finalizeProduceSingleRobot(node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState, claim *store.StyleNodeClaim) (*NodeOrderResult, error) {
	nodeID := node.ID
	if claim.InboundStaging == "" || claim.OutboundStaging == "" {
		return nil, fmt.Errorf("node %s: single-robot swap requires inbound and outbound staging nodes", node.Name)
	}

	// Manifest the filled bin first
	ingestOrder, err := e.setProduceManifest(nodeID, node, runtime, claim)
	if err != nil {
		return nil, err
	}
	_ = ingestOrder

	steps := BuildSingleSwapSteps(claim)
	order, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.CoreNodeName, steps)
	if err != nil {
		return nil, err
	}

	_ = e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0)
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)
	order, _ = e.db.GetOrder(order.ID)
	return &NodeOrderResult{CycleMode: "single_robot", Order: order, ProcessNodeID: nodeID}, nil
}

// finalizeProduceTwoRobot handles two-robot coordinated swap: two complex orders
// dispatched simultaneously. Robot A fetches the next empty and stages it.
// Robot B removes the filled bin. Wiring coordinates the release sequence.
func (e *Engine) finalizeProduceTwoRobot(node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState, claim *store.StyleNodeClaim) (*NodeOrderResult, error) {
	nodeID := node.ID
	if claim.InboundStaging == "" {
		return nil, fmt.Errorf("node %s: two-robot swap requires inbound staging node", node.Name)
	}

	// Manifest the filled bin first
	ingestOrder, err := e.setProduceManifest(nodeID, node, runtime, claim)
	if err != nil {
		return nil, err
	}
	_ = ingestOrder

	stepsA, stepsB := BuildTwoRobotSwapSteps(claim)
	orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.CoreNodeName, stepsA)
	if err != nil {
		return nil, err
	}
	orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, "", stepsB)
	if err != nil {
		return nil, err
	}

	_ = e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0)
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, &orderB.ID)
	orderA, _ = e.db.GetOrder(orderA.ID)
	orderB, _ = e.db.GetOrder(orderB.ID)
	return &NodeOrderResult{CycleMode: "two_robot", OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}
