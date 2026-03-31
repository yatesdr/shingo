package engine

import (
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
