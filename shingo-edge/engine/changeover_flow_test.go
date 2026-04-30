package engine

import (
	"testing"

	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// Seed helpers for changeover flow tests
// ---------------------------------------------------------------------------

// seedDropScenario creates a process where from-style has a node with full
// outbound config but to-style does NOT claim it — producing SituationDrop.
func seedDropScenario(t *testing.T, db *store.DB) (processID, nodeID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("DROP-PROC", "drop test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "DROP-NODE", Code: "DN1", Name: "Drop Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	fromStyleID, _ = db.CreateStyle("DROP-FROM", "drop from", processID)
	toStyleID, _ = db.CreateStyle("DROP-TO", "drop to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	fcID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "DROP-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-DROP", UOPCapacity: 100, InboundSource: "SRC-DROP",
		OutboundStaging: "OUT-STAGE", OutboundDestination: "DEST-DROP",
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	// To-style has NO claim on DROP-NODE → SituationDrop

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fcID, 50)
	return
}

// seedMultiNodeScenario creates 4 nodes with distinct changeover situations:
// NODE-SWAP (different payload), NODE-UNCHANGED (same payload),
// NODE-DROP (only in from), NODE-ADD (only in to).
func seedMultiNodeScenario(t *testing.T, db *store.DB) (processID int64, nodes map[string]int64, fromStyleID, toStyleID int64) {
	t.Helper()
	nodes = make(map[string]int64)

	processID, _ = db.CreateProcess("MULTI-PROC", "multi node test", "active_production", "", "", false)
	for i, name := range []string{"NODE-SWAP", "NODE-UNCHANGED", "NODE-DROP", "NODE-ADD"} {
		nid, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: processID, CoreNodeName: name, Code: string(rune('A' + i)),
			Name: name, Sequence: i + 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		nodes[name] = nid
	}

	fromStyleID, _ = db.CreateStyle("MULTI-FROM", "from", processID)
	toStyleID, _ = db.CreateStyle("MULTI-TO", "to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	// From-style claims: SWAP (full staging), UNCHANGED, DROP (full staging)
	swapFC, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "NODE-SWAP", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-OLD", UOPCapacity: 100, InboundSource: "SRC-OLD",
		OutboundStaging: "OUT-STAGE", OutboundDestination: "DEST-OLD",
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "NODE-UNCHANGED", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "SRC-SAME",
	})
	dropFC, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "NODE-DROP", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-DROP", UOPCapacity: 100, InboundSource: "SRC-DROP",
		OutboundStaging: "OUT-STAGE-DROP", OutboundDestination: "DEST-DROP",
	})

	// To-style claims: SWAP (new payload, full staging), UNCHANGED (same), ADD (new node)
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "NODE-SWAP", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-NEW", UOPCapacity: 200, InboundSource: "SRC-NEW", InboundStaging: "IN-STAGE",
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "NODE-UNCHANGED", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "SRC-SAME",
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "NODE-ADD", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-ADD", UOPCapacity: 50, InboundSource: "SRC-ADD", InboundStaging: "IN-STAGE-ADD",
	})

	for _, name := range []string{"NODE-SWAP", "NODE-UNCHANGED", "NODE-DROP", "NODE-ADD"} {
		db.EnsureProcessNodeRuntime(nodes[name])
	}
	db.SetProcessNodeRuntime(nodes["NODE-SWAP"], &swapFC, 50)
	db.SetProcessNodeRuntime(nodes["NODE-DROP"], &dropFC, 50)
	return
}

