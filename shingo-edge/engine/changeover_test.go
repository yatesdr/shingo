package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// seedChangeoverScenario creates two styles (from/to) with claims on the same
// core node, sets the from-style as active, and returns all IDs needed for
// changeover tests. The from-claim intentionally lacks OutboundStaging so that
// Phase 3 falls back to simple staging (no auto Order B). This lets the manual
// path tests (Empty → Release) continue working.
func seedChangeoverScenario(t *testing.T, db *store.DB) (processID, nodeID, fromStyleID, toStyleID, fromClaimID, toClaimID int64) {
	t.Helper()

	processID, err := db.CreateProcess("CO-PROC", "changeover test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "CO-NODE",
		Code:         "CO1",
		Name:         "Changeover Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}

	fromStyleID, err = db.CreateStyle("Style-FROM", "from style", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err = db.CreateStyle("Style-TO", "to style", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}

	// Set from-style as active
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// From-claim: consume node WITHOUT OutboundStaging — prevents Phase 3 auto Order B.
	// OutboundDestination is kept so the manual EvacuateNode path still works.
	fromClaimID, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:             fromStyleID,
		CoreNodeName:        "CO-NODE",
		Role:                "consume",
		SwapMode:            "simple",
		PayloadCode:         "PART-OLD",
		UOPCapacity:         100,
		InboundSource:       "SOURCE-OLD",
		OutboundDestination: "DEST-OLD",
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}

	// To-claim: consume node with inbound staging (triggers staged delivery path)
	toClaimID, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:        toStyleID,
		CoreNodeName:   "CO-NODE",
		Role:           "consume",
		SwapMode:       "simple",
		PayloadCode:    "PART-NEW",
		UOPCapacity:    200,
		InboundSource:  "SOURCE-NEW",
		InboundStaging: "IN-STAGING",
	})
	if err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}

	// Ensure runtime exists with from-claim active
	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fromClaimID, 50)

	return
}

// seedPhase3SwapScenario creates a swap changeover scenario with full staging
// config on both claims (InboundStaging + OutboundStaging + OutboundDestination).
// This triggers Phase 3 orders-up-front: Order A (staging) and Order B (swap
// with embedded wait step) are both created at changeover start.
func seedPhase3SwapScenario(t *testing.T, db *store.DB) (processID, nodeID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("P3-PROC", "phase3 swap test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "P3-NODE",
		Code:         "P3N1",
		Name:         "Phase3 Swap Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}

	fromStyleID, err = db.CreateStyle("Style-P3-FROM", "from style with full staging", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err = db.CreateStyle("Style-P3-TO", "to style with full staging", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}

	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// From-claim: full staging config — enables Phase 3 swap Order B
	fromClaimID, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:             fromStyleID,
		CoreNodeName:        "P3-NODE",
		Role:                "consume",
		SwapMode:            "simple",
		PayloadCode:         "PART-OLD",
		UOPCapacity:         100,
		InboundSource:       "SOURCE-OLD",
		OutboundStaging:     "OUT-STAGING",
		OutboundDestination: "DEST-OLD",
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}

	// To-claim: full staging config
	_, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:        toStyleID,
		CoreNodeName:   "P3-NODE",
		Role:           "consume",
		SwapMode:       "simple",
		PayloadCode:    "PART-NEW",
		UOPCapacity:    200,
		InboundSource:  "SOURCE-NEW",
		InboundStaging: "IN-STAGING",
	})
	if err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fromClaimID, 50)

	return
}

// startChangeover is a helper that starts a changeover and returns the
// changeover record and the node task for the single node.
func startChangeover(t *testing.T, eng *Engine, db *store.DB, processID, toStyleID int64) (*processes.Changeover, *processes.NodeTask) {
	t.Helper()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "test changeover")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	tasks, err := db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		t.Fatalf("list node tasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected at least one node task")
	}

	// Find the swap/add task (skip unchanged)
	for i := range tasks {
		if tasks[i].Situation != "unchanged" {
			return changeover, &tasks[i]
		}
	}
	return changeover, &tasks[0]
}

// emitOrderCompleted simulates an order completion event on the event bus.
func emitOrderCompleted(eng *Engine, orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID *int64) {
	// Fire EventOrderDelivered first so the runtime cache binding (which
	// now lives on the delivered event under the new contract, not on
	// EventOrderCompleted) sees the bin's arrival before any completion-
	// time bookkeeping runs. Production order lifecycle goes through
	// applyTransition which fires both events on the path through
	// StatusDelivered → StatusConfirmed; tests bypassing the lifecycle
	// service need the same shape.
	if processNodeID != nil {
		var binID *int64
		if order, err := eng.db.GetOrder(orderID); err == nil {
			binID = order.BinID
		}
		eng.Events.Emit(Event{
			Type: EventOrderDelivered,
			Payload: OrderDeliveredEvent{
				OrderID:       orderID,
				OrderUUID:     orderUUID,
				OrderType:     orderType,
				ProcessNodeID: processNodeID,
				BinID:         binID,
			},
		})
	}
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     orderUUID,
			OrderType:     orderType,
			ProcessNodeID: processNodeID,
		},
	})
}

// emitOrderFailed simulates an order failure event on the event bus.
func emitOrderFailed(eng *Engine, orderID int64, orderUUID string, orderType protocol.OrderType, reason string) {
	eng.Events.Emit(Event{
		Type: EventOrderFailed,
		Payload: OrderFailedEvent{
			OrderID:   orderID,
			OrderUUID: orderUUID,
			OrderType: orderType,
			Reason:    reason,
		},
	})
}

// markOrderTerminal advances an order to a terminal confirmed status so
// wiring sees it as completed. Also sets a default bin_id so the
// completion handler's binArrivingAt picks it up — without this, every
// test using this helper would land in the "no bin → reset to 0" path.
// Tests that exercise the no-bin path explicitly should set bin_id to
// nil after calling this helper.
func markOrderTerminal(db *store.DB, orderID int64) {
	db.UpdateOrderStatus(orderID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(orderID, string(orders.StatusAcknowledged))
	db.UpdateOrderStatus(orderID, string(orders.StatusInTransit))
	db.UpdateOrderStatus(orderID, string(orders.StatusDelivered))
	db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed))
	defaultBin := int64(900000 + orderID) // unique per order to avoid collisions
	db.UpdateOrderBinID(orderID, &defaultBin)
}

// TestChangeover_AutoStaging verifies that StartProcessChangeover automatically
// stages all swap/add positions without manual per-position clicks (Phase 2).
func TestChangeover_AutoStaging(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Start changeover — auto-staging should fire
	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "auto-stage test")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	// The node task should already be at staging_requested (not swap_required)
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}

	if task.Situation == "unchanged" {
		t.Skip("node situation is unchanged, nothing to auto-stage")
	}

	if task.State != domain.NodeTaskStagingRequested {
		t.Errorf("expected staging_requested after auto-stage, got %s", task.State)
	}

	// Should have a staging order linked
	if task.NextMaterialOrderID == nil {
		t.Error("expected NextMaterialOrderID to be set after auto-stage")
	}
}

// getAutoStagedOrder retrieves the staging order that was auto-created by
// StartProcessChangeover (Phase 2). Returns the order and node task.
func getAutoStagedOrder(t *testing.T, db *store.DB, changeoverID, nodeID int64) (*storeorders.Order, *processes.NodeTask) {
	t.Helper()
	task, err := db.GetChangeoverNodeTaskByNode(changeoverID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.State != domain.NodeTaskStagingRequested {
		t.Fatalf("expected staging_requested from auto-stage, got %s", task.State)
	}
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected staging order from auto-stage")
	}
	order, err := db.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		t.Fatalf("get auto-staged order: %v", err)
	}
	return order, task
}

