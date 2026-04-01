package engine

import (
	"database/sql"
	"testing"

	"shingoedge/orders"
	"shingoedge/store"
)

// TestWiring_IngestCompletion_ResetsProduceUOP verifies that when an ingest
// order completes for a produce node, the runtime UOP is reset to 0, order
// IDs are cleared, and the node is ready for the next empty bin.
func TestWiring_IngestCompletion_ResetsProduceUOP(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedProduceNode(t, db, "simple")
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Create an ingest order as if FinalizeProduceNode had been called
	orderID, err := db.CreateOrder("uuid-ingest-1", orders.TypeIngest,
		&nodeID, false, 1, "", "", "PRODUCE-NODE", "", true)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusSubmitted)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
	db.SetProcessNodeRuntime(nodeID, &claimID, 50)

	// Simulate order completion
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-ingest-1",
			OrderType:     orders.TypeIngest,
			ProcessNodeID: &nodeID,
		},
	})

	// Give event handler a moment to process (synchronous in single goroutine)
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after ingest completion", runtime.RemainingUOP)
	}
	if runtime.ActiveOrderID != nil {
		t.Error("ActiveOrderID should be nil after ingest completion")
	}
	if runtime.StagedOrderID != nil {
		t.Error("StagedOrderID should be nil after ingest completion")
	}
}

// TestWiring_RetrieveCompletion_ProduceResetsToZero verifies that when a
// retrieve/complex order completes for a produce node, UOP resets to 0
// (not to capacity, which is the consume-node behavior).
func TestWiring_RetrieveCompletion_ProduceResetsToZero(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedProduceNode(t, db, "simple")
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Create a retrieve order (empty bin delivery to produce node)
	orderID, err := db.CreateOrder("uuid-retrieve-prod", orders.TypeRetrieve,
		&nodeID, false, 1, "PRODUCE-NODE", "", "", "", false)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusSubmitted)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
	db.SetProcessNodeRuntime(nodeID, &claimID, 0) // was at 0, waiting for empty

	// Simulate order completion
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-retrieve-prod",
			OrderType:     orders.TypeRetrieve,
			ProcessNodeID: &nodeID,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	// Produce node: UOP should reset to 0 (empty bin received, starts at 0)
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 (produce node receives empty bin)", runtime.RemainingUOP)
	}
}

// consumeNodeConfig holds the configurable parameters for seedConsumeNode.
type consumeNodeConfig struct {
	Prefix       string // unique prefix for names (e.g. "CONSUME", "DELTA-CON")
	PayloadCode  string
	UOPCapacity  int
	InitialUOP   int
}

// seedConsumeNode creates a consume-role process, node, style, claim, and runtime.
// Returns processID, nodeID, styleID, claimID.
func seedConsumeNode(t *testing.T, db *store.DB, cfg consumeNodeConfig) (processID, nodeID, styleID, claimID int64) {
	t.Helper()
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "CONSUME"
	}

	processID, err := db.CreateProcess(prefix+"-PROC", prefix+" test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(store.ProcessNodeInput{
		ProcessID:    processID,
		CoreNodeName: prefix + "-NODE",
		Code:         prefix[:3],
		Name:         prefix + " Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err = db.CreateStyle(prefix+"-STYLE", prefix+" style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	claimID, err = db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: prefix + "-NODE",
		Role:         "consume",
		SwapMode:     "simple",
		PayloadCode:  cfg.PayloadCode,
		UOPCapacity:  cfg.UOPCapacity,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &claimID, cfg.InitialUOP)
	return processID, nodeID, styleID, claimID
}

// TestWiring_RetrieveCompletion_ConsumeResetsToCapacity verifies that when a
// retrieve/complex order completes for a consume node, UOP resets to the
// claim's UOPCapacity (full bin received).
func TestWiring_RetrieveCompletion_ConsumeResetsToCapacity(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "CONSUME", PayloadCode: "PART-X", UOPCapacity: 200, InitialUOP: 10,
	})

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Create a retrieve order (full bin delivery to consume node)
	orderID, err := db.CreateOrder("uuid-retrieve-con", orders.TypeRetrieve,
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusSubmitted)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-retrieve-con",
			OrderType:     orders.TypeRetrieve,
			ProcessNodeID: &nodeID,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	// Consume node: UOP should reset to capacity (full bin received)
	if runtime.RemainingUOP != 200 {
		t.Errorf("RemainingUOP = %d, want 200 (consume node UOPCapacity)", runtime.RemainingUOP)
	}
	_ = processID
}