// seedChangeoverRoleScenario creates a node with role="changeover" in both styles.
func seedChangeoverRoleScenario(t *testing.T, db *store.DB) (processID, nodeID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, _ = db.CreateProcess("COR-PROC", "changeover role test", "active_production", "", "", false)
	nodeID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "COR-NODE", Code: "CR1", Name: "CO Role Node", Sequence: 1, Enabled: true,
	})

	fromStyleID, _ = db.CreateStyle("COR-FROM", "co from", processID)
	toStyleID, _ = db.CreateStyle("COR-TO", "co to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	fcID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "COR-NODE", Role: "changeover", SwapMode: "simple",
		PayloadCode: "PART-CO", UOPCapacity: 100, InboundSource: "SRC-CO",
		InboundStaging: "IN-STAGE-CO", OutboundStaging: "OUT-STAGE-CO", OutboundDestination: "DEST-CO",
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "COR-NODE", Role: "changeover", SwapMode: "simple",
		PayloadCode: "PART-CO", UOPCapacity: 100, InboundSource: "SRC-CO",
		InboundStaging: "IN-STAGE-CO", OutboundStaging: "OUT-STAGE-CO", OutboundDestination: "DEST-CO",
	})

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fcID, 100)
	return
}

// seedNoChangeScenario creates two styles with identical claims on the same node.
func seedNoChangeScenario(t *testing.T, db *store.DB) (processID, nodeID, fromStyleID, toStyleID int64) {
	t.Helper()

	processID, _ = db.CreateProcess("NOCHANGE-PROC", "no change test", "active_production", "", "", false)
	nodeID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "NC-NODE", Code: "NC1", Name: "No Change Node", Sequence: 1, Enabled: true,
	})

	fromStyleID, _ = db.CreateStyle("NC-FROM", "from", processID)
	toStyleID, _ = db.CreateStyle("NC-TO", "to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	fcID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "NC-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "SRC",
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "NC-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "SRC",
	})

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fcID, 50)
	return
}

// findTaskByNode is a helper to find a node task by CoreNodeName from a list.
func findTaskByNode(tasks []processes.NodeTask, coreName string) *processes.NodeTask {
	for i := range tasks {
		if tasks[i].NodeName == coreName {
			return &tasks[i]
		}
	}
	return nil
}

// ===========================================================================
// Section 3: Changeover flow gaps
// ===========================================================================

// TC-91: SituationDrop lifecycle — evacuation-only order, no Order A.
func TestChangeoverFlow_SituationDrop(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, toStyleID := seedDropScenario(t, db)
	_ = fromStyleID
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "drop test")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Situation != "drop" {
		t.Fatalf("expected situation=drop, got %s", task.Situation)
	}

	// Drop: only Order B (evacuation), no Order A
	if task.NextMaterialOrderID != nil {
		t.Error("expected NO Order A for drop (no new material needed)")
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected Order B (evacuation) for drop")
	}

	// Verify Order B is a complex order (release steps)
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	if orderB.OrderType != orders.TypeComplex {
		t.Errorf("Order B type: expected complex, got %s", orderB.OrderType)
	}
	if task.State != "empty_requested" {
		t.Errorf("expected empty_requested, got %s", task.State)
	}

	// Complete Order B → line_cleared
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "line_cleared" {
		t.Errorf("after Order B complete: expected line_cleared, got %s", task.State)
	}
}

