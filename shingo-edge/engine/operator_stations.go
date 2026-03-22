package engine

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
)

type OpNodeOrderResult struct {
	CycleMode string       `json:"cycle_mode"`
	Order     *store.Order `json:"order,omitempty"`
	OrderA    *store.Order `json:"order_a,omitempty"`
	OrderB    *store.Order `json:"order_b,omitempty"`
	OpNodeID  int64        `json:"op_node_id"`
}

func (e *Engine) RequestOpNodeMaterial(opNodeID int64, quantity int64) (*OpNodeOrderResult, error) {
	node, runtime, assignment, err := e.loadActiveOpNode(opNodeID)
	if err != nil {
		return nil, err
	}
	if !node.AllowsReorder {
		return nil, fmt.Errorf("node %s does not allow reorder", node.Name)
	}
	if quantity < 1 {
		quantity = 1
	}

	switch assignment.CycleMode {
	case "", "simple":
		order, err := e.orderMgr.CreateRetrieveOrder(&opNodeID, assignment.RetrieveEmpty, quantity,
			node.DeliveryNode, node.StagingNode, "standard", assignment.PayloadCode, e.cfg.Web.AutoConfirm)
		if err != nil {
			return nil, err
		}
		_ = e.db.UpdateOpNodeRuntimeOrders(opNodeID, &order.ID, nil)
		_ = e.db.SetOpNodeRuntime(opNodeID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID,
			runtime.LoadedPayloadCode, "replenishing", runtime.RemainingUOP, runtime.ManifestStatus)
		order, _ = e.db.GetOrder(order.ID)
		return &OpNodeOrderResult{CycleMode: "simple", Order: order, OpNodeID: opNodeID}, nil
	case store.CycleModeSequential:
		return e.requestOpNodeSequential(node, runtime, assignment, quantity)
	case store.CycleModeSingleRobot:
		return e.requestOpNodeSingleRobot(node, runtime, assignment, quantity)
	case store.CycleModeTwoRobot:
		return e.requestOpNodeTwoRobot(node, runtime, assignment, quantity)
	default:
		return nil, fmt.Errorf("unsupported cycle mode %q", assignment.CycleMode)
	}
}

func (e *Engine) ReleaseOpNodeEmpty(opNodeID int64) (*store.Order, error) {
	node, runtime, _, err := e.loadActiveOpNode(opNodeID)
	if err != nil {
		return nil, err
	}
	if !node.AllowsEmptyRelease {
		return nil, fmt.Errorf("node %s does not allow empty release", node.Name)
	}
	order, err := e.orderMgr.CreateMoveOrder(&opNodeID, 1, node.DeliveryNode, node.OutgoingNode)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateOpNodeRuntimeOrders(opNodeID, &order.ID, runtime.StagedOrderID)
	order, _ = e.db.GetOrder(order.ID)
	return order, nil
}

func (e *Engine) ReleaseOpNodePartial(opNodeID int64, qty int64) (*store.Order, error) {
	node, runtime, _, err := e.loadActiveOpNode(opNodeID)
	if err != nil {
		return nil, err
	}
	if !node.AllowsPartialRelease {
		return nil, fmt.Errorf("node %s does not allow partial release", node.Name)
	}
	if qty < 1 {
		return nil, fmt.Errorf("qty must be at least 1")
	}
	order, err := e.orderMgr.CreateMoveOrder(&opNodeID, qty, node.DeliveryNode, node.OutgoingNode)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateOpNodeRuntimeOrders(opNodeID, &order.ID, runtime.StagedOrderID)
	order, _ = e.db.GetOrder(order.ID)
	return order, nil
}

func (e *Engine) ConfirmOpNodeManifest(opNodeID int64) error {
	node, _, assignment, err := e.loadActiveOpNode(opNodeID)
	if err != nil {
		return err
	}
	if !node.AllowsManifestConfirm {
		return fmt.Errorf("node %s does not allow manifest confirmation", node.Name)
	}
	if assignment != nil && !assignment.RequiresManifestConfirmation {
		return nil
	}
	return e.db.UpdateOpNodeManifestStatus(opNodeID, "confirmed")
}

