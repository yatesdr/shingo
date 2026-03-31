package engine

import (
	"testing"

	"shingoedge/orders"
	"shingoedge/store"

	"shingoedge/config"
)

// testOrderEmitter is a no-op implementation of orders.EventEmitter for testing.
type testOrderEmitter struct{}

func (testOrderEmitter) EmitOrderCreated(int64, string, string, *int64, *int64)                       {}
func (testOrderEmitter) EmitOrderStatusChanged(int64, string, string, string, string, string, *int64, *int64) {}
func (testOrderEmitter) EmitOrderCompleted(int64, string, string, *int64, *int64)                     {}
func (testOrderEmitter) EmitOrderFailed(int64, string, string, string)                                {}

// seedProduceNode creates a process, process node, style, claim, and runtime
// suitable for produce-node finalization tests. Returns all the IDs needed.
func seedProduceNode(t *testing.T, db *store.DB, swapMode string) (processID, nodeID, styleID, claimID int64) {
	t.Helper()

	processID, err := db.CreateProcess("PRODUCE-PROC", "produce test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(store.ProcessNodeInput{
		ProcessID:    processID,
		CoreNodeName: "PRODUCE-NODE",
		Code:         "PN1",
		Name:         "Produce Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}

	styleID, err = db.CreateStyle("PROD-STYLE", "produce style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}

	claimID, err = db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "PRODUCE-NODE",
		Role:                "produce",
		SwapMode:            swapMode,
		PayloadCode:         "WIDGET-A",
		UOPCapacity:         100,
		AutoReorder:         true,
		InboundSource:       "EMPTY-STORAGE",
		InboundStaging:      "PRODUCE-IN-STAGING",
		OutboundStaging:     "PRODUCE-OUT-STAGING",
		OutboundDestination: "FILLED-STORAGE",
		AutoRequestPayload:  "WIDGET-A",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	// Initialize runtime with some UOP (50 parts produced)
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	cID := claimID
	if err := db.SetProcessNodeRuntime(nodeID, &cID, 50); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	return processID, nodeID, styleID, claimID
}

// testEngine creates a minimal Engine with a real order manager backed by the
// given SQLite DB. The engine is suitable for testing FinalizeProduceNode and
// wiring handlers. No PLC manager or network services are created.
func testEngine(t *testing.T, db *store.DB) *Engine {
	t.Helper()
	cfg := &config.Config{
		Namespace: "test",
		LineID:    "test-line",
		Web:       config.WebConfig{AutoConfirm: true},
	}
	eng := &Engine{
		cfg:      cfg,
		db:       db,
		orderMgr: orders.NewManager(db, testOrderEmitter{}, "test.station"),
		Events:   NewEventBus(),
		stopChan: make(chan struct{}),
	}
	eng.hourlyTracker = NewHourlyTracker(db, "")
	return eng
}

func TestProduceSimple_FinalizeIngest(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "simple")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	if result.CycleMode != "simple" {
		t.Errorf("CycleMode = %q, want %q", result.CycleMode, "simple")
	}
	if result.Order == nil {
		t.Fatal("expected an ingest order")
	}
	if result.Order.OrderType != orders.TypeIngest {
		t.Errorf("OrderType = %q, want %q", result.Order.OrderType, orders.TypeIngest)
	}
	if result.ProcessNodeID != nodeID {
		t.Errorf("ProcessNodeID = %d, want %d", result.ProcessNodeID, nodeID)
	}

	// Runtime should be reset to UOP=0
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after finalize", runtime.RemainingUOP)
	}
	// Active order should be set
	if runtime.ActiveOrderID == nil {
		t.Error("ActiveOrderID should be set after finalize")
	}
}

func TestProduceSequential_RemovalThenBackfill(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "sequential")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	if result.CycleMode != "sequential" {
		t.Errorf("CycleMode = %q, want %q", result.CycleMode, "sequential")
	}
	if result.Order == nil {
		t.Fatal("expected a complex removal order")
	}
	if result.Order.OrderType != orders.TypeComplex {
		t.Errorf("OrderType = %q, want %q", result.Order.OrderType, orders.TypeComplex)
	}

	// Runtime should be reset to UOP=0
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after finalize", runtime.RemainingUOP)
	}
	// Active order should be the complex removal order (Order A)
	if runtime.ActiveOrderID == nil || *runtime.ActiveOrderID != result.Order.ID {
		t.Error("ActiveOrderID should match the removal order")
	}

	// An ingest order should also have been created (before the complex order)
	allOrders, err := db.ListOrders()
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	hasIngest := false
	for _, o := range allOrders {
		if o.OrderType == orders.TypeIngest {
			hasIngest = true
			break
		}
	}
	if !hasIngest {
		t.Error("expected an ingest order to manifest the filled bin")
	}
}

func TestProduceSingleRobot_TenStepSwap(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "single_robot")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	if result.CycleMode != "single_robot" {
		t.Errorf("CycleMode = %q, want %q", result.CycleMode, "single_robot")
	}
	if result.Order == nil {
		t.Fatal("expected a complex swap order")
	}
	if result.Order.OrderType != orders.TypeComplex {
		t.Errorf("OrderType = %q, want %q", result.Order.OrderType, orders.TypeComplex)
	}

	// Runtime should be reset to UOP=0
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after finalize", runtime.RemainingUOP)
	}
}

func TestProduceTwoRobot_BothOrdersCreated(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	if result.CycleMode != "two_robot" {
		t.Errorf("CycleMode = %q, want %q", result.CycleMode, "two_robot")
	}
	if result.OrderA == nil {
		t.Fatal("expected OrderA (fetch-and-stage)")
	}
	if result.OrderB == nil {
		t.Fatal("expected OrderB (remove filled)")
	}
	if result.OrderA.OrderType != orders.TypeComplex {
		t.Errorf("OrderA type = %q, want %q", result.OrderA.OrderType, orders.TypeComplex)
	}
	if result.OrderB.OrderType != orders.TypeComplex {
		t.Errorf("OrderB type = %q, want %q", result.OrderB.OrderType, orders.TypeComplex)
	}
	if result.OrderA.ID == result.OrderB.ID {
		t.Error("OrderA and OrderB should be different orders")
	}

	// Runtime: both order IDs tracked
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.ActiveOrderID == nil || *runtime.ActiveOrderID != result.OrderA.ID {
		t.Error("ActiveOrderID should be OrderA")
	}
	if runtime.StagedOrderID == nil || *runtime.StagedOrderID != result.OrderB.ID {
		t.Error("StagedOrderID should be OrderB")
	}
	if runtime.RemainingUOP != 0 {
		t.Errorf("RemainingUOP = %d, want 0", runtime.RemainingUOP)
	}
}

func TestProduceFinalize_RejectsZeroUOP(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedProduceNode(t, db, "simple")
	eng := testEngine(t, db)

	// Set UOP to 0 — nothing to finalize
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 0); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	_, err := eng.FinalizeProduceNode(nodeID)
	if err == nil {
		t.Fatal("expected error when RemainingUOP is 0")
	}
}

func TestProduceFinalize_RejectsConsumeNode(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "simple")
	eng := testEngine(t, db)

	// Override claim to consume role
	_, err := db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: "PRODUCE-NODE",
		Role:         "consume",
		SwapMode:     "simple",
		PayloadCode:  "WIDGET-A",
		UOPCapacity:  100,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	_ = processID

	_, err = eng.FinalizeProduceNode(nodeID)
	if err == nil {
		t.Fatal("expected error for consume node")
	}
}