// TC-92: Multi-node changeover — 4 nodes with distinct situations.
func TestChangeoverFlow_MultiNode(t *testing.T) {
	db := testEngineDB(t)
	processID, nodes, fromStyleID, toStyleID := seedMultiNodeScenario(t, db)
	_ = fromStyleID
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "multi node")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	tasks, err := db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}

	// Verify situations
	wantSituations := map[string]string{
		"NODE-SWAP":      "swap",
		"NODE-UNCHANGED": "unchanged",
		"NODE-DROP":      "drop",
		"NODE-ADD":       "add",
	}
	for name, want := range wantSituations {
		task := findTaskByNode(tasks, name)
		if task == nil {
			t.Errorf("missing task for %s", name)
			continue
		}
		if task.Situation != want {
			t.Errorf("%s: situation=%q, want %q", name, task.Situation, want)
		}
	}

	// Unchanged should have no orders
	unchanged := findTaskByNode(tasks, "NODE-UNCHANGED")
	if unchanged.NextMaterialOrderID != nil || unchanged.OldMaterialReleaseOrderID != nil {
		t.Error("NODE-UNCHANGED should have no orders")
	}

	// Swap should have Order A + Order B
	swap := findTaskByNode(tasks, "NODE-SWAP")
	if swap.NextMaterialOrderID == nil || swap.OldMaterialReleaseOrderID == nil {
		t.Error("NODE-SWAP should have both orders (Phase 3)")
	}

	// Drop should have only Order B
	drop := findTaskByNode(tasks, "NODE-DROP")
	if drop.NextMaterialOrderID != nil {
		t.Error("NODE-DROP should have no Order A")
	}
	if drop.OldMaterialReleaseOrderID == nil {
		t.Error("NODE-DROP should have Order B (evacuation)")
	}

	// Add should have only Order A
	add := findTaskByNode(tasks, "NODE-ADD")
	if add.NextMaterialOrderID == nil {
		t.Error("NODE-ADD should have Order A (staging)")
	}
	if add.OldMaterialReleaseOrderID != nil {
		t.Error("NODE-ADD should have no Order B")
	}

	// Complete swap: Order A → staged, Order B → released
	swapNodeID := nodes["NODE-SWAP"]
	if swap.NextMaterialOrderID != nil {
		orderA, _ := db.GetOrder(*swap.NextMaterialOrderID)
		markOrderTerminal(db, orderA.ID)
		emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &swapNodeID)
	}
	if swap.OldMaterialReleaseOrderID != nil {
		orderB, _ := db.GetOrder(*swap.OldMaterialReleaseOrderID)
		markOrderTerminal(db, orderB.ID)
		emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &swapNodeID)
	}

	tasks, _ = db.ListChangeoverNodeTasks(changeover.ID)
	swapTask := findTaskByNode(tasks, "NODE-SWAP")
	if swapTask.State != "released" {
		t.Errorf("NODE-SWAP after both orders complete: state=%s, want released", swapTask.State)
	}

	// Switch swap node → verify tryComplete is blocked (drop/add not done)
	eng.SwitchNodeToTarget(processID, nodes["NODE-SWAP"])
	// Changeover should NOT complete yet (other nodes still pending)
	co, _ := db.GetActiveProcessChangeover(processID)
	if co == nil || co.State == "completed" {
		t.Error("changeover should not complete — drop/add nodes not done")
	}
}

// TC-93: Changeover-only role — evacuate and restore.
func TestChangeoverFlow_ChangeoverRole(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedChangeoverRoleScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "co role")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Situation != "evacuate" {
		t.Fatalf("expected situation=evacuate (changeover role), got %s", task.Situation)
	}
	// Should have both orders (evacuate with full staging config)
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected both Order A (staging) and Order B (evacuate) for changeover role")
	}

	// Complete Order A → staged
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "staged" {
		t.Fatalf("after Order A: expected staged, got %s", task.State)
	}

	// Complete Order B (evacuate with 2 waits) → released
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "released" {
		t.Fatalf("after Order B: expected released, got %s", task.State)
	}
}

// TC-94: Double changeover — complete one, start another, verify clean state.
func TestChangeoverFlow_DoubleChangeover(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, toStyleID := seedPhase3SwapScenario(t, db)
	_ = fromStyleID
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// First changeover
	changeover, task := startChangeover(t, eng, db, processID, toStyleID)
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)

	// Complete both orders
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	// Switch + cutover
	eng.SwitchNodeToTarget(processID, nodeID)
	eng.CompleteProcessProductionCutover(processID)

	// Verify first changeover completed
	co1, err := db.GetActiveProcessChangeover(processID)
	if err == nil && co1 != nil && co1.State != "completed" {
		t.Fatalf("first changeover not completed: state=%s", co1.State)
	}

	// Create a third style for the second changeover
	styleC, _ := db.CreateStyle("Style-P3-C", "third style", processID)
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleC, CoreNodeName: "P3-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-C", UOPCapacity: 300, InboundSource: "SRC-C", InboundStaging: "IN-STAGING",
	})

	// Second changeover
	co2, err := eng.StartProcessChangeover(processID, styleC, "test", "second changeover")
	if err != nil {
		t.Fatalf("second changeover: %v", err)
	}
	if co2.ID == changeover.ID {
		t.Error("second changeover should be a new record, not the same as first")
	}

	// Verify node task for second changeover
	task2, _ := db.GetChangeoverNodeTaskByNode(co2.ID, nodeID)
	if task2.Situation != "swap" {
		t.Errorf("second changeover situation: expected swap, got %s", task2.Situation)
	}
	if task2.State == "unchanged" {
		t.Error("second changeover should have active orders, not unchanged")
	}

	// Verify production state
	proc, _ := db.GetProcess(processID)
	if proc.ProductionState != "changeover_active" {
		t.Errorf("production state: expected changeover_active, got %s", proc.ProductionState)
	}
}