func (e *Engine) StartProcessChangeoverV2(processID, toStyleID int64, calledBy, notes string) (*store.ProcessChangeover, error) {
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
	if style.LineID != processID {
		return nil, fmt.Errorf("target style %d does not belong to process %d", toStyleID, processID)
	}
	flow := store.SanitizeProcessFlow(process.ChangeoverFlow)
	firstPhase := flow[0].Kind
	tx, err := e.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, phase, called_by, notes)
		VALUES (?, ?, ?, 'active', ?, ?, ?)`, processID, process.ActiveStyleID, toStyleID, firstPhase, calledBy, notes)
	if err != nil {
		return nil, err
	}
	changeoverID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE processes SET target_job_style_id=? WHERE id=?`, toStyleID, processID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE processes SET production_state='changeover_active' WHERE id=?`, processID); err != nil {
		return nil, err
	}

	stations, err := e.db.ListOperatorStationsByProcess(processID)
	if err != nil {
		return nil, err
	}
	for _, station := range stations {
		res, err := tx.Exec(`INSERT INTO changeover_station_tasks (
			process_changeover_id, operator_station_id, state, current_phase, transition_mode, ready_for_local_change
		) VALUES (?, ?, ?, ?, ?, ?)`, changeoverID, station.ID, deriveStationTaskState("waiting", firstPhase), firstPhase, "rolling_local", shouldStationBeReadyForPhase(firstPhase))
		if err != nil {
			return nil, err
		}
		taskID, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		nodes, err := e.db.ListOpStationNodesByStation(station.ID)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			var fromAssignmentID *int64
			if process.ActiveStyleID != nil {
				if currentAssign, err := e.db.GetOpNodeAssignmentForStyle(node.ID, *process.ActiveStyleID); err == nil {
					fromAssignmentID = &currentAssign.ID
				}
			}
			var toAssignmentID *int64
			if nextAssign, err := e.db.GetOpNodeAssignmentForStyle(node.ID, toStyleID); err == nil {
				toAssignmentID = &nextAssign.ID
			}
			state := "unchanged"
			if toAssignmentID != nil || fromAssignmentID != nil {
				state = "swap_required"
			}
			if _, err := tx.Exec(`INSERT INTO changeover_node_tasks (
				changeover_station_task_id, op_node_id, from_assignment_id, to_assignment_id, state, old_material_release_required
			) VALUES (?, ?, ?, ?, ?, ?)`, taskID, node.ID, fromAssignmentID, toAssignmentID, state, fromAssignmentID != nil); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO op_node_runtime_states (op_node_id) VALUES (?)`, node.ID); err != nil {
				return nil, err
			}
			runtime, err := scanTxOpNodeRuntime(tx, node.ID)
			if err != nil {
				return nil, err
			}
			if toAssignmentID != nil {
				if _, err := tx.Exec(`UPDATE op_node_runtime_states SET
					effective_style_id=?, active_assignment_id=?, staged_assignment_id=?, loaded_payload_code=?,
					material_status=?, remaining_uop=?, manifest_status=?, updated_at=datetime('now')
					WHERE op_node_id=?`,
					runtime.EffectiveStyleID, runtime.ActiveAssignmentID, toAssignmentID, runtime.LoadedPayloadCode,
					runtime.MaterialStatus, runtime.RemainingUOP, runtime.ManifestStatus, node.ID); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return e.db.GetActiveProcessChangeover(processID)
}

