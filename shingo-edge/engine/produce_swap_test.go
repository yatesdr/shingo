package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/orders"
	ordertestutil "shingoedge/orders/testutil"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedProduceNode creates a process, process node, style, claim, and runtime
// suitable for produce-node finalization tests. Returns all the IDs needed.
func seedProduceNode(t *testing.T, db *store.DB, swapMode protocol.SwapMode) (processID, nodeID, styleID, claimID int64) {
	t.Helper()

	processID, err := db.CreateProcess("PRODUCE-PROC", "produce test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
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
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")

	// A blank swap mode is the legacy simple-produce default (blank historically
	// coerced to "simple"); map it explicitly so the seed routes through the
	// legacy-simple test shim (upsertClaimLegacySimple) below.
	if swapMode == "" {
		swapMode = protocol.SwapModeSimple
	}
	claimID, err = upsertClaimLegacySimple(db, processes.NodeClaimInput{
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
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &cID, 50), "set runtime")

	return processID, nodeID, styleID, claimID
}

// testEngine creates a minimal Engine with a real order manager backed by the
// given SQLite DB. The engine is suitable for testing RequestProduceSwap and
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
		orderMgr: orders.NewManager(db, ordertestutil.NoOpOrderEmitter{}, "test.station"),
		Events:   NewEventBus(),
		stopChan: make(chan struct{}),
		// logFn is initialized to a no-op for tests that exercise diagnostic
		// logging paths (e.g. ReleaseOrderWithLineside's toClaim==nil case,
		// releaseUnlessTerminal's terminal-skip branch). Production sets
		// this in engine.New; this fixture mirrors that contract so tests
		// don't nil-pointer panic on log calls.
		logFn:   func(string, ...any) {},
		debugFn: func(string, ...any) {},
	}
	eng.hourlyTracker = NewHourlyTracker(db, "")
	eng.stationService = service.NewStationService(db)
	eng.changeoverService = service.NewChangeoverService(db)
	// Phase 3a: default sink that does real DB writes so tests
	// exercising state-mutation verbs (BindActiveBin, ClearActiveBin,
	// OnDelivered, etc.) see post-state correctly without each test
	// injecting its own sink. Tests that need to assert on recorded
	// calls (binCalls, bucketCalls, boundaryCalls, etc.) override this
	// by calling eng.SetInventoryDeltaSink(...) themselves.
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	eng.loaderStore = newLoaderStore(eng)
	return eng
}

// TestDelivered_ComplexOrderBindsActiveBin pins the Fix 5 binding fix: a bin
// delivered to a producing node by a COMPLEX order (DeliveryNode blank,
// destination only in steps_json) must bind active_bin_id. Previously the
// delivery handler bailed on the empty DeliveryNode and the bin never bound, so
// PLC ticks parked in pending_uop_delta forever (HK PLN_04, 2026-06-17). The
// second half pins the removal-shaped filter: a complex order whose final
// dropoff is elsewhere (a supermarket) must NOT rebind this node.
func TestDelivered_ComplexOrderBindsActiveBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	// Swap mode is irrelevant to the binding fix (handleNodeOrderDelivered keys
	// on order type + destination, not the claim's swap mode); use the default
	// to avoid the press-index paired-node requirement.
	_, nodeID, _, _ := seedProduceNode(t, db, "")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Complex order whose final dropoff lands at the producing node, with a
	// blank DeliveryNode (as complex orders have) — destination only in steps.
	const binID int64 = 4242
	orderID, err := db.CreateOrder("uuid-cmplx-bind", orders.TypeComplex,
		&nodeID, false, 1, "", "", "PAIRED", "", true, "WIDGET-A")
	testutil.MustNoErr(t, err, "create complex order")
	steps := `[{"action":"wait","node":"PAIRED"},{"action":"pickup","node":"PAIRED"},{"action":"dropoff","node":"` + node.CoreNodeName + `"}]`
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(orderID, steps), "set steps")
	bid := binID
	testutil.MustNoErr(t, db.UpdateOrderBinID(orderID, &bid), "set bin id")

	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID: orderID, OrderType: orders.TypeComplex, ProcessNodeID: &nodeID, BinID: &bid,
	}})

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Fatalf("complex delivery to producing node: ActiveBinID = %v, want %d (must bind)", rt.ActiveBinID, binID)
	}

	// Removal-shaped complex order (final dropoff = a supermarket, not this
	// node) must NOT rebind — the active bin stays bin 4242.
	const binID2 int64 = 5555
	orderID2, err := db.CreateOrder("uuid-cmplx-removal", orders.TypeComplex,
		&nodeID, false, 1, "", "", node.CoreNodeName, "", true, "WIDGET-A")
	testutil.MustNoErr(t, err, "create removal order")
	removalSteps := `[{"action":"pickup","node":"` + node.CoreNodeName + `"},{"action":"dropoff","node":"FILLED-STORAGE"}]`
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(orderID2, removalSteps), "set removal steps")
	bid2 := binID2
	testutil.MustNoErr(t, db.UpdateOrderBinID(orderID2, &bid2), "set removal bin id")

	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID: orderID2, OrderType: orders.TypeComplex, ProcessNodeID: &nodeID, BinID: &bid2,
	}})

	rt2, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime after removal")
	if rt2.ActiveBinID == nil || *rt2.ActiveBinID != binID {
		t.Errorf("after removal-shaped complex delivery: ActiveBinID = %v, want %d unchanged (removal must NOT rebind)", rt2.ActiveBinID, binID)
	}
}