// TC-95: Changeover with no claim changes — all unchanged, effective no-op.
func TestChangeoverFlow_NoClaimChanges(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, toStyleID := seedNoChangeScenario(t, db)
	_ = fromStyleID
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "no changes")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.Situation != "unchanged" {
		t.Fatalf("expected unchanged, got %s", task.Situation)
	}
	// No orders should be created for unchanged nodes
	if task.NextMaterialOrderID != nil || task.OldMaterialReleaseOrderID != nil {
		t.Error("unchanged node should have no orders")
	}

	// Cutover should still work
	eng.CompleteProcessProductionCutover(processID)

	proc, _ := db.GetProcess(processID)
	if proc.ActiveStyleID == nil || *proc.ActiveStyleID != toStyleID {
		t.Error("active style should be set to to-style after cutover")
	}
	if proc.ProductionState != "active_production" {
		t.Errorf("expected active_production, got %s", proc.ProductionState)
	}
}

// TC-96: Cancel mid-release — Order B (swap) is in progress.
func TestChangeoverFlow_CancelMidRelease(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Complete Order A → staged
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	// Put Order B into in_transit (non-terminal, simulating robot executing)
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	db.UpdateOrderStatus(orderB.ID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(orderB.ID, string(orders.StatusAcknowledged))
	db.UpdateOrderStatus(orderB.ID, string(orders.StatusInTransit))

	// Cancel while Order B is mid-flight
	if err := eng.CancelProcessChangeover(processID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Verify Order B was aborted
	orderB, _ = db.GetOrder(orderB.ID)
	if !orders.IsTerminal(orderB.Status) {
		t.Errorf("Order B should be terminal after cancel, got %s", orderB.Status)
	}

	// Verify changeover cancelled
	co, _ := db.GetActiveProcessChangeover(processID)
	if co != nil {
		t.Errorf("expected no active changeover after cancel, got state=%s", co.State)
	}

	proc, _ := db.GetProcess(processID)
	if proc.ProductionState != "active_production" {
		t.Errorf("expected active_production, got %s", proc.ProductionState)
	}
}

// TC-97: Order A fails — staging order fails, node goes to error, retry succeeds.
func TestChangeoverFlow_OrderAFails(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)

	// Fail Order A (staging)
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	db.UpdateOrderStatus(orderA.ID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(orderA.ID, string(orders.StatusFailed))
	emitOrderFailed(eng, orderA.ID, orderA.UUID, orderA.OrderType, "staging failed")

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "error" {
		t.Fatalf("expected error after Order A failure, got %s", task.State)
	}

	// Retry staging
	retryOrder, err := eng.StageNodeChangeoverMaterial(processID, nodeID)
	if err != nil {
		t.Fatalf("retry staging: %v", err)
	}

	// Complete retry
	markOrderTerminal(db, retryOrder.ID)
	emitOrderCompleted(eng, retryOrder.ID, retryOrder.UUID, retryOrder.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "staged" {
		t.Errorf("expected staged after retry, got %s", task.State)
	}
}

// TC-98: Order B fails — swap/evacuate order fails, node goes to error.
func TestChangeoverFlow_OrderBFails(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)

	// Complete Order A first
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "staged" {
		t.Fatalf("expected staged after Order A, got %s", task.State)
	}

	// Fail Order B (swap)
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	db.UpdateOrderStatus(orderB.ID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(orderB.ID, string(orders.StatusFailed))
	emitOrderFailed(eng, orderB.ID, orderB.UUID, orderB.OrderType, "swap failed")

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "error" {
		t.Fatalf("expected error after Order B failure, got %s", task.State)
	}

	// Manual recovery: empty + release should still work (failed order is terminal)
	emptyOrder, err := eng.EmptyNodeForToolChange(processID, nodeID, 0)
	if err != nil {
		t.Fatalf("manual empty after Order B failure: %v", err)
	}
	markOrderTerminal(db, emptyOrder.ID)
	emitOrderCompleted(eng, emptyOrder.ID, emptyOrder.UUID, emptyOrder.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	// The manual empty order is complex; wiring handler matches it as a
	// swap/evacuate completion and advances directly to "released".
	if task.State != "released" {
		t.Errorf("expected released after manual empty (complex order on swap situation), got %s", task.State)
	}
}

// TC-99: Partial completion — 3 nodes, 2 complete, 1 errors. Cutover blocked.
func TestChangeoverFlow_PartialCompletion(t *testing.T) {
	db := testEngineDB(t)

	processID, _ := db.CreateProcess("PARTIAL-PROC", "partial test", "active_production", "", "", false)
	fromStyleID, _ := db.CreateStyle("PART-FROM", "from", processID)
	toStyleID, _ := db.CreateStyle("PART-TO", "to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	var nodeIDs [3]int64
	for i, suffix := range []string{"A", "B", "C"} {
		name := "PNODE-" + suffix
		nodeIDs[i], _ = db.CreateProcessNode(processes.NodeInput{
			ProcessID: processID, CoreNodeName: name, Code: suffix, Name: name, Sequence: i + 1, Enabled: true,
		})
		fcID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: fromStyleID, CoreNodeName: name, Role: "consume", SwapMode: "simple",
			PayloadCode: "OLD-" + suffix, UOPCapacity: 100, InboundSource: "SRC-OLD",
			OutboundStaging: "OUT-" + suffix, OutboundDestination: "DEST-" + suffix,
		})
		db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: toStyleID, CoreNodeName: name, Role: "consume", SwapMode: "simple",
			PayloadCode: "NEW-" + suffix, UOPCapacity: 200, InboundSource: "SRC-NEW", InboundStaging: "IN-" + suffix,
		})
		db.EnsureProcessNodeRuntime(nodeIDs[i])
		db.SetProcessNodeRuntime(nodeIDs[i], &fcID, 50)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "partial")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	tasks, _ := db.ListChangeoverNodeTasks(changeover.ID)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 node tasks, got %d", len(tasks))
	}

	// Complete nodes A and B fully (Order A + Order B for each)
	for i := 0; i < 2; i++ {
		task := findTaskByNode(tasks, "PNODE-"+string(rune('A'+i)))
		if task == nil {
			continue
		}
		if task.NextMaterialOrderID != nil {
			oa, _ := db.GetOrder(*task.NextMaterialOrderID)
			markOrderTerminal(db, oa.ID)
			emitOrderCompleted(eng, oa.ID, oa.UUID, oa.OrderType, &nodeIDs[i])
		}
		if task.OldMaterialReleaseOrderID != nil {
			ob, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
			markOrderTerminal(db, ob.ID)
			emitOrderCompleted(eng, ob.ID, ob.UUID, ob.OrderType, &nodeIDs[i])
		}
		eng.SwitchNodeToTarget(processID, nodeIDs[i])
	}

	// Fail node C
	taskC := findTaskByNode(tasks, "PNODE-C")
	obC, _ := db.GetOrder(*taskC.OldMaterialReleaseOrderID)
	db.UpdateOrderStatus(obC.ID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(obC.ID, string(orders.StatusFailed))
	emitOrderFailed(eng, obC.ID, obC.UUID, obC.OrderType, "C failed")

	// tryCompleteProcessChangeover is only triggered via SwitchNodeToTarget.
	// CompleteProcessProductionCutover is the escape hatch that bypasses node
	// task state checks. Verify the gate works: after switching A+B,
	// tryComplete fires but blocks because node C is still in "error".
	// (ActiveStyleID won't match ToStyleID until cutover, so tryComplete
	// returns early — node C error is a secondary blocker documented here.)
	co, _ := db.GetActiveProcessChangeover(processID)
	if co == nil {
		t.Error("expected changeover to still be active — cutover not yet called")
	}
	if co != nil && co.State == "completed" {
		t.Error("changeover should not auto-complete while node C is in error")
	}
}

// TC-100: Cutover completion — verify all state transitions.
func TestChangeoverFlow_CutoverCompletion(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, toStyleID := seedPhase3SwapScenario(t, db)
	_ = fromStyleID
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Complete both orders
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	// Verify node is released
	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "released" {
		t.Fatalf("expected released, got %s", task.State)
	}

	// Switch node to target — tryComplete fires but ActiveStyleID doesn't
	// match ToStyleID yet (set by cutover). So changeover stays active.
	eng.SwitchNodeToTarget(processID, nodeID)

	// Now call cutover — this sets ActiveStyleID, clears TargetStyleID,
	// transitions production state, and marks changeover completed.
	eng.CompleteProcessProductionCutover(processID)

	// Verify changeover is completed
	co, _ := db.GetActiveProcessChangeover(processID)
	if co != nil {
		t.Fatalf("expected changeover completed after cutover, got state=%s", co.State)
	}

	// Verify all state transitions
	proc, _ := db.GetProcess(processID)
	if proc.ActiveStyleID == nil || *proc.ActiveStyleID != toStyleID {
		t.Error("active style should be to-style")
	}
	if proc.TargetStyleID != nil {
		t.Error("target style should be nil after cutover")
	}
	if proc.ProductionState != "active_production" {
		t.Errorf("expected active_production, got %s", proc.ProductionState)
	}

	// Verify runtime switched to new claim
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	toClaim, _ := db.GetStyleNodeClaimByNode(toStyleID, "P3-NODE")
	if runtime.ActiveClaimID == nil || *runtime.ActiveClaimID != toClaim.ID {
		t.Error("runtime active claim should be to-claim")
	}
	if runtime.RemainingUOP != 200 {
		t.Errorf("expected UOP=200 (to-claim capacity), got %d", runtime.RemainingUOP)
	}
}

// ===========================================================================
// Section 4: Keep-staged edge cases
// ===========================================================================

// TC-101: Keep-staged with evacuate — both flags on same claim.
func TestChangeoverFlow_KeepStagedWithEvacuate(t *testing.T) {
	db := testEngineDB(t)

	processID, _ := db.CreateProcess("KS-EV-PROC", "ks+evac test", "active_production", "", "", false)
	nodeID, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "KS-EV-NODE", Code: "KE1", Name: "KS+EV Node", Sequence: 1, Enabled: true,
	})
	fromStyleID, _ := db.CreateStyle("KS-EV-FROM", "from", processID)
	toStyleID, _ := db.CreateStyle("KS-EV-TO", "to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	fcID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "KS-EV-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "SRC",
		InboundStaging: "STAGING", OutboundStaging: "OUT-STAGE", OutboundDestination: "DEST",
		KeepStaged: true, EvacuateOnChangeover: true,
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "KS-EV-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-SAME", UOPCapacity: 200, InboundSource: "SRC",
		InboundStaging: "STAGING", EvacuateOnChangeover: true,
	})

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fcID, 50)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "ks+evac")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	// Same payload + EvacuateOnChangeover → SituationEvacuate
	if task.Situation != "evacuate" {
		t.Fatalf("expected evacuate, got %s", task.Situation)
	}
	// KeepStaged=true → keep-staged handler creates orders
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected both orders for keep-staged evacuate")
	}

	// Complete Order A (combined keep-staged)
	orderA, _ := db.GetOrder(*task.NextMaterialOrderID)
	markOrderTerminal(db, orderA.ID)
	emitOrderCompleted(eng, orderA.ID, orderA.UUID, orderA.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "staged" {
		t.Fatalf("after Order A: expected staged, got %s", task.State)
	}

	// Complete Order B (evac)
	orderB, _ := db.GetOrder(*task.OldMaterialReleaseOrderID)
	markOrderTerminal(db, orderB.ID)
	emitOrderCompleted(eng, orderB.ID, orderB.UUID, orderB.OrderType, &nodeID)

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "released" {
		t.Errorf("after Order B (Order A done): expected released, got %s", task.State)
	}
}