func (e *Engine) AdvanceProcessChangeoverPhase(processID int64, phase string) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	if store.ProcessFlowIndex(process.ChangeoverFlow, phase) < 0 {
		return fmt.Errorf("invalid changeover phase %q", phase)
	}
	if phase == changeover.Phase {
		return nil
	}
	next := store.NextProcessFlowStep(process.ChangeoverFlow, changeover.Phase)
	if next == nil || next.Kind != phase {
		return fmt.Errorf("invalid phase transition from %s to %s", changeover.Phase, phase)
	}
	if changeover.Phase == "cutover" && (process.ActiveStyleID == nil || *process.ActiveStyleID != changeover.ToStyleID) {
		return fmt.Errorf("start new style production before advancing past cutover")
	}
	if err := e.validateChangeoverPhaseTransition(changeover.ID, phase); err != nil {
		return err
	}
	if err := e.db.UpdateProcessChangeoverPhase(changeover.ID, phase); err != nil {
		return err
	}
	if tasks, err := e.db.ListChangeoverStationTasks(changeover.ID); err == nil {
		for _, task := range tasks {
			_ = e.db.UpdateChangeoverStationTaskPhase(task.ID, phase)
			ready := shouldStationBeReadyForPhase(phase)
			_ = e.db.UpdateChangeoverStationTaskState(task.ID, deriveStationTaskState(task.State, phase), ready)
		}
	}
	switch phase {
	case "cutover":
		_ = e.db.SetProcessProductionState(processID, "awaiting_cutover")
	case "verify":
		if process.ActiveStyleID != nil && changeover.ToStyleID == *process.ActiveStyleID {
			_ = e.db.SetProcessProductionState(processID, "new_style_production")
		}
	default:
		_ = e.db.SetProcessProductionState(processID, "changeover_active")
	}
	return nil
}

func (e *Engine) CompleteProcessProductionCutover(processID int64) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	if changeover.Phase != "cutover" && changeover.Phase != "verify" {
		return fmt.Errorf("process is not in cutover")
	}
	toStyleID := changeover.ToStyleID
	if err := e.db.SetActiveStyle(processID, &toStyleID); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "new_style_production"); err != nil {
		return err
	}
	if err := e.SyncProcessCounterBinding(processID); err != nil {
		return err
	}
	return e.tryCompleteProcessChangeover(processID)
}

func (e *Engine) CancelProcessChangeoverV2(processID int64) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	if err := e.db.UpdateProcessChangeoverState(changeover.ID, "cancelled"); err != nil {
		return err
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	return e.db.SetProcessProductionState(processID, "active_production")
}

func (e *Engine) StageOpNodeChangeoverMaterial(processID, opNodeID int64) (*store.Order, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	if changeover.Phase != "runout" && changeover.Phase != "tool_change" {
		return nil, fmt.Errorf("changeover staging is only available during runout or tool change")
	}
	node, runtime, _, err := e.loadActiveOpNode(opNodeID)
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
	if !canNodeTaskEnterPhase(nodeTask, "tool_change") {
		return nil, fmt.Errorf("node %s is not ready for staging in phase %s", node.Name, changeover.Phase)
	}
	stagedAssignID := runtime.StagedAssignmentID
	if stagedAssignID == nil {
		return nil, fmt.Errorf("node has no staged assignment for the target style")
	}
	assign, err := e.db.GetOpNodeAssignment(*stagedAssignID)
	if err != nil {
		return nil, err
	}
	deliveryNode := node.StagingNode
	if deliveryNode == "" {
		deliveryNode = node.DeliveryNode
	}
	order, err := e.orderMgr.CreateRetrieveOrder(&node.ID, assign.RetrieveEmpty, 1, deliveryNode, node.StagingNode, "standard", assign.PayloadCode, e.cfg.Web.AutoConfirm)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateOpNodeRuntimeOrders(node.ID, runtime.ActiveOrderID, &order.ID)
	_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nil)
	_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
	_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress", true)
	return order, nil
}

