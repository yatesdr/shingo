package engine

import (
	"database/sql"
	"fmt"
	"log"
	"slices"
	"time"

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
		_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)
		order, _ = e.db.GetOrder(order.ID)
		return &NodeOrderResult{CycleMode: "simple", Order: order, ProcessNodeID: nodeID}, nil
	}

	switch claim.SwapMode {
	case "sequential":
		steps := BuildSequentialRemovalSteps(claim)
		orderA, err := e.orderMgr.CreateComplexOrder(&nodeID, quantity, "", steps) // "" = removal, no UOP reset
		if err != nil {
			return nil, err
		}
		_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, nil)
		orderA, _ = e.db.GetOrder(orderA.ID)
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
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, quantity, claim.InboundSource, claim.CoreNodeName)
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
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, runtime.StagedOrderID)
	order, _ = e.db.GetOrder(order.ID)
	return order, nil
}

func (e *Engine) ConfirmNodeManifest(nodeID int64) error {
	// Manifest confirmation is now core's domain. This is a no-op on edge
	// but kept for API compatibility.
	return nil
}

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

	// Abort pre-existing orders on affected nodes (not unchanged ones).
	for _, diff := range diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(nodes, diff.CoreNodeName)
		if node != nil {
			e.AbortNodeOrders(node.ID)
		}
	}

	// Retrieve the changeover we just created so we can link node tasks.
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil, err
	}

	// Phase 3: create ALL robot orders up front with embedded wait steps.
	// Operator controls flow by releasing waits, not by triggering individual orders.
	for _, diff := range diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(nodes, diff.CoreNodeName)
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
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error")
		}
	}

	return e.db.GetActiveProcessChangeover(processID)
}

// createChangeoverOrders creates the appropriate robot orders for a single node
// based on its changeover situation. For swap/evacuate nodes, two orders are
// created: Order A (staging to inbound staging) and Order B (complex swap/evacuate
// with wait steps). For add/drop situations, only one order is needed.
func (e *Engine) createChangeoverOrders(
	changeover *store.ProcessChangeover,
	nodeTask *store.ChangeoverNodeTask,
	node *store.ProcessNode,
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
		_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID)
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
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
		_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID)
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
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
		_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, nil, &orderB.ID)
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "empty_requested")
		log.Printf("changeover: drop node %s — Order B=%d (evacuation)", node.Name, orderB.ID)
	}

	return nil
}

// createFallbackStagingOrder creates a simple staging order (Phase 1 behavior)
// when the full orders-up-front flow cannot be used (e.g., missing staging config).
func (e *Engine) createFallbackStagingOrder(
	changeover *store.ProcessChangeover,
	nodeTask *store.ChangeoverNodeTask,
	node *store.ProcessNode,
	toClaim *store.StyleNodeClaim,
) error {
	nodeID := node.ID
	if toClaim.InboundStaging != "" {
		steps := BuildStageSteps(toClaim)
		if steps != nil {
			order, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, toClaim.InboundStaging, steps)
			if err != nil {
				return err
			}
			_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nil)
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
			return nil
		}
	}
	// Direct delivery fallback
	retrieveEmpty := toClaim.Role == "produce"
	order, err := e.orderMgr.CreateRetrieveOrder(&nodeID, retrieveEmpty, 1, toClaim.CoreNodeName, "", "standard", toClaim.PayloadCode, e.cfg.Web.AutoConfirm)
	if err != nil {
		return err
	}
	_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &order.ID, nil)
	_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
	return nil
}

// createKeepStagedChangeoverOrders handles swap/evacuate changeovers where the
// old style had keep_staged enabled. The staging area has a pre-staged bin from
// the old style that must be cleared before new material can stage there.
//
// Split mode (two robots):
//   Order A — BuildKeepStagedDeliverSteps(toClaim): fetch new, stage, wait, deliver (1 wait)
//   Order B — BuildKeepStagedEvacSteps(fromClaim): pre-position, wait, evacuate old to final (1 wait)
//   The old staged bin at InboundStaging is NOT automatically cleared by these
//   orders — it must be handled separately (e.g., operator clears it, or the
//   staging area can hold multiple bins).
//
// Combined mode (single robot):
//   Order A — BuildKeepStagedCombinedSteps(fromClaim, toClaim): clears old staged
//   bin back to source, fetches new, stages, waits, delivers (1 wait). One order
//   handles everything. No Order B needed for the staging — but we still need
//   Order B for line evacuation (BuildKeepStagedEvacSteps).
//
// The choice between split and combined is based on the from-claim's SwapMode.
func (e *Engine) createKeepStagedChangeoverOrders(
	nodeTask *store.ChangeoverNodeTask,
	node *store.ProcessNode,
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
		_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID)
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
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
		_ = e.db.LinkChangeoverNodeOrders(nodeTask.ID, &orderA.ID, &orderB.ID)
		_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staging_requested")
		log.Printf("changeover: keep-staged combined node %s — Order A=%d (combined w/ wait), Order B=%d (evac w/ wait)", node.Name, orderA.ID, orderB.ID)
	}

	return nil
}