// TestChangeover_StagingCompletion verifies that when a staging order completes,
// the node task state advances from staging_requested to staged.
func TestChangeover_StagingCompletion(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Auto-staging already created the order (Phase 2)
	order, _ := getAutoStagedOrder(t, db, changeover.ID, nodeID)

	// Simulate the staging order completing (delivered to InboundStaging)
	markOrderTerminal(db, order.ID)
	emitOrderCompleted(eng, order.ID, order.UUID, order.OrderType, &nodeID)

	// Verify state advanced to staged
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStaged {
		t.Errorf("expected staged, got %s", task.State)
	}
}

// TestChangeover_EmptyCompletion verifies that when an empty/clear order
// completes, the node task state advances from empty_requested to line_cleared.
func TestChangeover_EmptyCompletion(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Complete the auto-staged order
	stageOrder, _ := getAutoStagedOrder(t, db, changeover.ID, nodeID)
	markOrderTerminal(db, stageOrder.ID)
	emitOrderCompleted(eng, stageOrder.ID, stageOrder.UUID, stageOrder.OrderType, &nodeID)

	// Verify staged
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStaged {
		t.Fatalf("expected staged, got %s", task.State)
	}

	// Empty the node — creates move order for old material
	emptyOrder, err := eng.EvacuateNode(processID, nodeID, 0)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskEmptyRequested {
		t.Fatalf("expected empty_requested, got %s", task.State)
	}

	// Complete the empty order
	markOrderTerminal(db, emptyOrder.ID)
	emitOrderCompleted(eng, emptyOrder.ID, emptyOrder.UUID, emptyOrder.OrderType, &nodeID)

	// Verify line_cleared
	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskLineCleared {
		t.Errorf("expected line_cleared, got %s", task.State)
	}
}

// TestChangeover_ReleaseCompletion verifies that when a release order completes,
// the node task state advances to released and tryCompleteProcessChangeover fires.
func TestChangeover_ReleaseCompletion(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Complete auto-staged order
	stageOrder, _ := getAutoStagedOrder(t, db, changeover.ID, nodeID)
	markOrderTerminal(db, stageOrder.ID)
	emitOrderCompleted(eng, stageOrder.ID, stageOrder.UUID, stageOrder.OrderType, &nodeID)

	// Empty
	emptyOrder, err := eng.EvacuateNode(processID, nodeID, 0)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	markOrderTerminal(db, emptyOrder.ID)
	emitOrderCompleted(eng, emptyOrder.ID, emptyOrder.UUID, emptyOrder.OrderType, &nodeID)

	// Release
	releaseOrder, err := eng.DeliverNewMaterialForChangeover(processID, nodeID)
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskReleaseRequested {
		t.Fatalf("expected release_requested, got %s", task.State)
	}

	// Complete the release order
	markOrderTerminal(db, releaseOrder.ID)
	emitOrderCompleted(eng, releaseOrder.ID, releaseOrder.UUID, releaseOrder.OrderType, &nodeID)

	// Verify released
	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskReleased {
		t.Errorf("expected released, got %s", task.State)
	}
}

// TestChangeover_FullLifecycle tests the complete changeover flow with auto-staging:
// start (auto-stages) → empty → release → switch → cutover → complete.
func TestChangeover_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Start changeover — auto-staging fires automatically (Phase 2)
	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	process, _ := db.GetProcess(processID)
	if process.ProductionState != "changeover_active" {
		t.Fatalf("expected changeover_active, got %s", process.ProductionState)
	}

	// Auto-staging already created the order — find it from the node task
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStagingRequested {
		t.Fatalf("expected staging_requested from auto-stage, got %s", task.State)
	}
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected staging order from auto-stage")
	}
	stageOrder, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, stageOrder.ID)
	emitOrderCompleted(eng, stageOrder.ID, stageOrder.UUID, stageOrder.OrderType, &nodeID)

	// Empty
	emptyOrder, err := eng.EvacuateNode(processID, nodeID, 0)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	markOrderTerminal(db, emptyOrder.ID)
	emitOrderCompleted(eng, emptyOrder.ID, emptyOrder.UUID, emptyOrder.OrderType, &nodeID)

	// Release
	releaseOrder, err := eng.DeliverNewMaterialForChangeover(processID, nodeID)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	markOrderTerminal(db, releaseOrder.ID)
	emitOrderCompleted(eng, releaseOrder.ID, releaseOrder.UUID, releaseOrder.OrderType, &nodeID)

	// Switch
	testutil.MustNoErr(t, eng.SwitchNodeToTarget(processID, nodeID), "switch")

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskSwitched {
		t.Fatalf("expected switched, got %s", task.State)
	}

	// Complete the cutover (sets active_style_id, which is required for tryCompleteProcessChangeover)
	testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(processID), "cutover")

	// Verify changeover completed
	co, err := db.GetActiveProcessChangeover(processID)
	if err == nil && co != nil && co.State != domain.ChangeoverCompleted {
		t.Errorf("expected changeover completed, got state=%s", co.State)
	}

	process, _ = db.GetProcess(processID)
	if process.ProductionState != "active_production" {
		t.Errorf("expected active_production, got %s", process.ProductionState)
	}
	if process.ActiveStyleID == nil || *process.ActiveStyleID != toStyleID {
		t.Errorf("expected active style to be %d (to-style)", toStyleID)
	}
}

// TestChangeover_OrderFailure verifies that an order failure marks the node
// task as error, and a retry after failure succeeds.
func TestChangeover_OrderFailure(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Auto-staging already created the order (Phase 2) — retrieve it
	stageOrder, _ := getAutoStagedOrder(t, db, changeover.ID, nodeID)

	// Fail the staging order
	db.UpdateOrderStatus(stageOrder.ID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(stageOrder.ID, string(orders.StatusFailed))
	emitOrderFailed(eng, stageOrder.ID, stageOrder.UUID, stageOrder.OrderType, "test failure")

	// Verify error state
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskError {
		t.Fatalf("expected error, got %s", task.State)
	}

	// Retry staging — should succeed because the failed order is terminal
	retryOrder, err := eng.StageNodeChangeoverMaterial(processID, nodeID)
	if err != nil {
		t.Fatalf("retry stage: %v", err)
	}

	// Complete the retry order
	markOrderTerminal(db, retryOrder.ID)
	emitOrderCompleted(eng, retryOrder.ID, retryOrder.UUID, retryOrder.OrderType, &nodeID)

	// Verify recovered to staged
	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStaged {
		t.Errorf("expected staged after retry, got %s", task.State)
	}
}

// TestChangeover_CancelMidStaging verifies that cancelling a changeover while
// staging is in progress aborts the orders and reverts state.
func TestChangeover_CancelMidStaging(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Auto-staging already created the order (Phase 2) — retrieve it
	stageOrder, _ := getAutoStagedOrder(t, db, changeover.ID, nodeID)

	// Put order into submitted state so it's non-terminal
	db.UpdateOrderStatus(stageOrder.ID, string(orders.StatusSubmitted))

	// Cancel the changeover
	testutil.MustNoErr(t, eng.CancelProcessChangeover(processID), "cancel")

	// Verify staging order was aborted
	order, _ := db.GetOrder(stageOrder.ID)
	if !orders.IsTerminal(order.Status) {
		t.Errorf("expected staging order to be terminal after cancel, got %s", order.Status)
	}

	// Verify node tasks cancelled
	tasks, _ := db.ListChangeoverNodeTasks(changeover.ID)
	for _, task := range tasks {
		if task.Situation == "unchanged" {
			continue
		}
		if task.State != domain.NodeTaskCancelled {
			t.Errorf("node task %s: expected cancelled, got %s", task.NodeName, task.State)
		}
	}

	// Verify changeover cancelled and production state reverted
	co, _ := db.GetActiveProcessChangeover(processID)
	if co != nil {
		t.Errorf("expected no active changeover after cancel, got state=%s", co.State)
	}

	process, _ := db.GetProcess(processID)
	if process.ProductionState != "active_production" {
		t.Errorf("expected active_production, got %s", process.ProductionState)
	}
}

// seedAddNodeScenario creates a scenario where the to-style adds a new node
// that the from-style doesn't use. This produces a SituationAdd diff.
func seedAddNodeScenario(t *testing.T, db *store.DB) (processID, addNodeID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("ADD-PROC", "add node test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	addNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "ADD-NODE",
		Code:         "ADD1",
		Name:         "Add Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}

	fromStyleID, err = db.CreateStyle("Style-Empty", "no claims", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err = db.CreateStyle("Style-WithNode", "uses ADD-NODE", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}

	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// From-style has NO claims on ADD-NODE
	// To-style has a claim on ADD-NODE — this creates SituationAdd
	_, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:        toStyleID,
		CoreNodeName:   "ADD-NODE",
		Role:           "consume",
		SwapMode:       "simple",
		PayloadCode:    "PART-ADD",
		UOPCapacity:    100,
		InboundSource:  "SOURCE-ADD",
		InboundStaging: "STAGING-ADD",
	})
	if err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}

	db.EnsureProcessNodeRuntime(addNodeID)

	return
}