func (e *Engine) EmptyOpNodeForToolChange(processID, opNodeID int64, partialQty int64) (*store.Order, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	if changeover.Phase != "tool_change" {
		return nil, fmt.Errorf("line empty is only available during tool change")
	}
	node, _, _, err := e.loadActiveOpNode(opNodeID)
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
	if !canNodeTaskEnterPhase(nodeTask, "release") {
		return nil, fmt.Errorf("node %s is not ready to clear for tool change", node.Name)
	}
	if err := ensureNodeTaskCanRequestOrder(nodeTask.OldMaterialReleaseOrderID, "line clear", e.db); err != nil {
		return nil, err
	}
	var order *store.Order
	if partialQty > 0 {
		order, err = e.ReleaseOpNodePartial(opNodeID, partialQty)
	} else {
		order, err = e.ReleaseOpNodeEmpty(opNodeID)
	}
	if err != nil {
		return nil, err
	}
	_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, nodeTask.NextMaterialOrderID, &order.ID)
	_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "empty_requested")
	_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress", true)
	return order, nil
}

func (e *Engine) ReleaseOpNodeIntoProduction(processID, opNodeID int64) (*store.Order, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}
	if changeover.Phase != "release" && changeover.Phase != "cutover" && changeover.Phase != "verify" {
		return nil, fmt.Errorf("release to production is only available during release, cutover, or verify")
	}
	node, runtime, _, err := e.loadActiveOpNode(opNodeID)
	if err != nil {
		return nil, err
	}
	if runtime.StagedAssignmentID == nil {
		return nil, fmt.Errorf("node has no staged assignment to release")
	}
	if node.StagingNode == "" && node.StagingNodeGroup == "" {
		return nil, fmt.Errorf("node %s has no configured staging pickup location", node.Name)
	}
	if node.DeliveryNode == "" {
		return nil, fmt.Errorf("node %s has no configured delivery node", node.Name)
	}
	changeoverTask, nodeTask, err := e.loadChangeoverNodeTask(changeover.ID, node)
	if err != nil {
		return nil, err
	}
	if !canNodeTaskEnterPhase(nodeTask, "verify") {
		return nil, fmt.Errorf("node %s is not ready to release into production", node.Name)
	}
	if err := ensureNodeTaskCanRequestOrder(nodeTask.NextMaterialOrderID, "release", e.db); err != nil {
		return nil, err
	}
	steps := []protocol.ComplexOrderStep{
		buildPickupStep(node.StagingNode, node.StagingNodeGroup),
		{Action: "dropoff", Node: node.DeliveryNode},
	}
	order, err := e.orderMgr.CreateComplexOrder(&node.ID, 1, node.DeliveryNode, steps)
	if err != nil {
		return nil, err
	}
	_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nodeTask.OldMaterialReleaseOrderID)
	_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "release_requested")
	_ = e.db.UpdateChangeoverStationTaskState(changeoverTask.ID, "in_progress", true)
	return order, nil
}

func (e *Engine) SwitchOpNodeToTarget(processID, opNodeID int64) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	if process.TargetStyleID == nil {
		return fmt.Errorf("process has no target style")
	}
	node, err := e.db.GetOpStationNode(opNodeID)
	if err != nil {
		return err
	}
	if node.ProcessID != processID {
		return fmt.Errorf("node does not belong to process")
	}
	assign, err := e.db.GetOpNodeAssignmentForStyle(opNodeID, *process.TargetStyleID)
	if err != nil {
		return fmt.Errorf("target style assignment not found for node")
	}
	assignID := assign.ID
	styleID := assign.StyleID
	status := "active"
	if assign.UOPCapacity == 0 {
		status = "empty"
	}
	if err := e.db.SetOpNodeRuntime(opNodeID, &styleID, &assignID, nil, assign.PayloadCode, status, assign.UOPCapacity, "pending_confirmation"); err != nil {
		return err
	}

	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err == nil {
		tasks, _ := e.db.ListChangeoverStationTasks(changeover.ID)
		for _, stationTask := range tasks {
			nodeTask, err := e.db.GetChangeoverNodeTaskByNode(stationTask.ID, opNodeID)
			if err == nil {
				_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "switched")
				stationNodeTasks, _ := e.db.ListChangeoverNodeTasks(stationTask.ID)
				allDone := true
				for _, stationNodeTask := range stationNodeTasks {
					if stationNodeTask.State != "switched" && stationNodeTask.State != "unchanged" && stationNodeTask.State != "verified" {
						allDone = false
						break
					}
				}
				if allDone {
					_ = e.db.UpdateChangeoverStationTaskState(stationTask.ID, "switched", false)
				} else {
					_ = e.db.UpdateChangeoverStationTaskState(stationTask.ID, "in_progress", true)
				}
			}
		}
		_ = e.tryCompleteProcessChangeover(processID)
	}
	return nil
}

