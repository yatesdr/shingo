package engine

import (
	"database/sql"
	"fmt"
	"log"

	"shingo/protocol"
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
	node, runtime, claim, err := e.loadActiveNode(nodeID)
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
func (e *Engine) findActiveClaim(node *store.ProcessNode) *store.StyleNodeClaim {
	process, err := e.db.GetProcess(node.ProcessID)
	if err != nil || process.ActiveStyleID == nil {
		return nil
	}
	claim, err := e.db.GetStyleNodeClaimByNode(*process.ActiveStyleID, node.CoreNodeName)
	if err != nil {
		return nil
	}
	return claim
}

// requestNodeFromClaim constructs orders using style_node_claims routing.
func (e *Engine) requestNodeFromClaim(node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState, claim *store.StyleNodeClaim, quantity int64) (*NodeOrderResult, error) {
	nodeID := node.ID

	switch claim.SwapMode {
	case "two_robot":
		if claim.InboundStaging == "" || claim.OutboundStaging == "" {
			return nil, fmt.Errorf("node %s: two-robot swap requires inbound and outbound staging nodes", node.Name)
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
		_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, &orderB.ID)
		orderA, _ = e.db.GetOrder(orderA.ID)
		orderB, _ = e.db.GetOrder(orderB.ID)
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
		_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)
		order, _ = e.db.GetOrder(order.ID)
		return &NodeOrderResult{CycleMode: "single_robot", Order: order, ProcessNodeID: nodeID}, nil

	default: // "simple"
		steps := BuildDeliverSteps(claim)
		order, err := e.orderMgr.CreateComplexOrder(&nodeID, quantity, claim.CoreNodeName, steps)
		if err != nil {
			return nil, err
		}
		_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)
		order, _ = e.db.GetOrder(order.ID)
		return &NodeOrderResult{CycleMode: "simple", Order: order, ProcessNodeID: nodeID}, nil
	}
}

func (e *Engine) ReleaseNodeEmpty(nodeID int64) (*store.Order, error) {
	return e.ReleaseNodePartial(nodeID, 1)
}

func (e *Engine) ReleaseNodePartial(nodeID int64, qty int64) (*store.Order, error) {
	node, runtime, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if qty < 1 {
		return nil, fmt.Errorf("qty must be at least 1")
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim for release", node.Name)
	}
	if claim.OutboundStaging == "" {
		return nil, fmt.Errorf("node %s has no outbound staging configured", node.Name)
	}
	steps := BuildReleaseSteps(claim)
	order, err := e.orderMgr.CreateComplexOrder(&nodeID, qty, "", steps)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, runtime.StagedOrderID)
	order, _ = e.db.GetOrder(order.ID)
	return order, nil
}

func (e *Engine) ConfirmNodeManifest(nodeID int64) error {
	// Manifest confirmation is now core's domain. This is a no-op on edge
	// but kept for API compatibility.
	return nil
}

// FinalizeProduceNode locks the current UOP count as the manifest and creates
// an ingest order to send the bin to storage. The node's UOP resets to 0 and
// is ready for a new empty bin.
func (e *Engine) FinalizeProduceNode(nodeID int64) (*store.Order, error) {
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

	// Create an ingest order with the current count as the manifest
	manifest := []protocol.IngestManifestItem{
		{
			PartNumber:  claim.PayloadCode,
			Quantity:    int64(runtime.RemainingUOP),
			Description: claim.PayloadCode,
		},
	}
	order, err := e.orderMgr.CreateIngestOrder(
		&nodeID,
		claim.PayloadCode,
		"", // bin label resolved by core from node contents
		node.CoreNodeName,
		int64(runtime.RemainingUOP),
		manifest,
		e.cfg.Web.AutoConfirm,
	)
	if err != nil {
		return nil, err
	}

	// Reset the node UOP to 0 — ready for next empty bin
	_ = e.db.SetProcessNodeRuntime(nodeID, runtime.ActiveClaimID, 0)
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)

	return order, nil
}