// TestWiring_CounterDelta_ProduceIncrementsUOP verifies that counter delta
// events increment UOP for produce nodes (counting UP toward capacity).
func TestWiring_CounterDelta_ProduceIncrementsUOP(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedProduceNode(t, db, "simple")

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Set initial UOP
	db.SetProcessNodeRuntime(nodeID, &claimID, 10)

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     5,
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 15 {
		t.Errorf("RemainingUOP = %d, want 15 (10 + 5 delta)", runtime.RemainingUOP)
	}
}

// TestWiring_CounterDelta_ConsumeDecrementsUOP verifies that counter delta
// events decrement UOP for consume nodes (counting DOWN from capacity).
func TestWiring_CounterDelta_ConsumeDecrementsUOP(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "DELTA-CON", PayloadCode: "PART-Y", UOPCapacity: 100, InitialUOP: 80,
	})

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     3,
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 77 {
		t.Errorf("RemainingUOP = %d, want 77 (80 - 3 delta)", runtime.RemainingUOP)
	}
}

// TestWiring_CounterDelta_ConsumeFloorsAtZero verifies that consume node UOP
// never goes negative when delta exceeds remaining.
func TestWiring_CounterDelta_ConsumeFloorsAtZero(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "FLOOR", PayloadCode: "PART-Z", UOPCapacity: 50, InitialUOP: 2,
	})

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     10, // way more than remaining
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 (floored at zero)", runtime.RemainingUOP)
	}
}

// TestWiring_MoveCompletion_BinLoader verifies that when a move order completes
// for a bin_loader node, runtime resets UOP to 0 and clears order tracking.
func TestWiring_MoveCompletion_BinLoader(t *testing.T) {
	db := testEngineDB(t)

	processID, err := db.CreateProcess("BL-PROC", "bin loader test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(store.ProcessNodeInput{
		ProcessID:    processID,
		CoreNodeName: "BL-NODE",
		Code:         "BL1",
		Name:         "Bin Loader",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("BL-STYLE", "bin loader style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	claimID, err := db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: "BL-NODE",
		Role:         "bin_loader",
		SwapMode:     "simple",
		PayloadCode:  "PART-BL",
		UOPCapacity:  100,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &claimID, 75)

	// Create a move order
	orderID, err := db.CreateOrder("uuid-move-bl", orders.TypeMove,
		&nodeID, false, 1, "DEST-NODE", "", "BL-NODE", "", false)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusSubmitted)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-move-bl",
			OrderType:     orders.TypeMove,
			ProcessNodeID: &nodeID,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after bin_loader move completion", runtime.RemainingUOP)
	}
	if runtime.ActiveOrderID != nil {
		t.Error("ActiveOrderID should be nil after bin_loader move completion")
	}
}