// TestProduceSwap_FinalizeSendsIngestNoLocalOrder verifies that a
// swap-mode produce finalize must NOT mint a local ingest order (the phantom
// the operator-abort fan-out used to cancel into a "not_found"), yet Core must
// still receive the manifest-only ingest stamp via the fire-and-forget outbox.
func TestProduceSwap_FinalizeSendsIngestNoLocalOrder(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "sequential")
	eng := testEngine(t, db)

	if _, err := eng.RequestProduceSwap(nodeID); err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
	}

	// No local ingest order should exist — the swap's complex order carries
	// the bin; the ingest is now only a fire-and-forget manifest stamp.
	all, err := db.ListOrders()
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	for _, o := range all {
		if o.OrderType == protocol.OrderTypeIngest {
			t.Errorf("swap finalize created local ingest order #%d (phantom should be gone)", o.ID)
		}
	}

	// Core still receives the manifest-only ingest stamp via the outbox.
	msgs, err := db.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	stamped := false
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderIngest {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var req protocol.OrderIngestRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "decode ingest payload")
		if req.PayloadCode != "WIDGET-A" {
			t.Errorf("ingest stamp PayloadCode = %q, want WIDGET-A", req.PayloadCode)
		}
		stamped = true
	}
	if !stamped {
		t.Error("expected a manifest-only TypeOrderIngest stamp in the outbox, found none")
	}
}

func TestProduceSequential_RemovalThenBackfill(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "sequential")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	if err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
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
	if runtime.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after finalize", runtime.RemainingUOPCached)
	}
	// Active order should be the complex removal order (Order A)
	if runtime.ActiveOrderID == nil || *runtime.ActiveOrderID != result.Order.ID {
		t.Error("ActiveOrderID should match the removal order")
	}

	// Swap modes no longer create a local ingest order — the swap's complex
	// order carries the bin and the manifest is sent fire-and-forget. (The
	// outbox stamp is covered by
	// TestProduceSwap_FinalizeSendsIngestNoLocalOrder.)
	allOrders, err := db.ListOrders()
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	for _, o := range allOrders {
		if o.OrderType == protocol.OrderTypeIngest {
			t.Errorf("swap finalize created a local ingest order #%d (phantom should be gone)", o.ID)
		}
	}
}

func TestProduceSingleRobot_TenStepSwap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "single_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	if err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
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
	if runtime.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after finalize", runtime.RemainingUOPCached)
	}
}

func TestProduceTwoRobot_BothOrdersCreated(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	if err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
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
	// Fix D: the REQUEST tap must NOT reset the count on two-robot modes —
	// the bin keeps filling until the RELEASE tap, and the release-time
	// manifest is built from this live count.
	if runtime.RemainingUOPCached != 50 {
		t.Errorf("RemainingUOP = %d, want 50 preserved at request (release owns the reset)", runtime.RemainingUOPCached)
	}
}

func TestProduceFinalize_RejectsZeroUOP(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedProduceNode(t, db, "")
	eng := testEngine(t, db)

	// Set UOP to 0 — nothing to finalize
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 0), "set runtime")

	_, err := eng.RequestProduceSwap(nodeID)
	if err == nil {
		t.Fatal("expected error when RemainingUOP is 0")
	}
}

func TestProduceFinalize_RejectsConsumeNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "")
	eng := testEngine(t, db)

	// Override claim to consume role
	_, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
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

	_, err = eng.RequestProduceSwap(nodeID)
	if err == nil {
		t.Fatal("expected error for consume node")
	}
}