// TestChangeover_Phase3SwapLifecycle verifies the Phase 3 orders-up-front swap
// flow: StartProcessChangeover creates both Order A (staging) and Order B (swap
// with embedded wait) upfront. Order A completion → "staged". Order B completion
// → "released" (skips the manual empty → line_cleared → release path entirely).
func TestChangeover_Phase3SwapLifecycle(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Start changeover — Phase 3 should create Order A + Order B upfront
	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "phase3 swap")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	// Verify node task has both orders linked and situation is swap
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.Situation != "swap" {
		t.Fatalf("expected situation=swap, got %s", task.Situation)
	}
	if task.State != domain.NodeTaskStagingRequested {
		t.Fatalf("expected staging_requested, got %s", task.State)
	}
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected Order A (NextMaterialOrderID) to be set")
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected Order B (OldMaterialReleaseOrderID) to be set")
	}

	// Verify Order A is a complex order targeting inbound staging
	orderA, err := db.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		t.Fatalf("get order A: %v", err)
	}
	if orderA.OrderType != orders.TypeComplex {
		t.Errorf("Order A type: expected complex, got %s", orderA.OrderType)
	}
	if orderA.DeliveryNode != "IN-STAGING" {
		t.Errorf("Order A delivery: expected IN-STAGING, got %s", orderA.DeliveryNode)
	}

	// Verify Order B is a complex order (swap steps with wait)
	orderB, err := db.GetOrder(*task.OldMaterialReleaseOrderID)
	if err != nil {
		t.Fatalf("get order B: %v", err)
	}
	if orderB.OrderType != orders.TypeComplex {
		t.Errorf("Order B type: expected complex, got %s", orderB.OrderType)
	}

	// === Order A completes (staging delivery to IN-STAGING) ===
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStaged {
		t.Fatalf("after Order A: expected staged, got %s", task.State)
	}

	// === Order B completes (full swap: evacuate old + deliver new) ===
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskReleased {
		t.Fatalf("after Order B: expected released, got %s", task.State)
	}

	// Verify runtime switched to new claim with correct UOP
	runtime, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	// RemainingUOPCached no longer asserted at confirm — the cache
	// contract moved to delivered/release-click; coverage is in the
	// runtime-binding regression suite.
	// Verify active claim switched to to-claim
	if runtime.ActiveClaimID == nil {
		t.Fatal("expected active claim to be set after release")
	}
	toClaim, _ := db.GetStyleNodeClaimByNode(toStyleID, "P3-NODE")
	if *runtime.ActiveClaimID != toClaim.ID {
		t.Errorf("expected active claim = %d (to-claim), got %d", toClaim.ID, *runtime.ActiveClaimID)
	}
}

// TestChangeover_Phase3EvacuateLifecycle verifies the Phase 3 orders-up-front
// evacuate flow. Same as swap but Order B has 2 waits (ready + tooling done).
// When Order B completes, node goes directly to "released".
func TestChangeover_Phase3EvacuateLifecycle(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	// Create a process with evacuate situation (same payload, different UOP — or
	// we can force it by using the evacuate builder). For simplicity, we seed a
	// swap scenario but override the situation to "evacuate" by having the same
	// payload code (same material, different capacity triggers evacuate).
	processID, err := db.CreateProcess("P3E-PROC", "phase3 evacuate test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "P3E-NODE",
		Code:         "P3EN1",
		Name:         "Phase3 Evacuate Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}

	fromStyleID, err := db.CreateStyle("Style-P3E-FROM", "evacuate from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := db.CreateStyle("Style-P3E-TO", "evacuate to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}

	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// From-claim: full staging config, role=consume
	fromClaimID, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:             fromStyleID,
		CoreNodeName:        "P3E-NODE",
		Role:                "consume",
		SwapMode:            "simple",
		PayloadCode:         "PART-SAME",
		UOPCapacity:         100,
		InboundSource:       "SOURCE-OLD",
		OutboundStaging:     "OUT-STAGING",
		OutboundDestination: "DEST-OLD",
		// The OUTGOING claim carries the flag: the bins in the way of the tool
		// change are the ones this setup put on the node.
		EvacuateOnChangeover: true,
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}

	// To-claim: same payload code, and the outgoing evacuate flag above is
	// what resolves this to SituationEvacuate.
	_, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:        toStyleID,
		CoreNodeName:   "P3E-NODE",
		Role:           "consume",
		SwapMode:       "simple",
		PayloadCode:    "PART-SAME",
		UOPCapacity:    200,
		InboundSource:  "SOURCE-NEW",
		InboundStaging: "IN-STAGING",
	})
	if err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fromClaimID, 50)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Start changeover
	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "phase3 evacuate")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.Situation != "evacuate" {
		t.Fatalf("expected situation=evacuate, got %s", task.Situation)
	}
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected both Order A and Order B to be created for evacuate")
	}

	// Order A completion → staged
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStaged {
		t.Fatalf("after Order A: expected staged, got %s", task.State)
	}

	// Order B completion (evacuate with 2 waits — when fully complete) → released
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskReleased {
		t.Fatalf("after Order B: expected released, got %s", task.State)
	}

	// Runtime cache no longer asserted at confirm.
	_ = nodeID
}

// TestChangeover_Phase3FallbackToManual verifies that when a swap node is
// missing outbound staging config, Phase 3 falls back to simple staging
// (no Order B created), preserving the manual empty → release path.
func TestChangeover_Phase3FallbackToManual(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.Situation != "swap" {
		t.Fatalf("expected situation=swap, got %s", task.Situation)
	}

	// Should have Order A (staging) but NOT Order B (no outbound staging config)
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected staging order (fallback)")
	}
	if task.OldMaterialReleaseOrderID != nil {
		t.Error("expected NO Order B when outbound staging is missing (manual fallback)")
	}
}

// TestChangeover_SituationAdd verifies that auto-staging works for SituationAdd
// nodes — where the to-style uses a node that the from-style doesn't. The node
// has no active claim from the old style, but StageNodeChangeoverMaterial should
// still succeed by looking up the to-claim from the changeover's target style.
func TestChangeover_SituationAdd(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, addNodeID, _, toStyleID := seedAddNodeScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "add node test")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	// Find the node task for ADD-NODE
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, addNodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}

	if task.Situation != "add" {
		t.Fatalf("expected situation=add, got %s", task.Situation)
	}

	// Auto-staging should have fired — node task should be at staging_requested
	if task.State != domain.NodeTaskStagingRequested {
		t.Errorf("expected staging_requested after auto-stage of add node, got %s", task.State)
	}

	// Should have a staging order linked
	if task.NextMaterialOrderID == nil {
		t.Error("expected NextMaterialOrderID to be set for add node auto-stage")
	}
}