func (e *Engine) StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*store.ProcessChangeover, error) {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	if process.ActiveStyleID != nil && *process.ActiveStyleID == toStyleID {
		return nil, fmt.Errorf("process is already running style %d", toStyleID)
	}
	if _, err := e.db.GetActiveProcessChangeover(processID); err == nil {
		return nil, fmt.Errorf("process already has an active changeover")
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	style, err := e.db.GetStyle(toStyleID)
	if err != nil {
		return nil, err
	}
	if style.ProcessID != processID {
		return nil, fmt.Errorf("target style %d does not belong to process %d", toStyleID, processID)
	}

	// Pre-fetch all data before opening transaction (SQLite deadlock prevention)
	stations, err := e.db.ListOperatorStationsByProcess(processID)
	if err != nil {
		return nil, err
	}
	var fromClaims, toClaims []store.StyleNodeClaim
	if process.ActiveStyleID != nil {
		fromClaims, _ = e.db.ListStyleNodeClaims(*process.ActiveStyleID)
	}
	toClaims, _ = e.db.ListStyleNodeClaims(toStyleID)
	diffs := DiffStyleClaims(fromClaims, toClaims)
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil, err
	}

	stationIDs := make([]int64, len(stations))
	for i := range stations {
		stationIDs[i] = stations[i].ID
	}

	nodeTasks := make([]store.ChangeoverNodeTaskInput, len(diffs))
	for i, diff := range diffs {
		state := "unchanged"
		switch diff.Situation {
		case SituationSwap, SituationEvacuate, SituationDrop, SituationAdd:
			state = "swap_required"
		}
		var fromClaimID, toClaimID *int64
		if diff.FromClaim != nil {
			id := diff.FromClaim.ID
			fromClaimID = &id
		}
		if diff.ToClaim != nil {
			id := diff.ToClaim.ID
			toClaimID = &id
		}
		nodeTasks[i] = store.ChangeoverNodeTaskInput{
			ProcessID:    processID,
			CoreNodeName: diff.CoreNodeName,
			FromClaimID:  fromClaimID,
			ToClaimID:    toClaimID,
			Situation:    string(diff.Situation),
			State:        state,
		}
	}

	if _, err := e.db.CreateChangeover(processID, process.ActiveStyleID, toStyleID, calledBy, notes, stationIDs, nodeTasks, nodes); err != nil {
		return nil, err
	}

	return e.db.GetActiveProcessChangeover(processID)
}

func (e *Engine) CompleteProcessProductionCutover(processID int64) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	toStyleID := changeover.ToStyleID
	if err := e.db.SetActiveStyle(processID, &toStyleID); err != nil {
		return err
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}
	if err := e.SyncProcessCounter(processID); err != nil {
		return err
	}
	return e.db.UpdateProcessChangeoverState(changeover.ID, "completed")
}

func (e *Engine) CancelProcessChangeover(processID int64) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}

	// Abort all in-flight orders linked to this changeover's node tasks.
	// Core will handle safe resolution (convert loaded robots to store orders).
	nodeTasks, _ := e.db.ListChangeoverNodeTasks(changeover.ID)
	for _, task := range nodeTasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			order, err := e.db.GetOrder(*orderID)
			if err != nil {
				continue
			}
			if orders.IsTerminal(order.Status) {
				continue
			}
			if err := e.orderMgr.AbortOrder(order.ID); err != nil {
				log.Printf("changeover cancel: abort order %s: %v", order.UUID, err)
			}
		}
		// Mark node task as cancelled
		_ = e.db.UpdateChangeoverNodeTaskState(task.ID, "cancelled")
	}

	// Clear runtime order references for affected nodes
	for _, task := range nodeTasks {
		runtime, err := e.db.GetProcessNodeRuntime(task.ProcessNodeID)
		if err != nil || runtime == nil {
			continue
		}
		_ = e.db.UpdateProcessNodeRuntimeOrders(task.ProcessNodeID, nil, nil)
	}

	if err := e.db.UpdateProcessChangeoverState(changeover.ID, "cancelled"); err != nil {
		return err
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	return e.db.SetProcessProductionState(processID, "active_production")
}

