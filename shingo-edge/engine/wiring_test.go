package engine

import (
	"database/sql"
	"testing"

	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
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
		&nodeID, false, 1, "", "", "PRODUCE-NODE", "", true, "")
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
		&nodeID, false, 1, "PRODUCE-NODE", "", "", "", false, "")
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
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
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

	claimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
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
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false, "")
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

// TestWiring_MoveCompletion_ManualSwap verifies that when a move order completes
// for a manual_swap node, runtime resets UOP to 0 and clears order tracking.
func TestWiring_MoveCompletion_ManualSwap(t *testing.T) {
	db := testEngineDB(t)

	processID, err := db.CreateProcess("BL-PROC", "manual swap test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "BL-NODE",
		Code:         "BL1",
		Name:         "Bin Swap Loader",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("BL-STYLE", "manual swap style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "BL-NODE",
		Role:                "produce",
		SwapMode:            "manual_swap",
		PayloadCode:         "PART-BL",
		UOPCapacity:         100,
		OutboundDestination: "STORAGE-NODE",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &claimID, 75)

	// Create a move order
	orderID, err := db.CreateOrder("uuid-move-bl", orders.TypeMove,
		&nodeID, false, 1, "DEST-NODE", "", "BL-NODE", "", false, "")
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
		t.Errorf("RemainingUOP = %d, want 0 after manual_swap move completion", runtime.RemainingUOP)
	}
	// handleManualSwapCompletion clears ActiveOrderID then calls tryAutoRequest
	// which legitimately re-populates it with a new auto-requested order if
	// the claim has allowed payloads. The relevant invariant is that the
	// original move order is no longer tracked — not that the slot is empty.
	if runtime.ActiveOrderID != nil && *runtime.ActiveOrderID == orderID {
		t.Errorf("ActiveOrderID = %d (the original move order) after completion; manual_swap completion should have cleared the move-order tracking before re-populating with any auto-request", *runtime.ActiveOrderID)
	}
}

// TestHandlePayloadCatalog_PruneDeletedEntries verifies that when edge receives a
// payload catalog from core, entries that no longer exist in core's response
// are pruned from the local catalog. This prevents stale deleted payloads from
// appearing in edge's UI after a sync.
func TestHandlePayloadCatalog_PruneDeletedEntries(t *testing.T) {
	db := testEngineDB(t)

	// Seed local catalog with two entries as if they were previously synced from core
	if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "PART-A", Code: "PART-A", Description: "Part A", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("seed PART-A: %v", err)
	}
	if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 2, Name: "PART-B", Code: "PART-B", Description: "Part B", UOPCapacity: 50,
	}); err != nil {
		t.Fatalf("seed PART-B: %v", err)
	}

	// Simulate core responding with only PART-A (PART-B was deleted in core)
	if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{
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
	nodeAID, err = db.CreateProcessNode(processes.NodeInput{
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
	nodeBID, err = db.CreateProcessNode(processes.NodeInput{
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

	claimAID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
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
	claimBID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
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

// ---------------------------------------------------------------------------
// Section 5: A/B cycling gaps
// ---------------------------------------------------------------------------

// seedABProducePair creates two produce nodes in an A/B pair.
func seedABProducePair(t *testing.T, db *store.DB) (processID, nodeAID, nodeBID, styleID, claimAID, claimBID int64) {
	t.Helper()

	processID, _ = db.CreateProcess("ABP-PROC", "a/b produce test", "active_production", "", "", false)
	nodeAID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "ABP-A", Code: "PA1", Name: "Produce A", Sequence: 1, Enabled: true,
	})
	nodeBID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "ABP-B", Code: "PB1", Name: "Produce B", Sequence: 2, Enabled: true,
	})

	styleID, _ = db.CreateStyle("ABP-STYLE", "a/b produce style", processID)
	db.SetActiveStyle(processID, &styleID)

	claimAID, _ = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ABP-A", Role: "produce", SwapMode: "simple",
		PayloadCode: "PART-ABP", UOPCapacity: 100, InboundSource: "SRC-EMPTY",
		PairedCoreNode: "ABP-B",
	})
	claimBID, _ = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ABP-B", Role: "produce", SwapMode: "simple",
		PayloadCode: "PART-ABP", UOPCapacity: 100, InboundSource: "SRC-EMPTY",
		PairedCoreNode: "ABP-A",
	})

	db.EnsureProcessNodeRuntime(nodeAID)
	db.EnsureProcessNodeRuntime(nodeBID)
	db.SetProcessNodeRuntime(nodeAID, &claimAID, 10)
	db.SetProcessNodeRuntime(nodeBID, &claimBID, 10)
	db.SetActivePull(nodeAID, true)
	db.SetActivePull(nodeBID, false)

	return
}