// markStaged forces an order directly into the "staged" status, bypassing
// the lifecycle state machine. Used in tests to simulate both robots arriving
// at their wait points without running Core's reply pipeline.
func markStaged(t *testing.T, db *store.DB, orderID int64) {
	t.Helper()
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("mark order %d staged: %v", orderID, err)
	}
}

func TestReleaseStagedOrders_BothStaged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	if err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
	}
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}), "ReleaseStagedOrders")

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

// TestReleaseStagedOrders_OnlyOneStaged covers the post-2026-04-27 contract:
// when Robot B (the lineside robot, StagedOrderID slot) is at staged and
// Robot A is still in some pre-staged status, a single click releases BOTH
// orders. Order A's release fires unconditionally on a non-terminal status
// because Manager.ReleaseOrder no longer requires staged — it just sends
// the OrderRelease envelope and transitions to in_transit. Core's
// HandleOrderRelease handles the rest (queues post-wait blocks for the
// fleet to pick up at whatever point the robot is at).
//
// Pre-2026-04-27 the contract was different: A was skipped, and a separate
// handleAutoReleaseOnStaged hook fired when A later transitioned to staged.
// That coordination layer was deleted; this test now asserts the simpler
// shape.
//
// B-before-A ordering still matters for disposition: B gets the operator's
// full disposition first, A gets the zero-value disposition second. We
// verify both end at in_transit.
//
// Scenario: B (removal robot) is staged, A (delivery robot) is still in its
// initial post-finalize (pre-staged) status. ReleaseStagedOrders releases
// both.
func TestReleaseStagedOrders_OnlyOneStaged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	if err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
	}
	// B at staged (lineside robot parked at the wait point). A at
	// acknowledged — post-dispatch but pre-staged. In production this is
	// the typical state when Robot A is en route to the supermarket while
	// Robot B is already at the line. We seed acknowledged rather than
	// leaving A at the default submitted because Manager.ReleaseOrder has a
	// pre-dispatch guard (pending/submitted = silent no-op) and the
	// lifecycle validator rejects submitted->in_transit. Production traffic
	// reaches at-least acknowledged before B can stage at the line.
	markStaged(t, db, result.OrderB.ID)
	testutil.MustNoErr(t, db.UpdateOrderStatus(result.OrderA.ID, string(orders.StatusAcknowledged)), "seed A acknowledged")

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}), "ReleaseStagedOrders")

	a, _ := db.GetOrder(result.OrderA.ID)
	b, _ := db.GetOrder(result.OrderB.ID)
	if b.Status != orders.StatusInTransit {
		t.Errorf("OrderB status = %q, want in_transit (staged leg releases immediately)", b.Status)
	}
	// New contract: A is also released even though it wasn't at staged. The
	// release envelope was queued, and Manager.ReleaseOrder transitioned A
	// to in_transit. If this assertion fails because A's status didn't
	// change, ReleaseStagedOrders has regressed to its pre-2026-04-27
	// "skip if not staged" semantic and the auto-release hook would need to
	// be reintroduced.
	if a.Status != orders.StatusInTransit {
		t.Errorf("OrderA status = %q, want in_transit (pre-staged leg should release unconditionally under the new contract)", a.Status)
	}
}

// TestReleaseStagedOrders_RejectsNonTwoRobot verifies the claim-mode guard:
// even if a node somehow has both ActiveOrderID and StagedOrderID populated,
// ReleaseStagedOrders refuses to release them unless the active claim is
// two_robot. This is defense-in-depth for direct API callers; the UI already
// gates the button on swap_ready.
func TestReleaseStagedOrders_RejectsNonTwoRobot(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	if err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
	}
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	// Flip the claim's swap mode out from under the runtime. Both order IDs
	// remain tracked, but ReleaseStagedOrders should refuse.
	if _, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
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
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	if err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
	}
	markStaged(t, db, result.OrderA.ID)
	// B already advanced past staged.
	testutil.MustNoErr(t, db.UpdateOrderStatus(result.OrderB.ID, string(orders.StatusInTransit)), "force B in_transit")

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}), "ReleaseStagedOrders should be idempotent on already-released order")

	a, _ := db.GetOrder(result.OrderA.ID)
	if a.Status != orders.StatusInTransit {
		t.Errorf("OrderA status = %q, want in_transit", a.Status)
	}
}

func TestReleaseStagedOrders_NoTrackedOrders(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	// No finalize called — runtime has no ActiveOrderID/StagedOrderID.
	err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{})
	if err == nil {
		t.Fatal("expected error when no orders are tracked on the node")
	}
}