// findNodeByCoreName finds a process node by its CoreNodeName.
func findNodeByCoreName(nodes []store.ProcessNode, coreName string) *store.ProcessNode {
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
func (e *Engine) ReleaseChangeoverWait(processID int64) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		return err
	}

	for _, task := range tasks {
		if task.Situation == "unchanged" {
			continue
		}
		if task.OldMaterialReleaseOrderID == nil {
			continue
		}
		order, err := e.db.GetOrder(*task.OldMaterialReleaseOrderID)
		if err != nil {
			continue
		}
		if order.Status == orders.StatusStaged {
			if err := e.orderMgr.ReleaseOrder(order.ID); err != nil {
				log.Printf("release changeover wait node %s: %v", task.NodeName, err)
			}
		}
	}
	return nil
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

// CanAcceptOrders reports whether a process node can accept new orders.
// Returns false with a human-readable reason if the node is unavailable.
// Consolidates all availability checks: active/staged order, changeover.
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
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, nil, nil)
}

// FlipABNode switches the active pull point to the specified node and deactivates
// its paired partner. Used for A/B cycling — operator (or PLC bit) decides when
// to start pulling from the other side. Triggers auto-reorder on the depleted node
// if the depleted node's UOP is at or below its reorder point.
func (e *Engine) FlipABNode(nodeID int64) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil {
		return fmt.Errorf("node not found: %w", err)
	}

	claim := e.findActiveClaim(node)
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.PairedCoreNode == "" {
		return fmt.Errorf("node %s is not part of an A/B pair", node.Name)
	}

	// Find the paired node
	process, err := e.db.GetProcess(node.ProcessID)
	if err != nil {
		return err
	}
	nodes, err := e.db.ListProcessNodesByProcess(node.ProcessID)
	if err != nil {
		return err
	}
	var pairedNode *store.ProcessNode
	for i := range nodes {
		if nodes[i].CoreNodeName == claim.PairedCoreNode {
			pairedNode = &nodes[i]
			break
		}
	}
	if pairedNode == nil {
		return fmt.Errorf("paired node %s not found", claim.PairedCoreNode)
	}

	// Activate this node, deactivate the partner
	_ = e.db.SetActivePull(nodeID, true)
	_ = e.db.SetActivePull(pairedNode.ID, false)

	log.Printf("A/B flip: node %s now active, node %s inactive", node.Name, pairedNode.Name)

	// Trigger auto-reorder on the depleted partner if needed
	if process.ActiveStyleID != nil {
		pairedClaim, _ := e.db.GetStyleNodeClaimByNode(*process.ActiveStyleID, pairedNode.CoreNodeName)
		pairedRuntime, _ := e.db.GetProcessNodeRuntime(pairedNode.ID)
		if pairedClaim != nil && pairedRuntime != nil &&
			pairedClaim.AutoReorder && pairedRuntime.RemainingUOP <= pairedClaim.ReorderPoint {
			if ok, _ := e.CanAcceptOrders(pairedNode.ID); ok {
				if _, err := e.RequestNodeMaterial(pairedNode.ID, 1); err != nil {
					log.Printf("A/B flip auto-reorder for depleted node %s: %v", pairedNode.Name, err)
				}
			}
		}
	}

	return nil
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