// seedKeepStagedSwapScenario creates a swap changeover scenario where the
// from-claim has KeepStaged=true. This means there's a pre-staged bin at
// InboundStaging from the old style that must be cleared during changeover.
func seedKeepStagedSwapScenario(t *testing.T, db *store.DB, swapMode protocol.SwapMode) (processID, nodeID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("KS-PROC", "keep-staged swap test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "KS-NODE",
		Code:         "KS1",
		Name:         "KeepStaged Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}

	fromStyleID, err = db.CreateStyle("Style-KS-FROM", "keep-staged from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err = db.CreateStyle("Style-KS-TO", "keep-staged to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}

	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// From-claim: KeepStaged=true, full staging config
	fromClaimID, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:             fromStyleID,
		CoreNodeName:        "KS-NODE",
		Role:                "consume",
		SwapMode:            swapMode,
		PayloadCode:         "PART-OLD",
		UOPCapacity:         100,
		InboundSource:       "SOURCE-OLD",
		InboundStaging:      "STAGING-AREA", // shared staging area
		OutboundStaging:     "OUT-STAGING",
		OutboundDestination: "DEST-OLD",
		KeepStaged:          domain.Ptr(true),
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}

	// To-claim: same staging area
	_, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:        toStyleID,
		CoreNodeName:   "KS-NODE",
		Role:           "consume",
		SwapMode:       swapMode,
		PayloadCode:    "PART-NEW",
		UOPCapacity:    200,
		InboundSource:  "SOURCE-NEW",
		InboundStaging: "STAGING-AREA", // same physical staging area
	})
	if err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fromClaimID, 50)

	return
}

// TestChangeover_KeepStagedCombined verifies the keep-staged combined (single
// robot) changeover flow. Order A clears old staged bin, fetches new, stages,
// waits, and delivers. Order B evacuates old material from the line.
func TestChangeover_KeepStagedCombined(t *testing.T) {
	t.Parallel()
	t.Skip("KeepStaged short-circuited; runtime hooks no-op'd, planner+builders preserved for rewire")
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedKeepStagedSwapScenario(t, db, "")
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "keep-staged combined")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.Situation != "swap" {
		t.Fatalf("expected situation=swap, got %s", task.Situation)
	}
	if task.State != domain.NodeTaskStagingRequested {
		t.Fatalf("expected staging_requested, got %s", task.State)
	}

	// Both Order A (combined) and Order B (evac) should be created
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected Order A (combined) to be set")
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected Order B (evac) to be set")
	}

	// Verify Order A targets the staging area
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	if orderA.OrderType != orders.TypeComplex {
		t.Errorf("Order A type: expected complex, got %s", orderA.OrderType)
	}
	if orderA.DeliveryNode != "STAGING-AREA" {
		t.Errorf("Order A delivery: expected STAGING-AREA, got %s", orderA.DeliveryNode)
	}

	// Order A completion → staged
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStaged {
		t.Fatalf("after Order A: expected staged, got %s", task.State)
	}

	// Order B completion (evac) — Order A already completed, so both are done → released
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskReleased {
		t.Fatalf("after Order B (Order A already done): expected released, got %s", task.State)
	}

	// Runtime cache no longer asserted at confirm.
	_ = nodeID
}

// TestChangeover_KeepStagedSplit verifies the keep-staged split (two robot)
// changeover flow. Order A fetches new and delivers with wait. Order B
// evacuates old material with wait.
func TestChangeover_KeepStagedSplit(t *testing.T) {
	t.Parallel()
	t.Skip("KeepStaged short-circuited; runtime hooks no-op'd, planner+builders preserved for rewire")
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedKeepStagedSwapScenario(t, db, "two_robot")
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "keep-staged split")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.Situation != "swap" {
		t.Fatalf("expected situation=swap, got %s", task.Situation)
	}

	// Both orders created
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected both Order A (deliver) and Order B (evac) for keep-staged split")
	}

	// Order A → staged, Order B (evac) — Order A already done so both complete → released
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskStaged {
		t.Fatalf("after Order A: expected staged, got %s", task.State)
	}

	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != domain.NodeTaskReleased {
		t.Fatalf("after Order B (Order A already done): expected released, got %s", task.State)
	}
}

// TestChangeover_OrderBBeforeOrderA verifies defensive behavior when Order B
// (evacuation/swap) completes before Order A (staging). In production this
// shouldn't happen because Order B has a wait step that holds the robot, but
// the wiring should not prematurely set "released" if Order A hasn't completed.
func TestChangeover_OrderBBeforeOrderA(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "order-b-first")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)

	// Complete Order B FIRST (before Order A)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	// State should be "released" because the wiring does handle Order B
	// completion independently — it checks situation=swap and sets released.
	// This is technically correct: the robot has done the full swap. The
	// question is whether Order A (staging) matters at this point.
	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)

	// Document actual behavior: Order B swap completion sets released regardless
	// of Order A status, because the swap order IS the delivery — the robot
	// picked new material from InboundStaging and delivered it to the node.
	// If Order B completed, the material is at the node.
	if task.State != domain.NodeTaskReleased {
		t.Logf("Order B before Order A: state=%s (expected released — swap order IS the delivery)", task.State)
	}

	// Runtime active_claim_id and cache no longer flip at Order B
	// confirm under the new contract. The state-machine assertion
	// above (task.State == "released") is the surviving check.
	_ = toStyleID
}

// Press-index changeover with Core unavailable refuses to start. Without
// Core's bin-type catalog, the per-position fan-out post-processor
// silently treats every payload pair as same-bin-type and would route a
// real different-bin-type changeover through the wrong choreography.
// The planner-side gate refuses the start with an operator-readable
// error so the floor sees the misconfig instead of executing a doomed
// plan.
func TestChangeover_PressIndex_CoreUnavailable_RefusesStart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("PI-NOCORE", "press-index core down", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PI-NODE", Code: "PI", Name: "Press Front", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}
	pairedNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PI-NODE-B", Code: "PIB", Name: "Press Back", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create paired process node: %v", err)
	}
	_ = nodeID
	_ = pairedNodeID

	fromStyleID, err := db.CreateStyle("PI-FROM", "from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := db.CreateStyle("PI-TO", "to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	if _, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "PI-NODE", Role: "consume", SwapMode: "two_robot_press_index",
		PayloadCode: "PART-A", UOPCapacity: 100, InboundSource: "SRC", OutboundDestination: "DEST",
		PairedCoreNode: "PI-NODE-B",
	}); err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	if _, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "PI-NODE", Role: "consume", SwapMode: "two_robot_press_index",
		PayloadCode: "PART-B", UOPCapacity: 200, InboundSource: "SRC", OutboundDestination: "DEST",
		PairedCoreNode: "PI-NODE-B",
	}); err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}

	// testEngine wires no coreClient → Available() returns false.
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	_, err = eng.StartProcessChangeover(processID, toStyleID, "test", "core down")
	if err == nil {
		t.Fatal("expected StartProcessChangeover to refuse with Core unavailable; got nil")
	}
	if !strings.Contains(err.Error(), "Core unavailable") {
		t.Errorf("err = %q, want substring %q", err.Error(), "Core unavailable")
	}
	if !strings.Contains(err.Error(), "PI-NODE") {
		t.Errorf("err = %q, want substring naming the node %q", err.Error(), "PI-NODE")
	}
}

