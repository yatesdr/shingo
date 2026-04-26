package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// changeoverPlan holds all pre-computed data needed to start a changeover.
// Built by planChangeover (read-only), consumed by StartProcessChangeover (mutations).
type changeoverPlan struct {
	process    *processes.Process
	style      *processes.Style
	stations   []stations.Station
	stationIDs []int64
	diffs      []ChangeoverNodeDiff
	nodes      []processes.Node
	nodeTasks  []processes.NodeTaskInput
}

// planChangeover assembles all data needed for a changeover without writing anything.
// Returns an error if the changeover request is invalid (wrong style, already active, etc).
//
// Note: validation errors use changeover-specific messages ("process is already running
// style %d", etc). If this is later reused for a dry-run API, the error messages will
// still be appropriate — but callers should be aware they're changeover-flavored.
func (e *Engine) planChangeover(processID, toStyleID int64) (*changeoverPlan, error) {
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
	var fromClaims, toClaims []processes.NodeClaim
	if process.ActiveStyleID != nil {
		fromClaims, err = e.db.ListStyleNodeClaims(*process.ActiveStyleID)
		if err != nil {
			return nil, fmt.Errorf("list from-style claims: %w", err)
		}
	}
	toClaims, err = e.db.ListStyleNodeClaims(toStyleID)
	if err != nil {
		return nil, fmt.Errorf("list to-style claims: %w", err)
	}
	diffs := DiffStyleClaims(fromClaims, toClaims)
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil, err
	}

	stationIDs := make([]int64, len(stations))
	for i := range stations {
		stationIDs[i] = stations[i].ID
	}

	nodeTasks := make([]processes.NodeTaskInput, len(diffs))
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
		nodeTasks[i] = processes.NodeTaskInput{
			ProcessID:    processID,
			CoreNodeName: diff.CoreNodeName,
			FromClaimID:  fromClaimID,
			ToClaimID:    toClaimID,
			Situation:    string(diff.Situation),
			State:        state,
		}
	}

	return &changeoverPlan{
		process:    process,
		style:      style,
		stations:   stations,
		stationIDs: stationIDs,
		diffs:      diffs,
		nodes:      nodes,
		nodeTasks:  nodeTasks,
	}, nil
}

// Error handling policy: log and continue. Do not add early returns without understanding the caller contract. See 2567plandiscussion.md.
func (e *Engine) StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*processes.Changeover, error) {
	plan, err := e.planChangeover(processID, toStyleID)
	if err != nil {
		return nil, err
	}

	if _, err := e.changeoverService.Create(processID, plan.process.ActiveStyleID, toStyleID,
		calledBy, notes, plan.stationIDs, plan.nodeTasks, plan.nodes); err != nil {
		return nil, err
	}

	// Abort pre-existing orders on affected nodes (not unchanged ones).
	for _, diff := range plan.diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(plan.nodes, diff.CoreNodeName)
		if node != nil {
			e.AbortNodeOrders(node.ID)
		}
	}

	// Retrieve the changeover we just created so we can link node tasks.
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}

	// Create ALL robot orders up front with embedded wait steps.
	// Operator controls flow by releasing waits, not by triggering individual orders.
	for _, diff := range plan.diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(plan.nodes, diff.CoreNodeName)
		if node == nil {
			continue
		}
		nodeTask, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, node.ID)
		if err != nil {
			log.Printf("changeover: cannot find node task for %s: %v", diff.CoreNodeName, err)
			continue
		}
		if err := e.createChangeoverOrders(changeover, nodeTask, node, diff); err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): %v — operator must handle manually",
				diff.CoreNodeName, diff.Situation, err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error"); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
		}
	}

	return e.db.GetActiveProcessChangeover(processID)
}