func (e *Engine) StageNodeChangeoverMaterial(processID, nodeID int64) (*store.Order, error) {
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
	changeoverTask, nodeTask, err := e.loadChangeoverNodeTask(changeover.ID, node)
	if err != nil {
		return nil, err
	}
	if err := ensureNodeTaskCanRequestOrder(nodeTask.NextMaterialOrderID, "staging", e.db); err != nil {
		return nil, err
	}
	if isNodeTaskTerminal(nodeTask) {
		return nil, fmt.Errorf("node %s changeover task is already complete", node.Name)
	}

	// Look up the to-claim from the changeover's target style
	toClaim, err := e.db.GetStyleNodeClaimByNode(changeover.ToStyleID, node.CoreNodeName)
	if err != nil {
		return nil, fmt.Errorf("no claim for target style on node %s", node.Name)
	}

	if toClaim.InboundStaging != "" {
		steps := BuildStageSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&node.ID, 1, toClaim.InboundStaging, steps)
			if err != nil {
				return nil, err
			}
			_ = e.db.UpdateProcessNodeRuntimeOrders(node.ID, runtime.ActiveOrderID, &order.ID)
			_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nil)
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
			if changeoverTask != nil {
				_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress")
			}
			return order, nil
		}
	}

	// Direct delivery if no staging configured
	retrieveEmpty := toClaim.Role == "produce"
	order, err := e.orderMgr.CreateRetrieveOrder(&node.ID, retrieveEmpty, 1, toClaim.CoreNodeName, "", "standard", toClaim.PayloadCode, e.cfg.Web.AutoConfirm)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateProcessNodeRuntimeOrders(node.ID, runtime.ActiveOrderID, &order.ID)
	_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nil)
	_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
	if changeoverTask != nil {
		_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress")
	}
	return order, nil
}

func (e *Engine) EmptyNodeForToolChange(processID, nodeID int64, partialQty int64) (*store.Order, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	node, _, _, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if node.ProcessID != processID {
		return nil, fmt.Errorf("node does not belong to process")
	}
	changeoverTask, nodeTask, err := e.loadChangeoverNodeTask(changeover.ID, node)
	if err != nil {
		return nil, err
	}
	if isNodeTaskTerminal(nodeTask) {
		return nil, fmt.Errorf("node %s changeover task is already complete", node.Name)
	}
	if err := ensureNodeTaskCanRequestOrder(nodeTask.OldMaterialReleaseOrderID, "line clear", e.db); err != nil {
		return nil, err
	}

	// Use claim-based release
	if fromClaim := e.findActiveClaim(node); fromClaim != nil && fromClaim.OutboundStaging != "" {
		steps := BuildReleaseSteps(fromClaim)
		order, err := e.orderMgr.CreateComplexOrder(&node.ID, 1, "", steps)
		if err != nil {
			return nil, err
		}
		_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, nodeTask.NextMaterialOrderID, &order.ID)
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "empty_requested")
		if changeoverTask != nil {
			_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress")
		}
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
	_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, nodeTask.NextMaterialOrderID, &order.ID)
	_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "empty_requested")
	if changeoverTask != nil {
		_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress")
	}
	return order, nil
}