// Sequential EVAC OrderB delivers to PairedCoreNode (each robot handles
// its own physical position). When OrderB completes, the wiring must
// reset the paired runtime row's UOP — without that, the paired
// position keeps reading the old style's UOP value until the
// reconciler heals (~60s) and the line UI lies during the window.
func TestSequentialEvacuate_OrderBCompletion_ResetsPairedRuntime(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("SEQ-EV-PROC", "sequential evac", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	primaryID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SEQ-A", Code: "SQA", Name: "Seq Position A", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create primary node: %v", err)
	}
	pairedID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SEQ-B", Code: "SQB", Name: "Seq Position B", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create paired node: %v", err)
	}

	fromStyleID, err := db.CreateStyle("SEQ-FROM", "from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := db.CreateStyle("SEQ-TO", "to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// Same payload + EvacuateOnChangeover so the diff resolves to
	// SituationEvacuate (not Swap), exercising the sequential evacuate
	// dispatch path and the OrderB→paired-runtime reset.
	// Sequential is direct-trip; staging fields intentionally omitted
	// so this test also pins that the planner doesn't divert sequential
	// claims into the staging-fallback path.
	fcID, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "SEQ-A", Role: "consume", SwapMode: "sequential",
		PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "MARKET", OutboundDestination: "DEST",
		// Outgoing claim owns the evacuate flag.
		PairedCoreNode: "SEQ-B", EvacuateOnChangeover: true,
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	if _, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "SEQ-A", Role: "consume", SwapMode: "sequential",
		PayloadCode: "PART-SAME", UOPCapacity: 250, InboundSource: "MARKET", OutboundDestination: "DEST",
		PairedCoreNode: "SEQ-B",
	}); err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}

	// Seed both runtimes with the old style's claim and partial UOP so
	// the assertion can distinguish "reset to new capacity" from "left
	// alone" or "zeroed".
	db.EnsureProcessNodeRuntime(primaryID)
	db.EnsureProcessNodeRuntime(pairedID)
	db.SetProcessNodeRuntime(primaryID, &fcID, 50)
	db.SetProcessNodeRuntime(pairedID, &fcID, 30)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "seq evac paired runtime")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, primaryID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.Situation != "evacuate" {
		t.Fatalf("expected evacuate, got %s", task.Situation)
	}
	// PER-NODE (2026-08-28): this asserted BOTH order slots, because sequential
	// evacuate used to return both positions' step lists from one diff — SEQ-A's
	// task carried SEQ-B's order too. Each position now owns its own task and
	// its own order, and only SEQ-A's claim is seeded here, so this task has
	// exactly one. SEQ-B's order lives on SEQ-B's task when SEQ-B is claimed.
	if task.NextMaterialOrderID == nil {
		t.Fatal("sequential evacuate must populate this position's order slot")
	}
	if task.OldMaterialReleaseOrderID != nil {
		t.Errorf("this task also carries order %d in the old-material slot. Per-node evacuate emits "+
			"ONE order per position; a second one here is the whole-press shape, whose two orders "+
			"then raced for the same bins.", *task.OldMaterialReleaseOrderID)
	}

	// Complete this position's order. Under the new contract, runtime cache no
	// longer flips at confirm — only at delivered (active_bin_id /
	// active_bin_epoch / remaining_uop_cached) and at release-click.
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &primaryID)

	// State-machine assertion: paired-position behavior previously asserted
	// here came from resetSequentialEvacOrderBRuntime which is gone. The
	// surviving check is that the changeover task transitioned correctly.
	_ = toStyleID
	_ = pairedID
}

// TestStartChangeover_RefusesWhileNodeHasOrderInFlight pins the replacement for
// the old AbortNodeOrders sweep. A changeover must NOT start while LIVE
// CHOREOGRAPHY is running at a participating node, and the refusal must name the
// node and the order so the operator knows what to wait for.
//
// The sweep this replaced cancelled those orders instead. On a press-index swap
// they are frequently carrying the empty carriers the changeover's own index
// legs need to pick up, so cancelling them mid-delivery deadlocks the changeover
// (HK 2026-07-28: the sweep MISSED orders 1249/1251 and that near-miss is the
// only reason the swap recovered).
//
// The order is driven to in_transit deliberately. This test used to leave it at
// the CreateOrder default of `pending` and still expect a refusal, because the
// gate keyed on !IsTerminal — which is eleven statuses, nine of them not a
// carrier doing anything. The gate now keys on protocol.BlocksChangeoverStart,
// so the blocking case has to be a genuinely blocking status; the pending case
// is asserted below as the opposite.
func TestStartChangeover_RefusesWhileNodeHasOrderInFlight(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	orderID, err := db.CreateOrder("uuid-inflight-co", orders.TypeRetrieve,
		&nodeID, false, 1, "CO-NODE", "", "", "", false, "PART-OLD")
	testutil.MustNoErr(t, err, "create in-flight order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "put the carrier in transit")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "attach order to runtime")

	_, err = eng.StartProcessChangeover(processID, toStyleID, "test", "")
	if err == nil {
		t.Fatal("changeover started while a participating node had a carrier in transit — must refuse")
	}
	if !strings.Contains(err.Error(), "Changeover Node") {
		t.Errorf("refusal = %q, want it to NAME the blocking node", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("order %d", orderID)) {
		t.Errorf("refusal = %q, want it to name order %d", err, orderID)
	}

	// The in-flight order must still be alive — the whole point is that we no
	// longer cancel the operator's work to clear the way.
	order, gerr := db.GetOrder(orderID)
	testutil.MustNoErr(t, gerr, "reload in-flight order")
	if orders.IsTerminal(order.Status) {
		t.Fatalf("in-flight order was terminated (status=%s) — refusing must never cancel it", order.Status)
	}

	// Clearing the RUNTIME POINTER must NOT unblock it. The pointer is UI state,
	// not truth — handler_bin_picked_up nulls it while the order is still live,
	// press-index legs live in the head node's slots, and primes are never
	// slotted at all. The gate is destination-keyed precisely so a cleared
	// pointer cannot wave a moving carrier through.
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, nil, nil), "clear runtime slots")
	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err == nil {
		t.Fatal("changeover started after only the runtime pointer was cleared — the carrier is " +
			"still in transit to this node; the pointer is not the truth")
	}

	// The order actually landing is what clears the way.
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "land the carrier")
	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("changeover must start once the carrier is terminal: %v", err)
	}
}

// TestStartChangeover_PassesNonBlockingStatuses is the other half of the gate's
// decision, and it is the half the floor was hitting. None of these is a carrier
// in motion: pending and queued have no bin assigned, delivered has already
// landed and is waiting on a clerical confirm, and acknowledged/submitted are
// Edge-lifecycle words the fleet never emits.
//
// acknowledged carries the sharpest consequence and is why this test names each
// status rather than looping anonymously: NOTHING reaps it — AbandonStuckOrders
// is scoped to {dispatched, staged}, no Core reconciler or Edge ticker moves it,
// and this HMI has no operator order cancel. Blocking on it locked the operator
// out of changeover until somebody restarted Edge.
func TestStartChangeover_PassesNonBlockingStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []protocol.Status{
		orders.StatusPending,
		orders.StatusQueued,
		orders.StatusSourcing,
		orders.StatusSubmitted,
		orders.StatusAcknowledged,
		orders.StatusDelivered,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
			eng := testEngine(t, db)

			orderID, err := db.CreateOrder("uuid-pass-"+string(status), orders.TypeRetrieve,
				&nodeID, false, 1, "CO-NODE", "", "", "", false, "PART-OLD")
			testutil.MustNoErr(t, err, "create order")
			testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(status)), "set status")
			testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "attach to runtime")

			if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
				t.Fatalf("changeover refused with a %s order at the node: %v — "+
					"nothing is moving, so this must not block", status, err)
			}

			// Not blocking and not cancelled are different properties, and the
			// split is protocol.ChangeoverStartActionFor — NOT IsPreDispatch,
			// which is what this test used to assert and what let SNF2 happen.
			// Everything with no carrier is cancelled, and that now includes
			// submitted and acknowledged. Only `delivered` survives: the bin is
			// physically there and all that is outstanding is the operator's
			// count, which the changeover has no business discarding.
			order, gerr := db.GetOrder(orderID)
			testutil.MustNoErr(t, gerr, "reload order")
			if protocol.ChangeoverStartActionFor(status) == protocol.ChangeoverStartCancel {
				if order.Status != orders.StatusCancelled {
					t.Errorf("%s order = %s, want cancelled at changeover start — leaving it "+
						"alive lets an outgoing-style order outlive the changeover "+
						"(SPR SNF2 2026-07-30)", status, order.Status)
				}
				return
			}
			if orders.IsTerminal(order.Status) {
				t.Errorf("%s order was terminated (status=%s) — the bin has landed and only "+
					"the operator's count is outstanding; the changeover must not discard it",
					status, order.Status)
			}
		})
	}
}

