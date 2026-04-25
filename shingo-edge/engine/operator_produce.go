package engine

import (
	"fmt"
	"log"
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
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
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
	if err := e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0); err != nil {
		log.Printf("produce: set runtime for node %d: %v", nodeID, err)
		}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
		}

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
	if err := e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0); err != nil {
		log.Printf("produce: set runtime for node %d: %v", nodeID, err)
		}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, nil); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshedA, err := e.db.GetOrder(orderA.ID)
		if err != nil {
			log.Printf("produce: re-read order %d after runtime update: %v", orderA.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", orderA.ID, err)
		}
		orderA = refreshedA
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

	if err := e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0); err != nil {
		log.Printf("produce: set runtime for node %d: %v", nodeID, err)
		}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshed, err := e.db.GetOrder(order.ID)
		if err != nil {
			log.Printf("produce: re-read order %d after runtime update: %v", order.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", order.ID, err)
		}
		order = refreshed
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

	// Bug 3 guard: refuse to start a second swap on top of an in-flight one.
	// Runs BEFORE setProduceManifest so we don't burn an ingest order on a
	// node that's about to be rejected. Edge-runtime-only — Core anomalies
	// don't shut down the line.
	if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
		return nil, err
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

	if err := e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0); err != nil {
		log.Printf("produce: set runtime for node %d: %v", nodeID, err)
		}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, &orderB.ID); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
		}
		refreshedA, err := e.db.GetOrder(orderA.ID)
		if err != nil {
			log.Printf("produce: re-read order %d after runtime update: %v", orderA.ID, err)
			return nil, fmt.Errorf("re-read order %d: %w", orderA.ID, err)
		}
		orderA = refreshedA
	refreshedB, err := e.db.GetOrder(orderB.ID)
	if err != nil {
		log.Printf("produce: re-read order %d after runtime update: %v", orderB.ID, err)
		return nil, fmt.Errorf("re-read order %d: %w", orderB.ID, err)
	}
	orderB = refreshedB
	return &NodeOrderResult{CycleMode: "two_robot", OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}
