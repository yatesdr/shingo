package engine

import (
	"fmt"
	"log"

	"shingoedge/store"
)

// changeoverNodeCtx bundles the data loaded by every node-changeover method.
// Built by loadChangeoverNodeCtx, which performs shared validation.
type changeoverNodeCtx struct {
	changeover  *store.ProcessChangeover
	node        *store.ProcessNode
	runtime     *store.ProcessNodeRuntimeState // always loaded; callers that need it can access it
	nodeTask    *store.ChangeoverNodeTask
	stationTask *store.ChangeoverStationTask // may be nil (node not assigned to a station)
}

// loadChangeoverNodeCtx loads and validates the common data needed by
// StageNodeChangeoverMaterial, EmptyNodeForToolChange, and ReleaseNodeIntoProduction.
// Each caller adds its own ensureNodeTaskCanRequestOrder check after this returns.
func (e *Engine) loadChangeoverNodeCtx(processID, nodeID int64) (*changeoverNodeCtx, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	node, runtime, _, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if node.ProcessID != processID {
		return nil, fmt.Errorf("node does not belong to process")
	}
	stationTask, nodeTask, err := e.loadChangeoverNodeTask(changeover.ID, node)
	if err != nil {
		return nil, err
	}
	if isNodeTaskTerminal(nodeTask) {
		return nil, fmt.Errorf("node %s changeover task is already complete", node.Name)
	}
	return &changeoverNodeCtx{
		changeover:  changeover,
		node:        node,
		runtime:     runtime,
		nodeTask:    nodeTask,
		stationTask: stationTask,
	}, nil
}

// recordChangeoverOrder performs the bookkeeping that follows every order creation
// during a node changeover: link orders, update task state, update station task.
// Logs errors instead of silently discarding them (replaces _ = pattern).
// Set updateRuntime=true only for StageNodeChangeoverMaterial, which is the only
// caller that updates the node's runtime order tracking.
func (e *Engine) recordChangeoverOrder(
	ctx *changeoverNodeCtx,
	updateRuntime bool,
	nextOrderID, releaseOrderID *int64,
	newState string,
) {
	if updateRuntime {
		if err := e.db.UpdateProcessNodeRuntimeOrders(
			ctx.node.ID, ctx.runtime.ActiveOrderID, nextOrderID,
		); err != nil {
			log.Printf("changeover: update runtime orders for node %s: %v", ctx.node.Name, err)
		}
	}
	if err := e.db.LinkChangeoverNodeOrders(
		ctx.nodeTask.ID, nextOrderID, releaseOrderID,
	); err != nil {
		log.Printf("changeover: link orders for node %s: %v", ctx.node.Name, err)
	}
	if err := e.db.UpdateChangeoverNodeTaskState(ctx.nodeTask.ID, newState); err != nil {
		log.Printf("changeover: update node task state for node %s: %v", ctx.node.Name, err)
	}
	if ctx.stationTask != nil {
		if err := e.db.UpdateChangeoverStationTaskState(
			ctx.stationTask.ID, "in_progress",
		); err != nil {
			log.Printf("changeover: update station task state: %v", err)
		}
	}
}

func (e *Engine) StageNodeChangeoverMaterial(processID, nodeID int64) (*store.Order, error) {
	ctx, err := e.loadChangeoverNodeCtx(processID, nodeID)
	if err != nil {
		return nil, err
	}
	if err := ensureNodeTaskCanRequestOrder(ctx.nodeTask.NextMaterialOrderID, "staging", e.db); err != nil {
		return nil, err
	}

	// Look up the to-claim from the changeover's target style
	toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.changeover.ToStyleID, ctx.node.CoreNodeName)
	if err != nil {
		return nil, fmt.Errorf("no claim for target style on node %s", ctx.node.Name)
	}

	if toClaim.InboundStaging != "" {
		steps := BuildStageSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&ctx.node.ID, 1, toClaim.InboundStaging, steps)
			if err != nil {
				return nil, err
			}
			e.recordChangeoverOrder(ctx, true, &order.ID, nil, "staging_requested")
			return order, nil
		}
	}

	// Direct delivery if no staging configured
	retrieveEmpty := toClaim.Role == "produce"
	order, err := e.orderMgr.CreateRetrieveOrder(&ctx.node.ID, retrieveEmpty, 1, toClaim.CoreNodeName, "", "standard", toClaim.PayloadCode, e.cfg.Web.AutoConfirm)
	if err != nil {
		return nil, err
	}
	e.recordChangeoverOrder(ctx, true, &order.ID, nil, "staging_requested")
	return order, nil
}