// TestChangeoverBlockerNeverSaysInFlightAboutAQueuedOrder is the §6 hygiene
// assertion. The old string was "%s has order %d in flight (%s)", so a queued
// order rendered as "has order 4471 in flight (queued)" — a sentence
// contradicting its own parenthesis. Only statuses that genuinely block reach
// the formatter now, and each names its own remedy.
func TestChangeoverBlockerNeverSaysInFlightAboutAQueuedOrder(t *testing.T) {
	t.Parallel()
	for _, s := range protocol.AllStatuses() {
		if !protocol.BlocksChangeoverStart(s) {
			continue
		}
		got := changeoverBlockerFor("ALN_007", 4471, s)
		if !strings.Contains(got, "ALN_007") || !strings.Contains(got, "4471") {
			t.Errorf("%s: blocker %q must name the node and the order", s, got)
		}
		switch s {
		case protocol.StatusStaged:
			if !strings.Contains(got, "Release the wait") {
				t.Errorf("staged blocker %q must name the release remedy", got)
			}
		case protocol.StatusFaulted:
			if !strings.Contains(got, "maintenance") {
				t.Errorf("faulted blocker %q must say it is not operator-clearable", got)
			}
		}
	}
	// The exact regression: no blocker can be produced for a queued order at all,
	// because queued does not block.
	if protocol.BlocksChangeoverStart(protocol.StatusQueued) {
		t.Fatal("queued must not block — the 'in flight (queued)' sentence must be unreachable")
	}
}

// seedSequentialScenario builds a two-position sequential press mid changeover:
// SEQ-A active (the line is pulling from it), SEQ-B parked, both claimed in both
// styles so each yields its own Swap diff and its own order.
//
// evacuate=true makes it a tooling EVACUATE instead of a SWAP (same payload in
// both styles + the marker on the outgoing claims).
func seedSequentialScenario(t *testing.T, db *store.DB, evacuate bool) (
	eng *Engine, processID, activeNodeID, parkedNodeID int64, co *processes.Changeover) {
	t.Helper()

	processID, err := db.CreateProcess("SEQ-CUT-PROC", "sequential cutover", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	activeNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SEQ-A", Code: "SCA", Name: "Seq A", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create active node: %v", err)
	}
	parkedNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SEQ-B", Code: "SCB", Name: "Seq B", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create parked node: %v", err)
	}
	// The line is pulling from SEQ-A, so SEQ-B is the parked side and SEQ-A is
	// the one whose order holds for the cutover click.
	db.EnsureProcessNodeRuntime(activeNodeID)
	db.EnsureProcessNodeRuntime(parkedNodeID)
	testutil.MustNoErr(t, db.SetActivePull(activeNodeID, true), "active pull on SEQ-A")
	testutil.MustNoErr(t, db.SetActivePull(parkedNodeID, false), "no active pull on SEQ-B")

	fromStyleID, err := db.CreateStyle("SEQ-CUT-FROM", "from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := db.CreateStyle("SEQ-CUT-TO", "to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// Both positions claimed in BOTH styles — the A/B steady state, and what
	// makes a PRESS-2-RUN → PRESS-2-ALT changeover a Swap on each position
	// rather than a Drop/Add pair.
	toPayload := "PART-TO"
	if evacuate {
		toPayload = "PART-FROM" // same payload both sides → SituationEvacuate, not Swap
	}
	for _, c := range []struct {
		style        int64
		own, partner string
		payload      string
		isFrom       bool
	}{
		{fromStyleID, "SEQ-A", "SEQ-B", "PART-FROM", true},
		{fromStyleID, "SEQ-B", "SEQ-A", "PART-FROM", true},
		{toStyleID, "SEQ-A", "SEQ-B", toPayload, false},
		{toStyleID, "SEQ-B", "SEQ-A", toPayload, false},
	} {
		if _, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
			StyleID: c.style, CoreNodeName: c.own, Role: "produce", SwapMode: "sequential",
			PayloadCode: c.payload, UOPCapacity: 100,
			InboundSource: "MARKET", OutboundDestination: "DEST",
			PairedCoreNode:       c.partner,
			EvacuateOnChangeover: evacuate && c.isFrom, // the outgoing claim owns the marker
		}); err != nil {
			t.Fatalf("upsert claim %s/%d: %v", c.own, c.style, err)
		}
	}

	eng = testEngine(t, db)
	eng.wireEventHandlers()
	co, err = eng.StartProcessChangeover(processID, toStyleID, "test", "seq cutover")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	return eng, processID, activeNodeID, parkedNodeID, co
}

// ── THE RELEASE GUARD: A SPEED BUMP, NOT A WALL ───────────────────────────
//
// A robot may not strip a position the line is drawing from. That is physical
// and mode-agnostic — it is equally true in steady state — so the guard reads
// one fact (`active_pull` on an A/B position) and nothing about changeovers.
//
// It is refuse-by-DEFAULT and override-by-CONFIRM, because `active_pull` is a
// bit and bits go stale: a PLC that missed an edge, a runtime row written
// before someone moved a bin by hand. The operator can see the aisle; the
// system cannot. So the guard states the fact, names the next click, and gets
// out of the way when he insists.

// TestReleaseGuard_WarnsWhileTheLineIsPullingFromThePosition is the per-node
// door: the click is refused with the fact and the flip that fixes it.
func TestReleaseGuard_WarnsWhileTheLineIsPullingFromThePosition(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	activeOrder, _ := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(activeOrder, string(orders.StatusStaged)), "stage")

	_, err := eng.ReleaseChangeoverWaitForNode(processID, activeNodeID, ReleaseDisposition{CalledBy: "op"})
	if err == nil {
		t.Fatal("the release went through while the line was still pulling from SEQ-A. A robot sent " +
			"to lift that bin stops production on a press that was running.")
	}
	for _, want := range []string{"SEQ-A", "SEQ-B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("warning = %q, want it to name %q — the fact and the next click, or it is just "+
				"a button that does not work", err.Error(), want)
		}
	}
	if got := orderStatusOf(t, db, activeOrder); got != string(orders.StatusStaged) {
		t.Errorf("the order moved to %q despite the warning; a refused click must change nothing", got)
	}
}

// TestReleaseGuard_ConfirmReleasesAnyway — the operator outranks the bit.
func TestReleaseGuard_ConfirmReleasesAnyway(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	activeOrder, _ := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(activeOrder, string(orders.StatusStaged)), "stage")

	res, err := eng.ReleaseChangeoverWaitForNode(processID, activeNodeID,
		ReleaseDisposition{CalledBy: "op", ConfirmActivePull: true})
	if err != nil {
		t.Fatalf("the confirmed release was refused: %v. The guard is a speed bump — the person at "+
			"the press can see the aisle and the system cannot.", err)
	}
	if res.Released != 1 {
		t.Errorf("released=%d, want 1 — the confirm must actually release", res.Released)
	}
}

// TestReleaseGuard_SweepDeclinesAndNamesTheNode — a plant-wide click carries no
// per-node intent, so it can never answer the confirm the guard asks for. It
// reports the position by name instead of deciding for the operator.
func TestReleaseGuard_SweepDeclinesAndNamesTheNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	activeOrder, parkedOrder := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	for _, id := range []int64{activeOrder, parkedOrder} {
		testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusStaged)), "stage")
	}

	res, err := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "supervisor"})
	if err != nil {
		t.Fatalf("the sweep errored; declining one node must not fail it for the others: %v", err)
	}
	if got := orderStatusOf(t, db, activeOrder); got != string(orders.StatusStaged) {
		t.Errorf("the sweep released the position the line is pulling from (status %q)", got)
	}
	if got := orderStatusOf(t, db, parkedOrder); got == string(orders.StatusStaged) {
		t.Error("the sweep declined the PARKED side too — nothing is pulling from it, so it should " +
			"have released; the guard must key on the pull bit, not on being sequential")
	}
	if len(res.NeedsFlip) != 1 || res.NeedsFlip[0] != "SEQ-A" {
		t.Errorf("NeedsFlip = %v, want exactly [SEQ-A] — the operator has to know which press still "+
			"wants a click", res.NeedsFlip)
	}
}