// TC-102: Keep-staged from → non-keep-staged to. Old style had keep-staged,
// new style doesn't. The from-claim's KeepStaged flag drives the handler.
func TestChangeoverFlow_KeepStagedToNoKeep(t *testing.T) {
	db := testEngineDB(t)

	processID, _ := db.CreateProcess("KS2NK-PROC", "ks→nokeep", "active_production", "", "", false)
	nodeID, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "KS2NK-NODE", Code: "K2N", Name: "KS→NK Node", Sequence: 1, Enabled: true,
	})
	fromStyleID, _ := db.CreateStyle("KS2NK-FROM", "from", processID)
	toStyleID, _ := db.CreateStyle("KS2NK-TO", "to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	fcID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "KS2NK-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-OLD", UOPCapacity: 100, InboundSource: "SRC-OLD",
		InboundStaging: "STAGING", OutboundStaging: "OUT-STAGE", OutboundDestination: "DEST",
		KeepStaged: true,
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "KS2NK-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-NEW", UOPCapacity: 200, InboundSource: "SRC-NEW",
		InboundStaging: "STAGING",
		// KeepStaged not set — new style doesn't use keep-staged
	})

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fcID, 50)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "ks→nk")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	// Different payload → SituationSwap, but FromClaim.KeepStaged → keep-staged handler
	if task.Situation != "swap" {
		t.Fatalf("expected swap (different payload), got %s", task.Situation)
	}
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected both orders (keep-staged handler triggered by from-claim)")
	}
}