func (e *Engine) ReleaseNodeIntoProduction(processID, nodeID int64) (*store.Order, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	node, _, _, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	changeoverTask, nodeTask, err := e.loadChangeoverNodeTask(changeover.ID, node)
	if err != nil {
		return nil, err
	}
	if isNodeTaskTerminal(nodeTask) {
		return nil, fmt.Errorf("node %s changeover task is already complete", node.Name)
	}
	if err := ensureNodeTaskCanRequestOrder(nodeTask.NextMaterialOrderID, "release", e.db); err != nil {
		return nil, err
	}

	// Use claim-based delivery — check if this is a restore (changeover-only) or new material
	toClaim, err := e.db.GetStyleNodeClaimByNode(changeover.ToStyleID, node.CoreNodeName)
	if err != nil {
		return nil, fmt.Errorf("no claim for target style on node %s", node.Name)
	}

	// Changeover-only nodes: restore from outbound staging (where material was evacuated to)
	if toClaim.Role == "changeover" && toClaim.OutboundStaging != "" {
		steps := BuildRestoreSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&node.ID, 1, toClaim.CoreNodeName, steps)
			if err != nil {
				return nil, err
			}
			_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nodeTask.OldMaterialReleaseOrderID)
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "release_requested")
			if changeoverTask != nil {
				_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress")
			}
			return order, nil
		}
	}

	if toClaim.InboundStaging != "" {
		steps := BuildStagedDeliverSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&node.ID, 1, toClaim.CoreNodeName, steps)
			if err != nil {
				return nil, err
			}
			_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nodeTask.OldMaterialReleaseOrderID)
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "release_requested")
			if changeoverTask != nil {
				_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress")
			}
			return order, nil
		}
	}

	// No staging — mark as released directly
	_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, nodeTask.NextMaterialOrderID, nodeTask.OldMaterialReleaseOrderID)
	_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released")
	if changeoverTask != nil {
		_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress")
	}
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
	if uop == 0 {
		uop = 0 // explicit: empty node starts at 0
	}
	if err := e.db.SetProcessNodeRuntime(nodeID, &claimID, uop); err != nil {
		return err
	}

	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err == nil {
		nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
		if err == nil {
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "switched")

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
						_ = e.db.UpdateChangeoverStationTaskState(stationTask.ID, "switched")
					} else {
						_ = e.db.UpdateChangeoverStationTaskState(stationTask.ID, "in_progress")
					}
				}
			}
		}
		_ = e.tryCompleteProcessChangeover(processID)
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

func (e *Engine) tryCompleteProcessChangeover(processID int64) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil
	}
	if process.ActiveStyleID == nil || *process.ActiveStyleID != changeover.ToStyleID {
		return nil
	}
	tasks, err := e.db.ListChangeoverStationTasks(changeover.ID)
	if err != nil {
		return err
	}
	allNodeTasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		return err
	}
	allDone := true
	for _, nodeTask := range allNodeTasks {
		if nodeTask.State != "switched" && nodeTask.State != "unchanged" && nodeTask.State != "verified" && nodeTask.State != "released" {
			allDone = false
			break
		}
	}
	if !allDone {
		return nil
	}
	for _, task := range tasks {
		_ = e.db.UpdateChangeoverStationTaskState(task.ID, "switched")
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}
	return e.db.UpdateProcessChangeoverState(changeover.ID, "completed")
}

func isNodeTaskTerminal(task *store.ChangeoverNodeTask) bool {
	return task.State == "switched" || task.State == "verified" || task.State == "unchanged"
}

func ensureNodeTaskCanRequestOrder(orderID *int64, action string, db *store.DB) error {
	if orderID == nil {
		return nil
	}
	order, err := db.GetOrder(*orderID)
	if err != nil {
		return fmt.Errorf("%s already requested and order lookup failed: %w", action, err)
	}
	if !orders.IsTerminal(order.Status) {
		return fmt.Errorf("%s already requested with active order %s", action, order.UUID)
	}
	return nil
}

func (e *Engine) loadChangeoverNodeTask(changeoverID int64, node *store.ProcessNode) (*store.ChangeoverStationTask, *store.ChangeoverNodeTask, error) {
	var changeoverTask *store.ChangeoverStationTask
	if node.OperatorStationID != nil {
		task, err := e.db.GetChangeoverStationTaskByStation(changeoverID, *node.OperatorStationID)
		if err != nil {
			return nil, nil, err
		}
		changeoverTask = task
	}
	nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeoverID, node.ID)
	if err != nil {
		return nil, nil, err
	}
	return changeoverTask, nodeTask, nil
}

// loadActiveNode returns the process node, its runtime state, and the active
// style node claim (if any). The claim replaces the old assignment lookup.
func (e *Engine) loadActiveNode(nodeID int64) (*store.ProcessNode, *store.ProcessNodeRuntimeState, *store.StyleNodeClaim, error) {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(nodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	claim := e.findActiveClaim(node)
	return node, runtime, claim, nil
}