func (e *Engine) SwitchOperatorStationToTarget(processID, stationID int64) error {
	nodes, err := e.db.ListOpStationNodesByStation(stationID)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := e.SwitchOpNodeToTarget(processID, node.ID); err != nil {
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
	if store.NextProcessFlowStep(process.ChangeoverFlow, changeover.Phase) != nil {
		return nil
	}
	tasks, err := e.db.ListChangeoverStationTasks(changeover.ID)
	if err != nil {
		return err
	}
	allDone := true
	for _, task := range tasks {
		nodeTasks, _ := e.db.ListChangeoverNodeTasks(task.ID)
		for _, nodeTask := range nodeTasks {
			if nodeTask.State != "switched" && nodeTask.State != "unchanged" && nodeTask.State != "verified" && nodeTask.State != "released" {
				allDone = false
				break
			}
		}
		if !allDone {
			break
		}
		_ = e.db.UpdateChangeoverStationTaskState(task.ID, "switched", false)
	}
	if !allDone {
		return nil
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}
	return e.db.UpdateProcessChangeoverState(changeover.ID, "completed")
}

func scanTxOpNodeRuntime(tx *sql.Tx, opNodeID int64) (*store.OpNodeRuntimeState, error) {
	var r store.OpNodeRuntimeState
	var loadedAt, updatedAt sql.NullString
	err := tx.QueryRow(`SELECT id, op_node_id, effective_style_id, active_assignment_id, staged_assignment_id,
		loaded_payload_code, material_status, remaining_uop, manifest_status, active_order_id, staged_order_id,
		loaded_bin_label, loaded_at, updated_at
		FROM op_node_runtime_states WHERE op_node_id=?`, opNodeID).Scan(
		&r.ID, &r.OpNodeID, &r.EffectiveStyleID, &r.ActiveAssignmentID, &r.StagedAssignmentID,
		&r.LoadedPayloadCode, &r.MaterialStatus, &r.RemainingUOP, &r.ManifestStatus,
		&r.ActiveOrderID, &r.StagedOrderID, &r.LoadedBinLabel, &loadedAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	if loadedAt.Valid && loadedAt.String != "" {
		t, err := time.ParseInLocation("2006-01-02 15:04:05", loadedAt.String, time.UTC)
		if err == nil {
			r.LoadedAt = &t
		}
	}
	if updatedAt.Valid && updatedAt.String != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", updatedAt.String, time.UTC); err == nil {
			r.UpdatedAt = t
		}
	}
	return &r, nil
}

func shouldStationBeReadyForPhase(phase string) bool {
	switch phase {
	case "runout", "tool_change", "release", "cutover", "verify":
		return true
	default:
		return false
	}
}

func deriveStationTaskState(currentState, phase string) string {
	if currentState == "switched" || currentState == "verified" {
		return currentState
	}
	if phase == "runout" {
		return "waiting"
	}
	return "in_progress"
}

func canNodeTaskEnterPhase(task *store.ChangeoverNodeTask, targetPhase string) bool {
	switch targetPhase {
	case "tool_change":
		return task.State != "released" && task.State != "switched" && task.State != "verified"
	case "release":
		return task.State != "released" && task.State != "switched" && task.State != "verified"
	case "cutover":
		return task.State != "switched" && task.State != "verified"
	case "verify":
		return task.State != "released" && task.State != "switched" && task.State != "verified"
	default:
		return false
	}
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

func (e *Engine) loadChangeoverNodeTask(changeoverID int64, node *store.OpStationNode) (*store.ChangeoverStationTask, *store.ChangeoverNodeTask, error) {
	changeoverTask, err := e.db.GetChangeoverStationTaskByStation(changeoverID, node.OperatorStationID)
	if err != nil {
		return nil, nil, err
	}
	nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeoverTask.ID, node.ID)
	if err != nil {
		return nil, nil, err
	}
	return changeoverTask, nodeTask, nil
}