// createChangeoverOrders creates the appropriate robot orders for a single node
// based on its changeover situation. For swap/evacuate nodes, two orders are
// created: Order A (staging to inbound staging) and Order B (complex swap/evacuate
// with wait steps). For add/drop situations, only one order is needed.
func (e *Engine) createChangeoverOrders(
	changeover *processes.Changeover,
	nodeTask *processes.NodeTask,
	node *processes.Node,
	diff ChangeoverNodeDiff,
) error {
	nodeID := node.ID

	switch diff.Situation {
	case SituationSwap:
		if diff.FromClaim == nil || diff.ToClaim == nil {
			return fmt.Errorf("swap requires both from and to claims")
		}
		if diff.ToClaim.InboundStaging == "" || diff.FromClaim.OutboundStaging == "" {
			// Missing staging config — fall back to simple staging order (manual flow)
			return e.createFallbackStagingOrder(changeover, nodeTask, node, diff.ToClaim)
		}

		// Keep-staged: the old style's staging area has a pre-staged bin that
		// must be cleared before new material can be staged there.
		if diff.FromClaim.KeepStaged {
			return e.createKeepStagedChangeoverOrders(nodeTask, node, diff)
		}

		// Order A: Robot A stages new material to inbound staging
		stageSteps := BuildStageSteps(diff.ToClaim)
		if stageSteps == nil {
			return fmt.Errorf("cannot build staging steps for node %s", node.Name)
		}
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, diff.ToClaim.InboundStaging, stageSteps)
		if err != nil {
			return fmt.Errorf("create staging order: %w", err)
		}
		// Order B: Robot B runs swap with 1 wait
		swapSteps := BuildSwapChangeoverSteps(diff.FromClaim, diff.ToClaim)
		orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, "", swapSteps)
		if err != nil {
			return fmt.Errorf("create swap order: %w", err)
		}
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested"); err != nil {
			log.Printf("changeover: update node task %d state to staging_requested: %v", nodeTask.ID, err)
		}
		log.Printf("changeover: swap node %s — Order A=%d (staging), Order B=%d (swap w/ wait)", node.Name, orderA.ID, orderB.ID)

	case SituationEvacuate:
		if diff.FromClaim == nil || diff.ToClaim == nil {
			return fmt.Errorf("evacuate requires both from and to claims")
		}
		if diff.ToClaim.InboundStaging == "" || diff.FromClaim.OutboundStaging == "" {
			return e.createFallbackStagingOrder(changeover, nodeTask, node, diff.ToClaim)
		}

		// Keep-staged evacuate: same as keep-staged swap but with evacuation
		// wait steps. Route through the same keep-staged handler.
		if diff.FromClaim.KeepStaged {
			return e.createKeepStagedChangeoverOrders(nodeTask, node, diff)
		}

		// Order A: Robot A stages new material
		stageSteps := BuildStageSteps(diff.ToClaim)
		if stageSteps == nil {
			return fmt.Errorf("cannot build staging steps for node %s", node.Name)
		}
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, diff.ToClaim.InboundStaging, stageSteps)
		if err != nil {
			return fmt.Errorf("create staging order: %w", err)
		}
		// Order B: Robot B runs evacuate with 2 waits
		evacSteps := BuildEvacuateChangeoverSteps(diff.FromClaim, diff.ToClaim)
		orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, "", evacSteps)
		if err != nil {
			return fmt.Errorf("create evacuate order: %w", err)
		}
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested"); err != nil {
			log.Printf("changeover: update node task %d state to staging_requested: %v", nodeTask.ID, err)
		}
		log.Printf("changeover: evacuate node %s — Order A=%d (staging), Order B=%d (evacuate w/ 2 waits)", node.Name, orderA.ID, orderB.ID)

	case SituationAdd:
		if diff.ToClaim == nil {
			return fmt.Errorf("add requires to claim")
		}
		// Only staging order needed — no old material to evacuate
		return e.createFallbackStagingOrder(changeover, nodeTask, node, diff.ToClaim)

	case SituationDrop:
		if diff.FromClaim == nil {
			return fmt.Errorf("drop requires from claim")
		}
		if diff.FromClaim.OutboundStaging == "" {
			// No outbound staging — operator must handle manually
			return nil
		}
		// Only evacuation order needed — no new material coming
		releaseSteps := BuildReleaseSteps(diff.FromClaim)
		if releaseSteps == nil {
			return nil
		}
		orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, "", releaseSteps)
		if err != nil {
			return fmt.Errorf("create release order: %w", err)
		}
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, nil, &orderB.ID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "empty_requested"); err != nil {
			log.Printf("changeover: update node task %d state to empty_requested: %v", nodeTask.ID, err)
		}
		log.Printf("changeover: drop node %s — Order B=%d (evacuation)", node.Name, orderB.ID)
	}

	return nil
}