// TestHandlePayloadCatalog_PruneDeletedEntries verifies that when edge receives a
// payload catalog from core, entries that no longer exist in core's response
// are pruned from the local catalog. This prevents stale deleted payloads from
// appearing in edge's UI after a sync.
func TestHandlePayloadCatalog_PruneDeletedEntries(t *testing.T) {
	db := testEngineDB(t)

	// Seed local catalog with two entries as if they were previously synced from core
	if err := db.UpsertPayloadCatalog(&store.PayloadCatalogEntry{
		ID: 1, Name: "PART-A", Code: "PART-A", Description: "Part A", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("seed PART-A: %v", err)
	}
	if err := db.UpsertPayloadCatalog(&store.PayloadCatalogEntry{
		ID: 2, Name: "PART-B", Code: "PART-B", Description: "Part B", UOPCapacity: 50,
	}); err != nil {
		t.Fatalf("seed PART-B: %v", err)
	}

	// Simulate core responding with only PART-A (PART-B was deleted in core)
	if err := db.UpsertPayloadCatalog(&store.PayloadCatalogEntry{
		ID: 1, Name: "PART-A", Code: "PART-A", Description: "Part A", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("upsert active PART-A: %v", err)
	}

	// Prune entries not in core's active set
	activeIDs := []int64{1}
	if err := db.DeleteStalePayloadCatalogEntries(activeIDs); err != nil {
		t.Fatalf("prune stale entries: %v", err)
	}

	// Verify PART-A still exists
	entryA, err := db.GetPayloadCatalogByCode("PART-A")
	if err != nil {
		t.Fatalf("PART-A should still exist: %v", err)
	}
	if entryA.Code != "PART-A" {
		t.Errorf("PART-A code = %q, want PART-A", entryA.Code)
	}

	// Verify PART-B was pruned (deleted in core, should be removed locally)
	entryB, err := db.GetPayloadCatalogByCode("PART-B")
	if entryB == nil && err == sql.ErrNoRows {
		// good - entry was pruned
	} else if entryB != nil {
		t.Errorf("PART-B should have been pruned from local catalog after core sync (core deleted it)")
	}

	// Verify only one entry remains
	entries, _ := db.ListPayloadCatalog()
	if len(entries) != 1 {
		t.Errorf("catalog entries = %d, want 1 (only PART-A)", len(entries))
	}
}

// ---------------------------------------------------------------------------
// A/B cycling helpers + tests
// ---------------------------------------------------------------------------

// seedABPair creates a process with two consume nodes (A and B) paired via
// PairedCoreNode. Returns processID, nodeAID, nodeBID, styleID, claimAID, claimBID.
func seedABPair(t *testing.T, db *store.DB) (processID, nodeAID, nodeBID, styleID, claimAID, claimBID int64) {
	t.Helper()

	processID, err := db.CreateProcess("AB-PROC", "a/b cycling test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeAID, err = db.CreateProcessNode(store.ProcessNodeInput{
		ProcessID:    processID,
		CoreNodeName: "AB-NODE-A",
		Code:         "ABA",
		Name:         "AB Node A",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node A: %v", err)
	}
	nodeBID, err = db.CreateProcessNode(store.ProcessNodeInput{
		ProcessID:    processID,
		CoreNodeName: "AB-NODE-B",
		Code:         "ABB",
		Name:         "AB Node B",
		Sequence:     2,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node B: %v", err)
	}

	styleID, err = db.CreateStyle("AB-STYLE", "a/b style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	claimAID, err = db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:        styleID,
		CoreNodeName:   "AB-NODE-A",
		Role:           "consume",
		SwapMode:       "simple",
		PayloadCode:    "PART-AB",
		UOPCapacity:    100,
		ReorderPoint:   10,
		AutoReorder:    true,
		PairedCoreNode: "AB-NODE-B",
	})
	if err != nil {
		t.Fatalf("upsert claim A: %v", err)
	}
	claimBID, err = db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:        styleID,
		CoreNodeName:   "AB-NODE-B",
		Role:           "consume",
		SwapMode:       "simple",
		PayloadCode:    "PART-AB",
		UOPCapacity:    100,
		ReorderPoint:   10,
		AutoReorder:    true,
		PairedCoreNode: "AB-NODE-A",
	})
	if err != nil {
		t.Fatalf("upsert claim B: %v", err)
	}

	db.EnsureProcessNodeRuntime(nodeAID)
	db.EnsureProcessNodeRuntime(nodeBID)
	db.SetProcessNodeRuntime(nodeAID, &claimAID, 80)
	db.SetProcessNodeRuntime(nodeBID, &claimBID, 80)

	// Node A starts as active pull, Node B starts as inactive
	db.SetActivePull(nodeAID, true)
	db.SetActivePull(nodeBID, false)

	return
}

// TestWiring_ABCycling_ActiveNodeDecrements verifies that counter delta only
// decrements the active-pull node in an A/B pair.
func TestWiring_ABCycling_ActiveNodeDecrements(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABPair(t, db)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Send a delta — only Node A (active) should decrement
	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     5,
	})

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	if rtA.RemainingUOP != 75 {
		t.Errorf("Node A RemainingUOP = %d, want 75 (80 - 5)", rtA.RemainingUOP)
	}
	if rtB.RemainingUOP != 80 {
		t.Errorf("Node B RemainingUOP = %d, want 80 (inactive, should not decrement)", rtB.RemainingUOP)
	}
}