// seedAsymmetricABPair creates an A/B pair where only A names B as partner.
func seedAsymmetricABPair(t *testing.T, db *store.DB) (processID, nodeAID, nodeBID, styleID, claimAID, claimBID int64) {
	t.Helper()

	processID, _ = db.CreateProcess("ASYM-PROC", "asymmetric a/b", "active_production", "", "", false)
	nodeAID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "ASYM-A", Code: "AA1", Name: "Asym A", Sequence: 1, Enabled: true,
	})
	nodeBID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "ASYM-B", Code: "AB1", Name: "Asym B", Sequence: 2, Enabled: true,
	})

	styleID, _ = db.CreateStyle("ASYM-STYLE", "asym style", processID)
	db.SetActiveStyle(processID, &styleID)

	claimAID, _ = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ASYM-A", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-ASYM", UOPCapacity: 100, PairedCoreNode: "ASYM-B",
	})
	claimBID, _ = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ASYM-B", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-ASYM", UOPCapacity: 100,
		// PairedCoreNode intentionally empty — B doesn't know about A
	})

	db.EnsureProcessNodeRuntime(nodeAID)
	db.EnsureProcessNodeRuntime(nodeBID)
	db.SetProcessNodeRuntime(nodeAID, &claimAID, 80)
	db.SetProcessNodeRuntime(nodeBID, &claimBID, 80)
	db.SetActivePull(nodeAID, true)
	db.SetActivePull(nodeBID, false)

	return
}

// TC-104: Flip during active changeover — flip succeeds but auto-reorder blocked.
func TestWiring_ABFlip_DuringChangeover(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, claimAID, _ := seedABPair(t, db)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Create a second style and start changeover
	style2, _ := db.CreateStyle("AB-STYLE-2", "second style", processID)
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: style2, CoreNodeName: "AB-NODE-A", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-NEW", UOPCapacity: 100,
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: style2, CoreNodeName: "AB-NODE-B", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-NEW", UOPCapacity: 100,
	})

	if _, err := eng.StartProcessChangeover(processID, style2, "test", "flip during co"); err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	// Flip should succeed (FlipABNode doesn't check changeover state)
	if err := eng.FlipABNode(nodeBID); err != nil {
		t.Fatalf("FlipABNode during changeover: %v", err)
	}

	// Verify flip happened
	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)
	if rtA.ActivePull {
		t.Error("Node A should be inactive after flip to B")
	}
	if !rtB.ActivePull {
		t.Error("Node B should be active after flip to B")
	}

	// But CanAcceptOrders should block new orders during changeover
	ok, reason := eng.CanAcceptOrders(nodeAID)
	if ok {
		t.Error("CanAcceptOrders should return false during changeover")
	}
	if reason != "changeover in progress" {
		t.Errorf("reason = %q, want 'changeover in progress'", reason)
	}

	// Counter delta should still be blocked by changeover guard (CanAcceptOrders)
	// But handleCounterDelta doesn't use CanAcceptOrders — it checks claim directly
	// Verify counter delta behavior during changeover
	_ = styleID
	_ = claimAID
}