// createFallbackStagingOrder creates a simple staging order (Phase 1 behavior)
// when the full orders-up-front flow cannot be used (e.g., missing staging config).
func (e *Engine) createFallbackStagingOrder(
	changeover *processes.Changeover,
	nodeTask *processes.NodeTask,
	node *processes.Node,
	toClaim *processes.NodeClaim,
) error {
	nodeID := node.ID
	if toClaim.InboundStaging != "" {
		steps := BuildStageSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, toClaim.InboundStaging, steps)
			if err != nil {
				return err
			}
			if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nil); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested"); err != nil {
			log.Printf("changeover: update node task %d state to staging_requested: %v", nodeTask.ID, err)
		}
			return nil
		}
	}
	// Direct delivery fallback
	retrieveEmpty := toClaim.Role == "produce"
	order, err := e.orderMgr.CreateRetrieveOrder(&nodeID, retrieveEmpty, 1, toClaim.CoreNodeName, "", "standard", toClaim.PayloadCode, e.cfg.Web.AutoConfirm)
	if err != nil {
		return err
	}
	if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nil); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
	if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested"); err != nil {
			log.Printf("changeover: update node task %d state to staging_requested: %v", nodeTask.ID, err)
		}
	return nil
}

// createKeepStagedChangeoverOrders handles swap/evacuate changeovers where the
// old style had keep_staged enabled. The staging area has a pre-staged bin from
// the old style that must be cleared before new material can stage there.
//
// Split mode (two robots):
//
//	Order A — BuildKeepStagedDeliverSteps(toClaim): fetch new, stage, wait, deliver (1 wait)
//	Order B — BuildKeepStagedEvacSteps(fromClaim): pre-position, wait, evacuate old to final (1 wait)
//
// Combined mode (single robot):
//
//	Order A — BuildKeepStagedCombinedSteps(fromClaim, toClaim): clears old staged
//	bin back to source, fetches new, stages, waits, delivers (1 wait).
//	Order B — BuildKeepStagedEvacSteps(fromClaim) for line evacuation.
//
// The choice between split and combined is based on the from-claim's SwapMode.
func (e *Engine) createKeepStagedChangeoverOrders(
	nodeTask *processes.NodeTask,
	node *processes.Node,
	diff ChangeoverNodeDiff,
) error {
	nodeID := node.ID
	fromClaim := diff.FromClaim
	toClaim := diff.ToClaim

	switch fromClaim.SwapMode {
	case "two_robot":
		// Split: two robots work in parallel
		// Order A: fetch new → stage → wait → deliver
		deliverSteps := BuildKeepStagedDeliverSteps(toClaim)
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, toClaim.InboundStaging, deliverSteps)
		if err != nil {
			return fmt.Errorf("create keep-staged deliver order: %w", err)
		}
		// Order B: pre-position → wait → evacuate old → clear to final
		evacSteps := BuildKeepStagedEvacSteps(fromClaim)
		orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, "", evacSteps)
		if err != nil {
			return fmt.Errorf("create keep-staged evac order: %w", err)
		}
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested"); err != nil {
			log.Printf("changeover: update node task %d state to staging_requested: %v", nodeTask.ID, err)
		}
		log.Printf("changeover: keep-staged split node %s — Order A=%d (deliver w/ wait), Order B=%d (evac w/ wait)", node.Name, orderA.ID, orderB.ID)

	default:
		// Combined: single robot handles clearing old staged bin + staging new + delivery
		// Order A: clear old staged → fetch new → stage → wait → deliver
		combinedSteps := BuildKeepStagedCombinedSteps(fromClaim, toClaim)
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, toClaim.InboundStaging, combinedSteps)
		if err != nil {
			return fmt.Errorf("create keep-staged combined order: %w", err)
		}
		// Order B: evacuate old material from the line node
		evacSteps := BuildKeepStagedEvacSteps(fromClaim)
		orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, "", evacSteps)
		if err != nil {
			return fmt.Errorf("create keep-staged evac order: %w", err)
		}
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested"); err != nil {
			log.Printf("changeover: update node task %d state to staging_requested: %v", nodeTask.ID, err)
		}
		log.Printf("changeover: keep-staged combined node %s — Order A=%d (combined w/ wait), Order B=%d (evac w/ wait)", node.Name, orderA.ID, orderB.ID)
	}

	return nil
}

// findNodeByCoreName finds a process node by its CoreNodeName.
func findNodeByCoreName(nodes []processes.Node, coreName string) *processes.Node {
	for i := range nodes {
		if nodes[i].CoreNodeName == coreName {
			return &nodes[i]
		}
	}
	return nil
}