// LoadBin marks a bin at a bin_loader node as loaded with the given manifest.
// Calls Core's HTTP API directly to set the manifest on the existing bin at
// that node. No transport order is created — the bin stays in place until a
// downstream consume node pulls it.
func (e *Engine) LoadBin(nodeID int64, payloadCode string, uopCount int64, manifest []protocol.IngestManifestItem) error {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != "bin_loader" {
		return fmt.Errorf("node %s is not a bin_loader node", node.Name)
	}
	if len(manifest) == 0 {
		return fmt.Errorf("manifest is empty")
	}

	// Check that a bin is actually at this node
	if e.coreClient.Available() {
		bins, _ := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		if len(bins) == 0 || !bins[0].Occupied {
			return fmt.Errorf("no bin at node %s — request an empty bin first", node.Name)
		}
	}

	// Validate payload code against allowed list
	allowed := claim.AllowedPayloads()
	if payloadCode == "" && len(allowed) > 0 {
		payloadCode = allowed[0]
	}
	if payloadCode == "" {
		return fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(allowed, payloadCode) {
		return fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}

	if uopCount <= 0 {
		for _, item := range manifest {
			uopCount += item.Quantity
		}
	}

	// Load bin via direct HTTP to Core — synchronous, immediate feedback
	items := make([]ManifestItem, len(manifest))
	for i, m := range manifest {
		items[i] = ManifestItem{PartNumber: m.PartNumber, Quantity: m.Quantity, Description: m.Description}
	}
	if _, err := e.coreClient.LoadBin(&BinLoadRequest{
		NodeName:    node.CoreNodeName,
		PayloadCode: payloadCode,
		UOPCount:    uopCount,
		Manifest:    items,
	}); err != nil {
		return fmt.Errorf("load bin: %w", err)
	}

	// Update edge-side runtime state
	claimID := claim.ID
	_ = e.db.SetProcessNodeRuntime(nodeID, &claimID, int(uopCount))

	// If outbound destination is configured, move the loaded bin there
	if claim.OutboundDestination != "" {
		order, err := e.orderMgr.CreateMoveOrder(&nodeID, 1, node.CoreNodeName, claim.OutboundDestination)
		if err != nil {
			log.Printf("bin_loader: move to outbound for node %s: %v", node.Name, err)
		} else {
			_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)
		}
	}

	return nil
}

// ClearBin clears the manifest on the bin at a bin_loader node, resetting it
// to empty. Used to fix mis-loads.
func (e *Engine) ClearBin(nodeID int64) error {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != "bin_loader" {
		return fmt.Errorf("node %s is not a bin_loader node", node.Name)
	}
	if err := e.coreClient.ClearBin(node.CoreNodeName); err != nil {
		return fmt.Errorf("clear bin: %w", err)
	}
	claimID := claim.ID
	_ = e.db.SetProcessNodeRuntime(nodeID, &claimID, 0)
	return nil
}

// RequestEmptyBin requests an empty bin compatible with the given payload to be
// delivered to a bin_loader node. Core queues the order if no empties are
// immediately available. payloadCode determines bin type compatibility.
func (e *Engine) RequestEmptyBin(nodeID int64, payloadCode string) (*store.Order, error) {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != "bin_loader" {
		return nil, fmt.Errorf("node %s is not a bin_loader node", node.Name)
	}
	if ok, reason := e.CanAcceptOrders(nodeID); !ok {
		return nil, fmt.Errorf("node %s unavailable: %s", node.Name, reason)
	}

	// Check that node doesn't already have a bin
	if e.coreClient.Available() {
		bins, _ := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		if len(bins) > 0 && bins[0].Occupied {
			return nil, fmt.Errorf("node %s already has a bin", node.Name)
		}
	}

	// Validate payload code against allowed list
	if payloadCode == "" {
		return nil, fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(claim.AllowedPayloads(), payloadCode) {
		return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}

	// Create retrieve order for an empty bin — Core queues if none available
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, true, 1, node.CoreNodeName, "",
		"standard", payloadCode, e.cfg.Web.AutoConfirm,
	)
	if err != nil {
		return nil, err
	}
	_ = e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil)
	return order, nil
}

// tryAutoRequestEmpty attempts to auto-request an empty bin for a bin_loader node.
// Fails silently on any error — the next trigger will retry.
func (e *Engine) tryAutoRequestEmpty(node *store.ProcessNode, claim *store.StyleNodeClaim) {
	if claim.AutoRequestPayload == "" {
		return
	}
	order, err := e.RequestEmptyBin(node.ID, claim.AutoRequestPayload)
	if err != nil {
		log.Printf("bin_loader auto-request for node %s: %v", node.Name, err)
		return
	}
	log.Printf("bin_loader auto-request: created order %d for node %s (payload %s)", order.ID, node.Name, claim.AutoRequestPayload)
}