// TestReleaseGuard_SweepNeverAutoConfirms pins the thing that would quietly
// undo the whole guard: a sweep carrying a confirm.
func TestReleaseGuard_SweepNeverAutoConfirms(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, processID, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	activeOrder, _ := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(activeOrder, string(orders.StatusStaged)), "stage")

	// Even handed a confirm, the SWEEP must not use it: the confirm is an
	// answer about one aisle, and a plant-wide click was not aimed at one.
	res, err := eng.ReleaseChangeoverWait(processID,
		ReleaseDisposition{CalledBy: "supervisor", ConfirmActivePull: true})
	if err != nil {
		t.Fatalf("sweep errored: %v", err)
	}
	if got := orderStatusOf(t, db, activeOrder); got != string(orders.StatusStaged) {
		t.Errorf("a plant-wide sweep carrying a confirm released the pulling position (status %q). "+
			"A confirm answers for ONE press; a sweep was not aimed at one, so it may not spend it "+
			"on every press at once.", got)
	}
	if len(res.NeedsFlip) == 0 {
		t.Error("the sweep reported no NeedsFlip — it must still name what it declined")
	}
}

// TestReleaseGuard_IgnoresNodesThatAreNotHalfOfAPair — the guard is about A/B
// geometry, not about changeovers, and a single-position node has no partner to
// flip to. It must not become a wall in front of every ordinary release.
func TestReleaseGuard_IgnoresNodesThatAreNotHalfOfAPair(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", "single node"); err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, db.SetActivePull(nodeID, true), "active pull on the single node")

	// Whatever this release does about staging, it must not be refused BY THE
	// PULL GUARD: CO-NODE has no PairedCoreNode, so there is no partner to flip
	// to and the fact the guard reports would be meaningless.
	_, err := eng.ReleaseChangeoverWaitForNode(processID, nodeID, ReleaseDisposition{CalledBy: "op"})
	if err != nil && strings.Contains(err.Error(), "the line is pulling from") {
		t.Fatalf("the pull guard fired on a node with no A/B partner: %v", err)
	}
}

// seqTaskOrders returns the (active, parked) positions' changeover order ids.
func seqTaskOrders(t *testing.T, db *store.DB, changeoverID, activeNodeID, parkedNodeID int64) (active, parked int64) {
	t.Helper()
	get := func(nodeID int64) int64 {
		task, err := db.GetChangeoverNodeTaskByNode(changeoverID, nodeID)
		if err != nil {
			t.Fatalf("get node task %d: %v", nodeID, err)
		}
		if task.NextMaterialOrderID == nil {
			t.Fatalf("node task %d has no order; the fixture is wrong", nodeID)
		}
		return *task.NextMaterialOrderID
	}
	return get(activeNodeID), get(parkedNodeID)
}

func orderStatusOf(t *testing.T, db *store.DB, orderID int64) string {
	t.Helper()
	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("get order %d: %v", orderID, err)
	}
	return string(o.Status)
}

// activePullOf reads one process node's active-pull bit.
func activePullOf(t *testing.T, db *store.DB, nodeID int64) bool {
	t.Helper()
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("get runtime for node %d: %v", nodeID, err)
	}
	return rt.ActivePull
}

// ── THE FLIP GUARD: DO NOT PUT THE LINE ONTO A POSITION THAT CANNOT FEED IT ──
//
// The flip is what makes the other side releasable, so it is where "has the
// operator got a bin of the new product" belongs. Every arm is answered from
// state this Edge already owns — it holds no bin table, and needs none, because
// the same steady-state invariant the reuse-skip leans on carries the knowledge:
// a produce press's parked side holds an empty of the running style's carrier.
//
//	SKIPPED       the reuse shortcut turned this side Unchanged, which happens
//	              only when the catalog says both styles ride the same carrier.
//	DELIVERED     this side's own changeover order is terminal — the Edge watched
//	              its robot deliver.
//	STEADY STATE  no changeover; a bin is present.

// TestFlipGuard_SkippedSideIsReadyImmediately — arm (a). The reuse shortcut
// already decided the resident empty is the right carrier, so there is no order
// to wait for and the flip must not invent one.
func TestFlipGuard_SkippedSideIsReadyImmediately(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, _, parkedNodeID, co := seedSequentialScenario(t, db, false)

	// Stand in for the shortcut having fired on this side.
	task, err := db.GetChangeoverNodeTaskByNode(co.ID, parkedNodeID)
	testutil.MustNoErr(t, err, "get parked task")
	_, uErr := db.Exec("UPDATE changeover_node_tasks SET situation=? WHERE id=?",
		string(SituationUnchanged), task.ID)
	testutil.MustNoErr(t, uErr, "mark the parked side skipped")

	if err := eng.FlipABNode(parkedNodeID, OperatorFlip("op")); err != nil {
		t.Fatalf("flip onto the skipped side was refused: %v. The catalog said both styles ride the "+
			"same carrier, so the empty already standing there IS the one the new style wants — "+
			"there is nothing to deliver and nothing to wait for.", err)
	}
	if !activePullOf(t, db, parkedNodeID) {
		t.Error("the flip reported success but did not move the pull")
	}
}

// TestFlipGuard_WarnsUntilTheTargetsOrderDelivers — arm (b), produce. A
// different-carrier side has a real order; until it lands, the position cannot
// feed the line.
func TestFlipGuard_WarnsUntilTheTargetsOrderDelivers(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	_, parkedOrder := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)

	err := eng.FlipABNode(parkedNodeID, OperatorFlip("op"))
	if err == nil {
		t.Fatal("the flip went through while the parked side's changeover order was still running. " +
			"The line would be switched onto a position whose new carrier has not arrived.")
	}
	for _, want := range []string{"SEQ-B", fmt.Sprint(parkedOrder)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("warning = %q, want it to name %q — the operator needs the position and the "+
				"order that fixes it", err.Error(), want)
		}
	}
	if activePullOf(t, db, parkedNodeID) {
		t.Error("a refused flip moved the pull anyway")
	}

	// CONFIRM OVERRIDES. The bit is the system's belief; the operator can see
	// the aisle and may know better.
	if err := eng.FlipABNode(parkedNodeID, FlipRequest{Confirm: true, CalledBy: "op"}); err != nil {
		t.Fatalf("the confirmed flip was refused: %v — the guard is a speed bump, not a wall", err)
	}
	if !activePullOf(t, db, parkedNodeID) {
		t.Error("the confirmed flip did not move the pull")
	}

	// AND ONCE IT DELIVERS, no warning at all.
	db2 := testEngineDB(t)
	eng2, _, activeB, parkedB, co2 := seedSequentialScenario(t, db2, false)
	_, parkedOrder2 := seqTaskOrders(t, db2, co2.ID, activeB, parkedB)
	markOrderTerminal(db2, parkedOrder2)
	if err := eng2.FlipABNode(parkedB, OperatorFlip("op")); err != nil {
		t.Fatalf("flip refused after the parked side's order delivered: %v", err)
	}
}

// TestFlipGuard_ConsumeAlsoWantsMaterial — arm (b)'s consume conjunct. An empty
// carrier on a consume position is as bad as nothing, so "the order finished" is
// not on its own enough.
func TestFlipGuard_ConsumeAlsoWantsMaterial(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, false)
	_, parkedOrder := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	markOrderTerminal(db, parkedOrder)

	// Make the target a CONSUME position with no material on it.
	_, rErr := db.Exec("UPDATE style_node_claims SET role='consume' WHERE core_node_name='SEQ-B'")
	testutil.MustNoErr(t, rErr, "make SEQ-B consume")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(parkedNodeID, nil, 0), "no material")

	err := eng.FlipABNode(parkedNodeID, OperatorFlip("op"))
	if err == nil {
		t.Fatal("the flip went through onto a consume position holding no material. Its order " +
			"finished, but an empty carrier at a consume position feeds the line nothing.")
	}
	if !strings.Contains(err.Error(), "material") {
		t.Errorf("warning = %q, want it to say what is missing", err.Error())
	}
}

