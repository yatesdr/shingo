package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// changeoverNodeCtx bundles the data loaded by every node-changeover method.
// Built by loadChangeoverNodeCtx, which performs shared validation.
type changeoverNodeCtx struct {
	changeover  *processes.Changeover
	node        *processes.Node
	runtime     *processes.RuntimeState // always loaded; callers that need it can access it
	nodeTask    *processes.NodeTask
	stationTask *processes.StationTask // may be nil (node not assigned to a station)
}

// loadChangeoverNodeCtx loads and validates the common data needed by
// StageNodeChangeoverMaterial, EvacuateNode, and DeliverNewMaterialForChangeover.
// Each caller adds its own ensureNodeTaskCanRequestOrder check after this returns.
func (e *Engine) loadChangeoverNodeCtx(processID, nodeID int64) (*changeoverNodeCtx, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	node, runtime, _, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}
	if node.ProcessID != processID {
		return nil, fmt.Errorf("node does not belong to process")
	}
	stationTask, nodeTask, err := loadChangeoverNodeTask(e.db, changeover.ID, node)
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

func (e *Engine) StageNodeChangeoverMaterial(processID, nodeID int64) (*orders.Order, error) {
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
			order, err := e.orderMgr.CreateComplexOrder(&ctx.node.ID, 1, toClaim.InboundStaging, toClaim.CoreNodeName, steps)
			if err != nil {
				return nil, err
			}
			e.recordChangeoverOrder(ctx, true, &order.ID, nil, "staging_requested")
			return order, nil
		}
	}

	// Direct delivery if no staging configured
	retrieveEmpty := toClaim.Role == protocol.ClaimRoleProduce
	order, err := e.orderMgr.CreateRetrieveOrder(&ctx.node.ID, retrieveEmpty, 1, toClaim.CoreNodeName, "", "standard", toClaim.PayloadCode, e.cfg.Web.AutoConfirm, false)
	if err != nil {
		return nil, err
	}
	e.recordChangeoverOrder(ctx, true, &order.ID, nil, "staging_requested")
	return order, nil
}