func (e *Engine) validateChangeoverPhaseTransition(changeoverID int64, nextPhase string) error {
	tasks, err := e.db.ListChangeoverStationTasks(changeoverID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		nodeTasks, err := e.db.ListChangeoverNodeTasks(task.ID)
		if err != nil {
			return err
		}
		for _, nodeTask := range nodeTasks {
			if !nodeTaskReadyForPhase(&nodeTask, nextPhase) {
				return fmt.Errorf("node %s is not ready for phase %s; current state is %s", nodeTask.NodeName, nextPhase, nodeTask.State)
			}
		}
	}
	return nil
}

func nodeTaskReadyForPhase(task *store.ChangeoverNodeTask, nextPhase string) bool {
	switch nextPhase {
	case "tool_change":
		if task.ToAssignmentID == nil {
			return true
		}
		return isNodeStateAtLeast(task.State, "staged")
	case "release":
		if task.OldMaterialReleaseRequired {
			return isNodeStateAtLeast(task.State, "line_cleared")
		}
		if task.ToAssignmentID == nil {
			return true
		}
		return isNodeStateAtLeast(task.State, "staged")
	case "verify":
		if task.ToAssignmentID != nil {
			return isNodeStateAtLeast(task.State, "released")
		}
		if task.OldMaterialReleaseRequired {
			return isNodeStateAtLeast(task.State, "line_cleared")
		}
		return true
	case "cutover":
		if task.ToAssignmentID != nil {
			return isNodeStateAtLeast(task.State, "released")
		}
		if task.OldMaterialReleaseRequired {
			return isNodeStateAtLeast(task.State, "line_cleared")
		}
		return true
	default:
		return false
	}
}

func isNodeStateAtLeast(state, min string) bool {
	order := map[string]int{
		"unchanged":         0,
		"swap_required":     0,
		"staging_requested": 1,
		"staged":            2,
		"empty_requested":   3,
		"line_cleared":      4,
		"release_requested": 5,
		"released":          6,
		"switched":          7,
		"verified":          8,
	}
	return order[state] >= order[min]
}