func (e *Engine) EmptyNodeForToolChange(processID, nodeID int64, partialQty int64) (*store.Order, error) {
	ctx, err := e.loadChangeoverNodeCtx(processID, nodeID)
	if err != nil {
		return nil, err
	}
	if err := ensureNodeTaskCanRequestOrder(ctx.nodeTask.OldMaterialReleaseOrderID, "line clear", e.db); err != nil {
		return nil, err
	}

	// Use claim-based release
	if fromClaim := e.findActiveClaim(ctx.node); fromClaim != nil && fromClaim.OutboundStaging != "" {
		steps := BuildReleaseSteps(fromClaim)
		order, err := e.orderMgr.CreateComplexOrder(&ctx.node.ID, 1, "", steps)
		if err != nil {
			return nil, err
		}
		e.recordChangeoverOrder(ctx, false, ctx.nodeTask.NextMaterialOrderID, &order.ID, "empty_requested")
		return order, nil
	}

	// Fallback: simple release via move order
	var order *store.Order
	if partialQty > 0 {
		order, err = e.ReleaseNodePartial(nodeID, partialQty)
	} else {
		order, err = e.ReleaseNodeEmpty(nodeID)
	}
	if err != nil {
		return nil, err
	}
	e.recordChangeoverOrder(ctx, false, ctx.nodeTask.NextMaterialOrderID, &order.ID, "empty_requested")
	return order, nil
}

func (e *Engine) ReleaseNodeIntoProduction(processID, nodeID int64) (*store.Order, error) {
	ctx, err := e.loadChangeoverNodeCtx(processID, nodeID)
	if err != nil {
		return nil, err
	}
	if err := ensureNodeTaskCanRequestOrder(ctx.nodeTask.NextMaterialOrderID, "release", e.db); err != nil {
		return nil, err
	}

	// Use claim-based delivery — check if this is a restore (changeover-only) or new material
	toClaim, err := e.db.GetStyleNodeClaimByNode(ctx.changeover.ToStyleID, ctx.node.CoreNodeName)
	if err != nil {
		return nil, fmt.Errorf("no claim for target style on node %s", ctx.node.Name)
	}

	// Changeover-only nodes: restore from outbound staging (where material was evacuated to)
	if toClaim.Role == "changeover" && toClaim.OutboundStaging != "" {
		steps := BuildRestoreSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&ctx.node.ID, 1, toClaim.CoreNodeName, steps)
			if err != nil {
				return nil, err
			}
			e.recordChangeoverOrder(ctx, false, &order.ID, ctx.nodeTask.OldMaterialReleaseOrderID, "release_requested")
			return order, nil
		}
	}

	if toClaim.InboundStaging != "" {
		steps := BuildStagedDeliverSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&ctx.node.ID, 1, toClaim.CoreNodeName, steps)
			if err != nil {
				return nil, err
			}
			e.recordChangeoverOrder(ctx, false, &order.ID, ctx.nodeTask.OldMaterialReleaseOrderID, "release_requested")
			return order, nil
		}
	}

	// No staging — mark as released directly
	e.recordChangeoverOrder(ctx, false, ctx.nodeTask.NextMaterialOrderID, ctx.nodeTask.OldMaterialReleaseOrderID, "released")
	return nil, nil
}

func (e *Engine) SwitchNodeToTarget(processID, nodeID int64) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	if process.TargetStyleID == nil {
		return fmt.Errorf("process has no target style")
	}
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil {
		return err
	}
	if node.ProcessID != processID {
		return fmt.Errorf("node does not belong to process")
	}
	claim, err := e.db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName)
	if err != nil {
		return fmt.Errorf("target style claim not found for node")
	}
	claimID := claim.ID
	uop := claim.UOPCapacity
	if err := e.db.SetProcessNodeRuntime(nodeID, &claimID, uop); err != nil {
		return err
	}

	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err == nil {
		nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
		if err == nil {
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "switched"); err != nil {
				log.Printf("switch_node: update node task state for task %d: %v", nodeTask.ID, err)
			}

			// If this node belongs to an operator station, update that station task
			if node.OperatorStationID != nil {
				stationTask, stErr := e.db.GetChangeoverStationTaskByStation(changeover.ID, *node.OperatorStationID)
				if stErr == nil {
					stationNodeTasks, _ := e.db.ListChangeoverNodeTasksByStation(changeover.ID, stationTask.OperatorStationID)
					allDone := true
					for _, snt := range stationNodeTasks {
						if snt.State != "switched" && snt.State != "unchanged" && snt.State != "verified" {
							allDone = false
							break
						}
					}
					if allDone {
						if err := e.db.UpdateChangeoverStationTaskState(stationTask.ID, "switched"); err != nil {
							log.Printf("switch_node: update station task state (all done) for task %d: %v", stationTask.ID, err)
						}
					} else {
						if err := e.db.UpdateChangeoverStationTaskState(stationTask.ID, "in_progress"); err != nil {
							log.Printf("switch_node: update station task state (partial) for task %d: %v", stationTask.ID, err)
						}
					}
				}
			}
		}
		if err := e.tryCompleteProcessChangeover(processID); err != nil {
			log.Printf("switch_node: complete changeover check for process %d after switching node %d: %v", processID, nodeID, err)
		}
	}
	return nil
}

func (e *Engine) SwitchOperatorStationToTarget(processID, stationID int64) error {
	nodes, err := e.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := e.SwitchNodeToTarget(processID, node.ID); err != nil {
			return err
		}
	}
	return nil
}