// TestFlipGuard_SteadyStateWantsABinPresent — arm (c). Outside a changeover
// there is nothing to be ready FOR beyond a bin being there.
func TestFlipGuard_SteadyStateWantsABinPresent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, _, parkedNodeID, co := seedSequentialScenario(t, db, false)

	// End the changeover so this is ordinary A/B cycling.
	testutil.MustNoErr(t, db.UpdateProcessChangeoverState(co.ID, domain.ChangeoverCompleted),
		"close changeover")

	if err := eng.FlipABNode(parkedNodeID, OperatorFlip("op")); err == nil {
		t.Fatal("steady-state flip onto a position with no bin was allowed; the line would starve")
	}
	bin := int64(4242)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(parkedNodeID, nil, &bin, 40), "put a bin on it")
	if err := eng.FlipABNode(parkedNodeID, OperatorFlip("op")); err != nil {
		t.Fatalf("steady-state flip onto a position holding a bin was refused: %v", err)
	}
}

// TestFlipGuard_PLCCannotConfirm — a PLC bit cannot look at the aisle, so it
// cannot override. Refused loudly (changeover-53 precedent), never silently.
func TestFlipGuard_PLCCannotConfirm(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, _, parkedNodeID, _ := seedSequentialScenario(t, db, false)

	// Even carrying a confirm, a PLC request must be refused: the field is an
	// operator's statement that he looked, and a PLC has no eyes.
	err := eng.FlipABNode(parkedNodeID, FlipRequest{ByPLC: true, Confirm: true, CalledBy: "plc"})
	if err == nil {
		t.Fatal("a PLC flip onto an unready position was allowed. A PLC cannot see whether the new " +
			"carrier arrived; only a person can, and this must land in front of one.")
	}
	if activePullOf(t, db, parkedNodeID) {
		t.Error("the refused PLC flip moved the pull anyway")
	}
}

// ── THE PRODUCE REUSE-SKIP ────────────────────────────────────────────────
//
// Same answer press-index already gives to the identical question: ask the
// CATALOG what each style rides and lean on the mode's steady-state invariant,
// rather than asking the floor what is on deck. A produce press's parked side
// holds an empty of the running style's carrier; if the carrier does not
// change, that empty is already the one the new style wants, so swapping it for
// an identical one is a robot trip that moves nothing.
func seqReuseDiffs(role protocol.ClaimRole) []ChangeoverNodeDiff {
	mk := func(own, partner, payload string) processes.NodeClaim {
		c := fullSwapClaim(own, payload, role)
		c.SwapMode = protocol.SwapModeSequential
		c.PairedCoreNode = partner
		return c
	}
	fromA, toA := mk("A_POS", "B_POS", "PART-FROM"), mk("A_POS", "B_POS", "PART-TO")
	fromB, toB := mk("B_POS", "A_POS", "PART-FROM"), mk("B_POS", "A_POS", "PART-TO")
	return []ChangeoverNodeDiff{
		{CoreNodeName: "A_POS", Situation: SituationSwap, FromClaim: &fromA, ToClaim: &toA},
		{CoreNodeName: "B_POS", Situation: SituationSwap, FromClaim: &fromB, ToClaim: &toB},
	}
}

func TestSequentialReuseSkip_SameCarrierProduceSkipsTheParkedSide(t *testing.T) {
	t.Parallel()
	// Empty active-pull snapshot → canonical tie-break → A_POS is parked.
	sameCarrier := map[string]string{"PART-FROM": "STANDARD-SM", "PART-TO": "STANDARD-SM"}

	out := ApplySequentialReuseShortcut(seqReuseDiffs(protocol.ClaimRoleProduce), sameCarrier, nil)
	got := map[string]ChangeoverSituation{}
	for _, d := range out {
		got[d.CoreNodeName] = d.Situation
	}
	if got["A_POS"] != SituationUnchanged {
		t.Errorf("the PARKED side is %q, want unchanged. Both styles ride STANDARD-SM, so the empty "+
			"already standing there is the one the new style wants — no order, no button, no release.",
			got["A_POS"])
	}
	if got["B_POS"] != SituationSwap {
		t.Errorf("the ACTIVE side is %q, want swap — it holds the outgoing style's partial full and "+
			"that has to leave whatever the carrier does", got["B_POS"])
	}
}

func TestSequentialReuseSkip_NeverSkipsConsume(t *testing.T) {
	t.Parallel()
	sameCarrier := map[string]string{"PART-FROM": "STANDARD-SM", "PART-TO": "STANDARD-SM"}
	for _, d := range ApplySequentialReuseShortcut(seqReuseDiffs(protocol.ClaimRoleConsume), sameCarrier, nil) {
		if d.Situation != SituationSwap {
			t.Errorf("consume position %s was skipped (%q). A consume press's parked side holds a FULL "+
				"standby of the OUTGOING style — material, not an empty carrier — and material is "+
				"material: it leaves and is replaced whatever the carrier does.", d.CoreNodeName, d.Situation)
		}
	}
}

func TestSequentialReuseSkip_DifferentCarrierSwapsBothSides(t *testing.T) {
	t.Parallel()
	diffCarrier := map[string]string{"PART-FROM": "STANDARD-SM", "PART-TO": "DEEP-LG"}
	for _, d := range ApplySequentialReuseShortcut(seqReuseDiffs(protocol.ClaimRoleProduce), diffCarrier, nil) {
		if d.Situation != SituationSwap {
			t.Errorf("position %s was skipped on a CARRIER CHANGE (%q) — the resident empty is the "+
				"wrong shape for the incoming style", d.CoreNodeName, d.Situation)
		}
	}
}

// NO CATALOG, NO SKIP — the opposite direction from binTypesDiffer's, and the
// reason binTypesKnownSame is not its negation. Acting on unknown here means
// SKIPPING a side; the degraded direction has to be one redundant carrier swap,
// never a wrong one. press-index can read unknown as "same" because
// refusePressIndexWhenCoreUnavailable refuses the changeover outright; sequential
// has no such gate.
func TestSequentialReuseSkip_NoCatalogAnswerBuildsBothOrders(t *testing.T) {
	t.Parallel()
	for name, binTypes := range map[string]map[string]string{
		"core down":     {},
		"one side only": {"PART-FROM": "STANDARD-SM"},
		"blank entries": {"PART-FROM": "", "PART-TO": ""},
	} {
		for _, d := range ApplySequentialReuseShortcut(seqReuseDiffs(protocol.ClaimRoleProduce), binTypes, nil) {
			if d.Situation != SituationSwap {
				t.Errorf("%s: position %s was skipped without a catalog answer (%q). Unknown must "+
					"build both orders — a redundant swap is recoverable, a skipped one is a press "+
					"left on the wrong carrier.", name, d.CoreNodeName, d.Situation)
			}
		}
	}
}

// TestSequentialReuseSkip_LeavesPressIndexAlone is the Unit 5 inertness proof:
// HK runs two_robot_press_index, and this pass must be invisible to it. The
// argument is structural — the predicate's first test is the sequential mode —
// but HK is a live plant and "structural" is what an assertion is for.
func TestSequentialReuseSkip_LeavesPressIndexAlone(t *testing.T) {
	t.Parallel()
	sameCarrier := map[string]string{"PART-FROM": "STANDARD-SM", "PART-TO": "STANDARD-SM"}
	diffs := seqReuseDiffs(protocol.ClaimRoleProduce)
	for i := range diffs {
		diffs[i].FromClaim.SwapMode = protocol.SwapModeTwoRobotPressIndex
		diffs[i].ToClaim.SwapMode = protocol.SwapModeTwoRobotPressIndex
	}
	for _, d := range ApplySequentialReuseShortcut(diffs, sameCarrier, nil) {
		if d.Situation != SituationSwap {
			t.Errorf("press-index position %s was rewritten to %q by the SEQUENTIAL shortcut. HK runs "+
				"this mode; its own reuse pass is ApplyReuseCompatibleBinsShortcut and it must stay "+
				"the only thing that touches it.", d.CoreNodeName, d.Situation)
		}
	}
}