func (e *Engine) loadActiveOpNode(opNodeID int64) (*store.OpStationNode, *store.OpNodeRuntimeState, *store.OpNodeStyleAssignment, error) {
	node, err := e.db.GetOpStationNode(opNodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	runtime, err := e.db.EnsureOpNodeRuntime(opNodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	if runtime.ActiveAssignmentID != nil {
		assign, err := e.db.GetOpNodeAssignment(*runtime.ActiveAssignmentID)
		return node, runtime, assign, err
	}
	process, err := e.db.GetProcess(node.ProcessID)
	if err != nil {
		return nil, nil, nil, err
	}
	if process.ActiveStyleID == nil {
		return nil, nil, nil, fmt.Errorf("process has no active style")
	}
	assign, err := e.db.GetOpNodeAssignmentForStyle(opNodeID, *process.ActiveStyleID)
	if err != nil {
		return nil, nil, nil, err
	}
	assignID := assign.ID
	styleID := assign.StyleID
	if runtime.MaterialStatus == "" || runtime.MaterialStatus == "empty" {
		_ = e.db.SetOpNodeRuntime(opNodeID, &styleID, &assignID, runtime.StagedAssignmentID, assign.PayloadCode, "empty", 0, runtime.ManifestStatus)
		runtime, _ = e.db.GetOpNodeRuntime(opNodeID)
	}
	return node, runtime, assign, nil
}

func (e *Engine) requestOpNodeSingleRobot(node *store.OpStationNode, runtime *store.OpNodeRuntimeState, assignment *store.OpNodeStyleAssignment, quantity int64) (*OpNodeOrderResult, error) {
	steps := []protocol.ComplexOrderStep{
		buildPickupStep(node.FullPickupNode, node.FullPickupNodeGroup),
		buildDropoffStep(node.StagingNode, node.StagingNodeGroup),
		{Action: "dropoff", Node: node.DeliveryNode},
		{Action: "wait"},
		{Action: "pickup", Node: node.DeliveryNode},
		buildDropoffStep(node.SecondaryStagingNode, node.SecondaryNodeGroup),
		buildPickupStep(node.StagingNode, node.StagingNodeGroup),
		{Action: "dropoff", Node: node.DeliveryNode},
		buildPickupStep(node.SecondaryStagingNode, node.SecondaryNodeGroup),
		buildDropoffStep(node.OutgoingNode, node.OutgoingNodeGroup),
	}
	order, err := e.orderMgr.CreateComplexOrder(&node.ID, quantity, node.DeliveryNode, steps)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateOpNodeRuntimeOrders(node.ID, &order.ID, nil)
	_ = e.db.SetOpNodeRuntime(node.ID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID, assignment.PayloadCode, "replenishing", runtime.RemainingUOP, runtime.ManifestStatus)
	order, _ = e.db.GetOrder(order.ID)
	return &OpNodeOrderResult{CycleMode: store.CycleModeSingleRobot, Order: order, OpNodeID: node.ID}, nil
}

func (e *Engine) requestOpNodeTwoRobot(node *store.OpStationNode, runtime *store.OpNodeRuntimeState, assignment *store.OpNodeStyleAssignment, quantity int64) (*OpNodeOrderResult, error) {
	resupplySteps := []protocol.ComplexOrderStep{
		buildPickupStep(node.FullPickupNode, node.FullPickupNodeGroup),
		buildDropoffStep(node.StagingNode, node.StagingNodeGroup),
		{Action: "wait"},
		buildPickupStep(node.StagingNode, node.StagingNodeGroup),
		{Action: "dropoff", Node: node.DeliveryNode},
	}
	a, err := e.orderMgr.CreateComplexOrder(&node.ID, quantity, node.DeliveryNode, resupplySteps)
	if err != nil {
		return nil, err
	}
	removalSteps := []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: node.DeliveryNode},
		{Action: "wait"},
		{Action: "pickup", Node: node.DeliveryNode},
		buildDropoffStep(node.OutgoingNode, node.OutgoingNodeGroup),
	}
	b, err := e.orderMgr.CreateComplexOrder(&node.ID, quantity, "", removalSteps)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateOpNodeRuntimeOrders(node.ID, &a.ID, &b.ID)
	_ = e.db.SetOpNodeRuntime(node.ID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID, assignment.PayloadCode, "replenishing", runtime.RemainingUOP, runtime.ManifestStatus)
	a, _ = e.db.GetOrder(a.ID)
	b, _ = e.db.GetOrder(b.ID)
	return &OpNodeOrderResult{CycleMode: store.CycleModeTwoRobot, OrderA: a, OrderB: b, OpNodeID: node.ID}, nil
}

func (e *Engine) requestOpNodeSequential(node *store.OpStationNode, runtime *store.OpNodeRuntimeState, assignment *store.OpNodeStyleAssignment, quantity int64) (*OpNodeOrderResult, error) {
	steps := []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: node.DeliveryNode},
		{Action: "wait"},
		{Action: "pickup", Node: node.DeliveryNode},
		buildDropoffStep(node.OutgoingNode, node.OutgoingNodeGroup),
	}
	order, err := e.orderMgr.CreateComplexOrder(&node.ID, quantity, node.DeliveryNode, steps)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateOpNodeRuntimeOrders(node.ID, &order.ID, nil)
	_ = e.db.SetOpNodeRuntime(node.ID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID, assignment.PayloadCode, "replenishing", runtime.RemainingUOP, runtime.ManifestStatus)
	order, _ = e.db.GetOrder(order.ID)
	return &OpNodeOrderResult{CycleMode: store.CycleModeSequential, Order: order, OpNodeID: node.ID}, nil
}
