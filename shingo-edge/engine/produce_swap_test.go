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
		// logFn is initialized to a no-op for tests that exercise diagnostic
		// logging paths (e.g. ReleaseOrderWithLineside's toClaim==nil case,
		// releaseIfStaged's pre-staged tolerance branch). Production sets
		// this in engine.New; this fixture mirrors that contract so tests
		// don't nil-pointer panic on log calls.
		logFn: func(string, ...interface{}) {},
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

// markStaged forces an order directly into the "staged" status, bypassing
// the lifecycle state machine. Used in tests to simulate both robots arriving
// at their wait points without running Core's reply pipeline.
func markStaged(t *testing.T, db *store.DB, orderID int64) {
	t.Helper()
	if err := db.UpdateOrderStatus(orderID, orders.StatusStaged); err != nil {
		t.Fatalf("mark order %d staged: %v", orderID, err)
	}
}

func TestReleaseStagedOrders_BothStaged(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	if err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}); err != nil {
		t.Fatalf("ReleaseStagedOrders: %v", err)
	}

	a, err := db.GetOrder(result.OrderA.ID)
	if err != nil {
		t.Fatalf("get OrderA: %v", err)
	}
	b, err := db.GetOrder(result.OrderB.ID)
	if err != nil {
		t.Fatalf("get OrderB: %v", err)
	}
	if a.Status != orders.StatusInTransit {
		t.Errorf("OrderA status = %q, want in_transit", a.Status)
	}
	if b.Status != orders.StatusInTransit {
		t.Errorf("OrderB status = %q, want in_transit", b.Status)
	}
}

// TestReleaseStagedOrders_OnlyOneStaged covers two guarantees at once:
//
//  1. Pre-staged tolerance (Bug 2 timing-window fix): if the second leg (A,
//     the delivery robot) hasn't reached staged yet, ReleaseStagedOrders no
//     longer errors. It releases the staged sibling immediately and skips
//     the pre-staged one — the auto-release-on-staged hook in wiring.go
//     picks up A when it later transitions to staged. Pre-2026-04-25
//     semantic was fail-closed (return error); the relaxed contract makes
//     the consolidated RELEASE button safe to click before BOTH robots
//     have arrived.
//  2. B-before-A ordering: because releaseIfStaged is called on the
//     StagedOrderID first, B transitions to in_transit before A's check
//     runs. Observing B's post-call status proves the call order without
//     relying on SQLite's timestamp resolution.
//
// Scenario: B (removal robot) is staged, A (delivery robot) is still in its
// initial post-finalize (pre-staged) status. ReleaseStagedOrders should
// release B, log+skip A, and return nil.
func TestReleaseStagedOrders_OnlyOneStaged(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	// Only B is staged; A is still in its initial post-finalize status.
	markStaged(t, db, result.OrderB.ID)
	aBefore, err := db.GetOrder(result.OrderA.ID)
	if err != nil {
		t.Fatalf("read A before release: %v", err)
	}

	// New contract: pre-staged sibling is tolerated, no error.
	if err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}); err != nil {
		t.Fatalf("ReleaseStagedOrders should succeed when one leg is pre-staged (the auto-release hook covers the late arrival), got: %v", err)
	}

	a, _ := db.GetOrder(result.OrderA.ID)
	b, _ := db.GetOrder(result.OrderB.ID)
	// B was released (proves B-before-A ordering and the staged leg fired).
	if b.Status != orders.StatusInTransit {
		t.Errorf("OrderB status = %q, want in_transit (the staged leg must release immediately)", b.Status)
	}
	// A's status did not change (proves the pre-staged leg was skipped, not
	// silently advanced — auto-release-on-staged hook is responsible for it
	// once A actually reaches staged).
	if a.Status != aBefore.Status {
		t.Errorf("OrderA status changed from %q to %q; pre-staged leg should be left alone for the auto-release hook to handle", aBefore.Status, a.Status)
	}
	if a.Status == orders.StatusInTransit {
		t.Error("OrderA should not have been released when it was not in staged status")
	}
}

// TestReleaseStagedOrders_RejectsNonTwoRobot verifies the claim-mode guard:
// even if a node somehow has both ActiveOrderID and StagedOrderID populated,
// ReleaseStagedOrders refuses to release them unless the active claim is
// two_robot. This is defense-in-depth for direct API callers; the UI already
// gates the button on swap_ready.
func TestReleaseStagedOrders_RejectsNonTwoRobot(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	// Flip the claim's swap mode out from under the runtime. Both order IDs
	// remain tracked, but ReleaseStagedOrders should refuse.
	if _, err := db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "PRODUCE-NODE",
		Role:                "produce",
		SwapMode:            "single_robot",
		PayloadCode:         "WIDGET-A",
		UOPCapacity:         100,
		InboundStaging:      "PRODUCE-IN-STAGING",
		OutboundStaging:     "PRODUCE-OUT-STAGING",
		OutboundDestination: "FILLED-STORAGE",
	}); err != nil {
		t.Fatalf("flip claim swap mode: %v", err)
	}

	if err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}); err == nil {
		t.Fatal("expected error when claim swap mode is not two_robot")
	}

	// Neither order should have been released.
	a, _ := db.GetOrder(result.OrderA.ID)
	b, _ := db.GetOrder(result.OrderB.ID)
	if a.Status == orders.StatusInTransit {
		t.Error("OrderA should not have been released when claim is not two_robot")
	}
	if b.Status == orders.StatusInTransit {
		t.Error("OrderB should not have been released when claim is not two_robot")
	}
}

// TestReleaseStagedOrders_Idempotent verifies that if one order has already
// advanced past staged (e.g. a concurrent Core reply transitioned it to
// in_transit between the operator's click and the handler running), the
// release call treats it as success rather than erroring.
func TestReleaseStagedOrders_Idempotent(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.FinalizeProduceNode(nodeID)
	if err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}
	markStaged(t, db, result.OrderA.ID)
	// B already advanced past staged.
	if err := db.UpdateOrderStatus(result.OrderB.ID, orders.StatusInTransit); err != nil {
		t.Fatalf("force B in_transit: %v", err)
	}

	if err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}); err != nil {
		t.Fatalf("ReleaseStagedOrders should be idempotent on already-released order: %v", err)
	}

	a, _ := db.GetOrder(result.OrderA.ID)
	if a.Status != orders.StatusInTransit {
		t.Errorf("OrderA status = %q, want in_transit", a.Status)
	}
}

func TestReleaseStagedOrders_NoTrackedOrders(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	// No finalize called — runtime has no ActiveOrderID/StagedOrderID.
	err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{})
	if err == nil {
		t.Fatal("expected error when no orders are tracked on the node")
	}
}