func (e *Engine) EvacuateNode(processID, nodeID int64, partialQty int64) (*orders.Order, error) {
	ctx, err := e.loadChangeoverNodeCtx(processID, nodeID)
	if err != nil {
		return nil, err
	}
	if err := ensureNodeTaskCanRequestOrder(ctx.nodeTask.OldMaterialReleaseOrderID, "line clear", e.db); err != nil {
		return nil, err
	}

	// Use claim-based release
	if fromClaim := findActiveClaim(e.db, ctx.node); fromClaim != nil && fromClaim.OutboundStaging != "" {
		steps := BuildReleaseSteps(fromClaim)
		order, err := e.orderMgr.CreateComplexOrderWithAutoConfirm(&ctx.node.ID, 1, "", fromClaim.CoreNodeName, steps)
		if err != nil {
			return nil, err
		}
		e.recordChangeoverOrder(ctx, false, ctx.nodeTask.NextMaterialOrderID, &order.ID, "empty_requested")
		return order, nil
	}

	// Fallback: simple release via move order
	var order *orders.Order
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

func (e *Engine) DeliverNewMaterialForChangeover(processID, nodeID int64) (*orders.Order, error) {
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
	if toClaim.Role == protocol.ClaimRoleChangeover && toClaim.OutboundStaging != "" {
		steps := BuildRestoreSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&ctx.node.ID, 1, toClaim.CoreNodeName, toClaim.CoreNodeName, steps)
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
			order, err := e.orderMgr.CreateComplexOrder(&ctx.node.ID, 1, toClaim.CoreNodeName, toClaim.CoreNodeName, steps)
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

	// Drop check before claim lookup: a drop node has no to-claim by
	// design (the new style doesn't use this position), so
	// GetStyleNodeClaimByNode returns ErrNoRows and bails the whole
	// Switch / Complete-Station action. Stamp line_cleared (terminal for
	// drop, per IsNodeTaskStateTerminal) without touching runtime, then
	// run the station rollup so an all-drop station still completes.
	changeover, coErr := e.db.GetActiveProcessChangeover(processID)
	if coErr == nil {
		nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
		if err == nil && nodeTask.Situation == "drop" {
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "line_cleared"); err != nil {
				log.Printf("switch_node: update drop task state for task %d: %v", nodeTask.ID, err)
			}
			e.rollupStationTaskAfterNodeSwitch(changeover.ID, node)
			if err := e.tryCompleteProcessChangeover(processID); err != nil {
				log.Printf("switch_node: complete changeover check for process %d after dropping node %d: %v", processID, nodeID, err)
			}
			return nil
		}
	}

	claim, err := e.db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName)
	if err != nil {
		return fmt.Errorf("target style claim not found for node")
	}
	claimID := claim.ID

	// Lineside phase 5: skip the UOP reset when the release-click path
	// already pointed runtime at the target claim. Re-resetting here
	// would clobber any counter drift accumulated while the bots were
	// heading home — exactly the "post-swap confirm" behaviour we're
	// removing. Still update runtime (and state transition below) when
	// the runtime hasn't been advanced yet, so legacy / safety-net
	// paths continue to work.
	runtime, runtimeErr := e.db.EnsureProcessNodeRuntime(nodeID)
	needsUOPReset := runtimeErr != nil || runtime == nil ||
		runtime.ActiveClaimID == nil || *runtime.ActiveClaimID != claimID
	if needsUOPReset {
		uop := claim.UOPCapacity
		if err := e.db.SetProcessNodeRuntime(nodeID, &claimID, uop); err != nil {
			return err
		}
	}

	if coErr == nil {
		nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
		if err == nil {
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "switched"); err != nil {
				log.Printf("switch_node: update node task state for task %d: %v", nodeTask.ID, err)
			}
			e.rollupStationTaskAfterNodeSwitch(changeover.ID, node)
		}
		if err := e.tryCompleteProcessChangeover(processID); err != nil {
			log.Printf("switch_node: complete changeover check for process %d after switching node %d: %v", processID, nodeID, err)
		}
	}
	return nil
}

// rollupStationTaskAfterNodeSwitch updates the parent station task state
// after a per-node switch / drop. Called by both the non-drop "switched"
// path and the drop "line_cleared" early-return so the station tile
// reflects partial vs. all-nodes-terminal regardless of which leg
// finished the last node. No-op for nodes not assigned to a station.
func (e *Engine) rollupStationTaskAfterNodeSwitch(changeoverID int64, node *processes.Node) {
	if node.OperatorStationID == nil {
		return
	}
	stationTask, err := e.db.GetChangeoverStationTaskByStation(changeoverID, *node.OperatorStationID)
	if err != nil {
		return
	}
	stationNodeTasks, _ := e.db.ListChangeoverNodeTasksByStation(changeoverID, stationTask.OperatorStationID)
	allDone := true
	for _, snt := range stationNodeTasks {
		if !domain.IsNodeTaskStateTerminal(snt.State, snt.Situation) {
			allDone = false
			break
		}
	}
	newState := "in_progress"
	if allDone {
		newState = "switched"
	}
	if err := e.db.UpdateChangeoverStationTaskState(stationTask.ID, newState); err != nil {
		log.Printf("switch_node: update station task state (%s) for task %d: %v", newState, stationTask.ID, err)
	}
}

// SwitchOperatorStationToTarget switches every node assigned to a
// station. Per-node errors are logged and skipped rather than aborting
// the whole station — pre-fix, a single misconfigured node (most
// commonly a drop whose target style has no claim, before the drop
// branch was added above) stranded every other node behind it. The
// underlying recovery for a stuck node still happens via the per-node
// HMI, but the station-level "Complete Station" action stops being a
// hostage to one bad row.
func (e *Engine) SwitchOperatorStationToTarget(processID, stationID int64) error {
	nodes, err := e.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := e.SwitchNodeToTarget(processID, node.ID); err != nil {
			log.Printf("switch_station: node %d (process %d): %v — continuing", node.ID, processID, err)
		}
	}
	return nil
}