// TestWiring_ABCycling_InactiveNodeSkipped verifies that the inactive side of
// an A/B pair is NOT decremented by counter deltas.
func TestWiring_ABCycling_InactiveNodeSkipped(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABPair(t, db)

	// Flip: B active, A inactive
	db.SetActivePull(nodeAID, false)
	db.SetActivePull(nodeBID, true)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     10,
	})

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	if rtA.RemainingUOP != 80 {
		t.Errorf("Node A RemainingUOP = %d, want 80 (inactive, should not decrement)", rtA.RemainingUOP)
	}
	if rtB.RemainingUOP != 70 {
		t.Errorf("Node B RemainingUOP = %d, want 70 (80 - 10)", rtB.RemainingUOP)
	}
}

// TestWiring_ABCycling_FallthroughBothInactive verifies that when NEITHER A
// nor B has active_pull=true, the fallthrough logic still decrements one of
// them (the "count to lineside storage" safety net).
func TestWiring_ABCycling_FallthroughBothInactive(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABPair(t, db)

	// Set both inactive — shouldn't happen normally, but defensive
	db.SetActivePull(nodeAID, false)
	db.SetActivePull(nodeBID, false)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     7,
	})

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	// One of them should have been decremented as fallback
	totalRemaining := rtA.RemainingUOP + rtB.RemainingUOP
	if totalRemaining != 153 { // 80 + 80 - 7 = 153
		t.Errorf("total remaining = %d, want 153 (one node decremented by 7 as fallthrough)", totalRemaining)
	}
}

// TestWiring_FlipABNode_SwitchesActivePull verifies that FlipABNode correctly
// sets active_pull=true on the target and active_pull=false on the partner.
func TestWiring_FlipABNode_SwitchesActivePull(t *testing.T) {
	db := testEngineDB(t)
	_, nodeAID, nodeBID, _, _, _ := seedABPair(t, db)

	eng := testEngine(t, db)

	// Initially A=active, B=inactive. Flip to B.
	if err := eng.FlipABNode(nodeBID); err != nil {
		t.Fatalf("FlipABNode to B: %v", err)
	}

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	if rtA.ActivePull {
		t.Error("Node A should be inactive after flip to B")
	}
	if !rtB.ActivePull {
		t.Error("Node B should be active after flip to B")
	}

	// Flip back to A
	if err := eng.FlipABNode(nodeAID); err != nil {
		t.Fatalf("FlipABNode to A: %v", err)
	}

	rtA, _ = db.GetProcessNodeRuntime(nodeAID)
	rtB, _ = db.GetProcessNodeRuntime(nodeBID)

	if !rtA.ActivePull {
		t.Error("Node A should be active after flip back to A")
	}
	if rtB.ActivePull {
		t.Error("Node B should be inactive after flip back to A")
	}
}

// TestWiring_FlipABNode_RejectsUnpairedNode verifies that FlipABNode returns
// an error when called on a node without PairedCoreNode.
func TestWiring_FlipABNode_RejectsUnpairedNode(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "NOPAIR", PayloadCode: "PART-NP", UOPCapacity: 100, InitialUOP: 50,
	})

	eng := testEngine(t, db)

	err := eng.FlipABNode(nodeID)
	if err == nil {
		t.Fatal("FlipABNode should reject a node without PairedCoreNode")
	}
}

// TestWiring_ABCycling_UnpairedNodeAlwaysDecrements verifies that unpaired
// consume nodes (PairedCoreNode="") always decrement regardless of active_pull,
// maintaining backward compatibility.
func TestWiring_ABCycling_UnpairedNodeAlwaysDecrements(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "UNPAIR", PayloadCode: "PART-UP", UOPCapacity: 100, InitialUOP: 60,
	})

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     4,
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 56 {
		t.Errorf("RemainingUOP = %d, want 56 (unpaired node always decrements)", runtime.RemainingUOP)
	}
}