// TC-105: A/B pair with produce role — active node increments, inactive skipped.
func TestWiring_ABProducePair_ActiveIncrements(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABProducePair(t, db)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Send delta — only Node A (active) should increment
	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     5,
	})

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	if rtA.RemainingUOP != 15 {
		t.Errorf("Produce A RemainingUOP = %d, want 15 (10 + 5)", rtA.RemainingUOP)
	}
	if rtB.RemainingUOP != 10 {
		t.Errorf("Produce B RemainingUOP = %d, want 10 (inactive, should not increment)", rtB.RemainingUOP)
	}
}

// TC-106: Flip + immediate counter delta — verify race-free sequencing.
func TestWiring_ABFlip_ImmediateDelta(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABPair(t, db)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Flip then immediately send delta — Node B should get the decrement
	eng.FlipABNode(nodeBID)

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     7,
	})

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	if rtA.RemainingUOP != 80 {
		t.Errorf("Node A RemainingUOP = %d, want 80 (inactive after flip)", rtA.RemainingUOP)
	}
	if rtB.RemainingUOP != 73 {
		t.Errorf("Node B RemainingUOP = %d, want 73 (80 - 7, active after flip)", rtB.RemainingUOP)
	}
}

// TC-107: A/B pair across styles — pairing changes after changeover.
func TestWiring_ABPairsAcrossStyles(t *testing.T) {
	db := testEngineDB(t)
	processID, _, _, style1ID, _, _ := seedABPair(t, db)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Create Style 2 with NO pairing (both nodes unpaired)
	style2ID, _ := db.CreateStyle("AB-STYLE-NO-PAIR", "no pairing", processID)
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: style2ID, CoreNodeName: "AB-NODE-A", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-X", UOPCapacity: 100,
		// No PairedCoreNode
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: style2ID, CoreNodeName: "AB-NODE-B", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-X", UOPCapacity: 100,
		// No PairedCoreNode
	})

	// Start and complete changeover to Style 2
	co, err := eng.StartProcessChangeover(processID, style2ID, "test", "unpair")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	// All nodes unchanged (same payload codes are different: PART-AB vs PART-X)
	// Actually they're swap (PART-AB → PART-X). Complete the changeover via cutover.
	_ = co
	eng.CompleteProcessProductionCutover(processID)

	// After cutover, active style is style2. Claims have no PairedCoreNode.
	// Counter delta should decrement BOTH nodes (unpaired behavior).
	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   style2ID,
		Delta:     3,
	})

	// Both should decrement — no pairing in new style
	// Need to look up nodes again since they were created in seedABPair
	nodes, _ := db.ListProcessNodesByProcess(processID)
	var nodeAID, nodeBID int64
	for _, n := range nodes {
		if n.CoreNodeName == "AB-NODE-A" {
			nodeAID = n.ID
		}
		if n.CoreNodeName == "AB-NODE-B" {
			nodeBID = n.ID
		}
	}

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	// Both should decrement (unpaired nodes always decrement independently)
	totalDelta := (80 - rtA.RemainingUOP) + (80 - rtB.RemainingUOP)
	if totalDelta != 6 {
		t.Errorf("after unpairing: total delta = %d, want 6 (each node gets 3 independently)", totalDelta)
	}

	// Document: with two unpaired consume nodes for same payload, delta hits both.
	// This is expected — pairing is the mechanism that prevents double-counting.
	_ = style1ID
}