// ReleaseChangeoverWait releases all evacuation orders that are currently staged
// (waiting at a wait step). Called once per operator gate:
//   - First call releases the "ready" wait on all nodes
//   - For evacuate nodes, orders stage again at the second wait, and the second
//     call releases "tooling done"
//
// Each evacuation order is routed through ReleaseOrderWithLineside with the
// capture_lineside disposition so the bin's manifest is cleared at Core
// (via OrderRelease.RemainingUOP=0) before the fleet picks the bin up. Going
// through the lineside-aware path also runs the deactivation side-effect and
// the changeover-task state advance — without it, the evacuation bin would
// land at OutboundDestination still tagged with its old payload (the exact
// ALN_001 → SLN_002 → SMN_005 incident reported in 2026-04). LinesideCapture
// is empty here because the operator has already gated through the wait
// button by the time this is called — there's no per-part prompt at this
// point. CalledBy is plumbed through for audit.
func (e *Engine) ReleaseChangeoverWait(processID int64, calledBy string) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		return err
	}

	disp := ReleaseDisposition{
		Mode:     DispositionCaptureLineside,
		CalledBy: calledBy,
	}
	// Collect per-task failures rather than swallowing them. Pre-fix
	// behaviour was log-and-continue + return nil, which silently recreated
	// the original ALN_001 incident on partial failure: one node's manifest
	// stays stale, the operator gets a 200 OK, and the bin loader can't
	// move that bin. Returning errors.Join ensures the handler surfaces
	// the failed node names instead of lying about success.
	var failures []error
	for _, task := range tasks {
		if task.Situation == "unchanged" {
			continue
		}
		if task.OldMaterialReleaseOrderID == nil {
			continue
		}
		order, err := e.db.GetOrder(*task.OldMaterialReleaseOrderID)
		if err != nil {
			// Couldn't even read the order — log + collect for the rollup.
			log.Printf("release changeover wait node %s: get order: %v", task.NodeName, err)
			failures = append(failures, fmt.Errorf("node %s: get order: %w", task.NodeName, err))
			continue
		}
		if order.Status == orders.StatusStaged {
			if err := e.ReleaseOrderWithLineside(order.ID, disp); err != nil {
				log.Printf("release changeover wait node %s: %v", task.NodeName, err)
				failures = append(failures, fmt.Errorf("node %s: %w", task.NodeName, err))
			}
		}
	}
	return errors.Join(failures...)
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
	return e.cancelProcessChangeoverInternal(processID, nil)
}

// CancelProcessChangeoverRedirect cancels the active changeover and immediately
// starts a new one to a different target style. If nextStyleID is nil, behaves
// identically to CancelProcessChangeover (plain revert).
func (e *Engine) CancelProcessChangeoverRedirect(processID int64, nextStyleID *int64) error {
	return e.cancelProcessChangeoverInternal(processID, nextStyleID)
}

func (e *Engine) cancelProcessChangeoverInternal(processID int64, nextStyleID *int64) error {
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
		if err := e.db.UpdateChangeoverNodeTaskState(task.ID, "cancelled"); err != nil {
			log.Printf("changeover: update node task %d state to cancelled: %v", task.ID, err)
		}
	}

	// Clear runtime order references for affected nodes
	for _, task := range nodeTasks {
		runtime, err := e.db.GetProcessNodeRuntime(task.ProcessNodeID)
		if err != nil || runtime == nil {
			continue
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(task.ProcessNodeID, nil, nil); err != nil {
			log.Printf("changeover: update runtime orders for node %d: %v", task.ProcessNodeID, err)
		}
	}

	if err := e.db.UpdateProcessChangeoverState(changeover.ID, "cancelled"); err != nil {
		return err
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}

	// Redirect — start new changeover immediately to a different target style
	if nextStyleID != nil && *nextStyleID != 0 {
		_, err := e.StartProcessChangeover(processID, *nextStyleID,
			"changeover-redirect", "redirected from cancelled changeover")
		return err
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
		if err := e.db.UpdateChangeoverStationTaskState(task.ID, "switched"); err != nil {
			log.Printf("changeover: update station task state: %v", err)
		}
	}
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}
	return e.db.UpdateProcessChangeoverState(changeover.ID, "completed")
}

func isNodeTaskTerminal(task *processes.NodeTask) bool {
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