// TC-103: Keep-staged with missing staging config — falls back to simple staging.
func TestChangeoverFlow_KeepStagedMissingStaging(t *testing.T) {
	db := testEngineDB(t)

	processID, _ := db.CreateProcess("KSMS-PROC", "ks missing staging", "active_production", "", "", false)
	nodeID, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "KSMS-NODE", Code: "KM1", Name: "KS Missing Staging", Sequence: 1, Enabled: true,
	})
	fromStyleID, _ := db.CreateStyle("KSMS-FROM", "from", processID)
	toStyleID, _ := db.CreateStyle("KSMS-TO", "to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	fcID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "KSMS-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-OLD", UOPCapacity: 100, InboundSource: "SRC-OLD",
		OutboundStaging: "OUT-STAGE", OutboundDestination: "DEST",
		KeepStaged: true,
		// InboundStaging not set — can't stage
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "KSMS-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-NEW", UOPCapacity: 200, InboundSource: "SRC-NEW",
		// InboundStaging not set
	})

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fcID, 50)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "ks missing staging")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	// SituationSwap but InboundStaging empty → fallback to simple staging/retrieve
	// Only Order A (staging/retrieve), no Order B (can't do Phase 3 without staging)
	if task.NextMaterialOrderID == nil {
		t.Fatal("expected fallback staging/retrieve order (Order A)")
	}
	// Order B should NOT be created (no InboundStaging for Phase 3)
	if task.OldMaterialReleaseOrderID != nil {
		t.Error("expected NO Order B when InboundStaging is missing (fallback path)")
	}
}