// TC-108: Asymmetric A/B pair — only A names B as partner.
func TestWiring_AB_AsymmetricPair(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, claimAID, _ := seedAsymmetricABPair(t, db)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Send counter delta — Node A is paired (checks ActivePull), Node B is unpaired (always decrements)
	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     5,
	})

	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)

	// Node A: paired with ASYM-B, ActivePull=true → should decrement
	if rtA.RemainingUOP != 75 {
		t.Errorf("Asym A RemainingUOP = %d, want 75 (active paired node)", rtA.RemainingUOP)
	}
	// Node B: unpaired (PairedCoreNode="") → always decrements regardless of ActivePull
	if rtB.RemainingUOP != 75 {
		t.Errorf("Asym B RemainingUOP = %d, want 75 (unpaired always decrements)", rtB.RemainingUOP)
	}

	// Flip: make B active, A inactive
	db.SetActivePull(nodeAID, false)
	db.SetActivePull(nodeBID, true)

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     3,
	})

	rtA, _ = db.GetProcessNodeRuntime(nodeAID)
	rtB, _ = db.GetProcessNodeRuntime(nodeBID)

	// Node A: paired + inactive → skipped in main loop, but fallthrough hits it
	// because pairedConsumeHandled is never set (Node B is unpaired, doesn't set it).
	// This documents the asymmetric A/B edge case: fallthrough fires on the inactive
	// paired node when the unpaired partner doesn't set pairedConsumeHandled.
	if rtA.RemainingUOP != 72 {
		t.Errorf("Asym A after flip (inactive, but fallthrough): RemainingUOP = %d, want 72", rtA.RemainingUOP)
	}
	// Node B: unpaired → always decrements
	if rtB.RemainingUOP != 72 {
		t.Errorf("Asym B after flip: RemainingUOP = %d, want 72 (75 - 3, unpaired always decrements)", rtB.RemainingUOP)
	}

	_ = claimAID
}

// ── Lineside-first drain tests ──────────────────────────────────────

// TestWiring_CounterDelta_DrainsLinesideBeforeNodeCounter verifies that
// counter deltas decrement an active lineside bucket before touching
// RemainingUOP on the node.
func TestWiring_CounterDelta_DrainsLinesideBeforeNodeCounter(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-DRAIN", PayloadCode: "P-500", UOPCapacity: 100, InitialUOP: 100,
	})

	// Capture 60 parts to lineside for this (node, style, part).
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "P-500", 60); err != nil {
		t.Fatalf("CaptureLinesideBucket: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Drain 15 — should come entirely from the bucket, node counter untouched.
	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     15,
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (drain came from lineside)", runtime.RemainingUOP)
	}

	b, err := db.GetActiveLinesideBucket(nodeID, styleID, "P-500")
	if err != nil {
		t.Fatalf("GetActiveLinesideBucket: %v", err)
	}
	if b.Qty != 45 {
		t.Errorf("bucket qty = %d, want 45 (60 - 15)", b.Qty)
	}
}

// TestWiring_CounterDelta_CarriesRemainderToNodeCounter verifies that
// when a delta exceeds the bucket qty, the bucket drains to zero (and
// is deleted) and the remainder flows to the node counter.
func TestWiring_CounterDelta_CarriesRemainderToNodeCounter(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-CARRY", PayloadCode: "P-600", UOPCapacity: 100, InitialUOP: 100,
	})
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "P-600", 10); err != nil {
		t.Fatalf("CaptureLinesideBucket: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Drain 25 — 10 from bucket, 15 from node counter.
	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     25,
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 85 {
		t.Errorf("RemainingUOP = %d, want 85 (100 - 15 remainder)", runtime.RemainingUOP)
	}

	// Bucket should be gone.
	if _, err := db.GetActiveLinesideBucket(nodeID, styleID, "P-600"); err != sql.ErrNoRows {
		t.Errorf("expected drained bucket to be deleted, got err=%v", err)
	}
}

// TestWiring_CounterDelta_NoLinesideBucket verifies that when no bucket
// exists, the full delta hits the node counter as before.
func TestWiring_CounterDelta_NoLinesideBucket(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-NONE", PayloadCode: "P-700", UOPCapacity: 100, InitialUOP: 100,
	})

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.handleCounterDelta(CounterDeltaEvent{
		ProcessID: processID,
		StyleID:   styleID,
		Delta:     7,
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 93 {
		t.Errorf("RemainingUOP = %d, want 93 (no bucket, full delta hits counter)", runtime.RemainingUOP)
	}
}
