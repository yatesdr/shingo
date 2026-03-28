package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// --- Test helpers ---

func testDB(t *testing.T) *store.DB {
	t.Helper()
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			if strings.Contains(strings.ToLower(msg), "docker") {
				t.Skipf("skipping integration test: %s", msg)
			}
			panic(r)
		}
	}()

	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("shingocore_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second)),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "docker") {
			t.Skipf("skipping integration test: %v", err)
		}
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { pgContainer.Terminate(ctx) })

	host, _ := pgContainer.Host(ctx)
	port, _ := pgContainer.MappedPort(ctx, "5432")

	db, err := store.Open(&config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     host,
			Port:     port.Int(),
			Database: "shingocore_test",
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func setupTestData(t *testing.T, db *store.DB) (storageNode *store.Node, lineNode *store.Node, bp *store.Payload) {
	t.Helper()
	storageNode = &store.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
	if err := db.CreateNode(storageNode); err != nil {
		t.Fatalf("create storage node: %v", err)
	}
	lineNode = &store.Node{Name: "LINE1-IN", Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	bp = &store.Payload{Code: "PART-A", Description: "Steel bracket tote"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	bt := &store.BinType{Code: "DEFAULT", Description: "Default test bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	return
}

func createTestBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *store.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("get bin type: %v", err)
	}
	bin := &store.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	if err := db.SetBinManifest(bin.ID, `{"items":[]}`, payloadCode, 100); err != nil {
		t.Fatalf("set manifest for bin %s: %v", label, err)
	}
	if err := db.ConfirmBinManifest(bin.ID); err != nil {
		t.Fatalf("confirm manifest for bin %s: %v", label, err)
	}
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin %s after setup: %v", label, err)
	}
	return got
}

func testEnvelope() *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
}

// newTestEngine constructs a real Engine wired to the test database and simulator.
// No Kafka, no HTTP server. Background goroutines tick harmlessly against the simulator.
// The engine is stopped automatically via t.Cleanup.
func newTestEngine(t *testing.T, db *store.DB, sim *simulator.SimulatorBackend) *Engine {
	t.Helper()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"

	eng := New(Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: nil, // safe: checkConnectionStatus nil-guards msgClient
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })
	return eng
}

// --- TC-15: Full Lifecycle ---
// Scenario: verifies the complete order lifecycle works end-to-end.
// Dispatches a retrieve order, drives RUNNING → FINISHED, simulates Edge receipt
// confirmation. Verifies complete lifecycle: bin moved + claim released.
func TestSimulator_FullLifecycle(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-LC")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	// Step 1: Create order
	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "lc-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("lc-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("initial status = %q, want %q", order.Status, dispatch.StatusDispatched)
	}

	// Step 2: Drive RUNNING — event fires, handleVendorStatusChange updates DB
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order, err = db.GetOrderByUUID("lc-1")
	if err != nil {
		t.Fatalf("get order after RUNNING: %v", err)
	}
	if order.Status != "in_transit" {
		t.Fatalf("after RUNNING: status = %q, want %q", order.Status, "in_transit")
	}

	// Step 3: Drive FINISHED — handleVendorStatusChange calls handleOrderDelivered
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order, err = db.GetOrderByUUID("lc-1")
	if err != nil {
		t.Fatalf("get order after FINISHED: %v", err)
	}
	if order.Status != "delivered" {
		t.Fatalf("after FINISHED: status = %q, want %q", order.Status, "delivered")
	}

	// Step 4: Simulate Edge receipt — triggers handleOrderCompleted → ApplyBinArrival
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "lc-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order, err = db.GetOrderByUUID("lc-1")
	if err != nil {
		t.Fatalf("get order after receipt: %v", err)
	}
	if order.Status != "confirmed" {
		t.Fatalf("after receipt: status = %q, want %q", order.Status, "confirmed")
	}

	// Step 5: Verify bin moved to destination and claim released
	bin, err := db.GetBin(*order.BinID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if bin.NodeID == nil || *bin.NodeID != lineNode.ID {
		t.Errorf("bin node = %v, want %d (line node)", bin.NodeID, lineNode.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil (claim should be released)", bin.ClaimedBy)
	}
}

// --- TC-2: Staged Complex Order Release ---
// Scenario: verifies staged order release works through the full engine pipeline.
// Creates a complex order with a "wait" step (pickup → dropoff → wait → pickup → dropoff).
// Drives fleet through RUNNING → WAITING so the engine sets DB status to "staged".
// Then sends HandleOrderRelease and verifies post-wait blocks are appended and the
// order completes through the full lifecycle.
func TestSimulator_StagedComplexOrderRelease(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC2")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	env := testEnvelope()
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "staged-tc2",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: storageNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("staged-tc2")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("initial status = %q, want %q", order.Status, dispatch.StatusDispatched)
	}

	// Simulator should have a staged (incomplete) order
	if sim.StagedOrderCount() != 1 {
		t.Fatalf("staged orders = %d, want 1", sim.StagedOrderCount())
	}

	// Pre-wait blocks only (pickup + dropoff = 2 blocks)
	view := sim.GetOrder(order.VendorOrderID)
	if view == nil {
		t.Fatal("simulator should have the staged order")
	}
	if len(view.Blocks) != 2 {
		t.Fatalf("pre-wait blocks = %d, want 2", len(view.Blocks))
	}
	if view.Complete {
		t.Fatal("staged order should not be complete yet")
	}

	// Step 2: Drive RUNNING — robot is moving to first pickup
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order, err = db.GetOrderByUUID("staged-tc2")
	if err != nil {
		t.Fatalf("get order after RUNNING: %v", err)
	}
	if order.Status != "in_transit" {
		t.Fatalf("after RUNNING: status = %q, want %q", order.Status, "in_transit")
	}

	// Step 3: Drive WAITING — robot has arrived at wait point and is dwelling.
	// The engine maps WAITING → "staged" and updates the DB.
	sim.DriveState(order.VendorOrderID, "WAITING")

	order, err = db.GetOrderByUUID("staged-tc2")
	if err != nil {
		t.Fatalf("get order after WAITING: %v", err)
	}
	if order.Status != dispatch.StatusStaged {
		t.Fatalf("after WAITING: status = %q, want %q", order.Status, dispatch.StatusStaged)
	}

	// Step 4: Edge sends release — appends post-wait blocks
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "staged-tc2",
	})

	// Verify: post-wait blocks were appended (2 pre-wait + 2 post-wait = 4)
	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 4 {
		t.Fatalf("total blocks after release = %d, want 4", len(view.Blocks))
	}
	if !view.Complete {
		t.Fatal("order should be complete after release")
	}

	// All blocks must have bin tasks
	for i, b := range view.Blocks {
		if b.BinTask == "" {
			t.Errorf("block %d (%q) has empty BinTask", i, b.BlockID)
		}
	}

	// Order status should now be in_transit (released from staging)
	order, err = db.GetOrderByUUID("staged-tc2")
	if err != nil {
		t.Fatalf("get order after release: %v", err)
	}
	if order.Status != dispatch.StatusInTransit {
		t.Fatalf("after release: status = %q, want %q", order.Status, dispatch.StatusInTransit)
	}

	// Step 5: Drive RUNNING → FINISHED to complete the order
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order, err = db.GetOrderByUUID("staged-tc2")
	if err != nil {
		t.Fatalf("get order after FINISHED: %v", err)
	}
	if order.Status != "delivered" {
		t.Fatalf("after FINISHED: status = %q, want %q", order.Status, "delivered")
	}
}

// --- TC-ClaimBin: Silent Claim Overwrite ---
// Regression: guards against silent bin claim overwrites (fixed 2026-03-27).
// Demonstrates that ClaimBin allows a second order to silently overwrite an
// existing claim. In production, two near-simultaneous dispatches could race:
// both call FindSourceBinFIFO (which returns the same unclaimed bin), then
// both call ClaimBin. The second ClaimBin silently steals the bin from the
// first order because the SQL lacks AND claimed_by IS NULL.
//
// This test expects the second ClaimBin to FAIL (return an error), proving
// the bug exists when it doesn't.
func TestClaimBin_SilentOverwrite(t *testing.T) {
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-CLAIM")

	// Order 1 claims the bin
	if err := db.ClaimBin(bin.ID, 100); err != nil {
		t.Fatalf("first ClaimBin: %v", err)
	}

	// Verify claim is set
	bin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin after first claim: %v", err)
	}
	if bin.ClaimedBy == nil || *bin.ClaimedBy != 100 {
		t.Fatalf("claimed_by = %v, want 100", bin.ClaimedBy)
	}

	// Order 2 tries to claim the same bin — this SHOULD fail but currently doesn't.
	err = db.ClaimBin(bin.ID, 200)
	if err == nil {
		// Bug confirmed: second claim silently overwrote the first.
		bin, _ = db.GetBin(bin.ID)
		t.Errorf("BUG: ClaimBin(bin=%d, order=200) succeeded — silently overwrote claim from order 100. claimed_by is now %v",
			bin.ID, *bin.ClaimedBy)
	} else {
		t.Logf("ClaimBin correctly rejected second claim: %v", err)
	}
}

// =============================================================================
// TC-23 cluster: Line operations — staged bins, operator moves, and changeover
//
// These tests model a production line with 3 bins in operation. The operator
// moves one bin elsewhere (quality hold, storage, etc.) via the system, then
// initiates changeover. Each test explores a different timing/state scenario.
// =============================================================================

// setupThreeBinLine creates a line with 3 bins delivered and confirmed (claims released).
// This represents a line mid-operation: bins are physically there, orders are done.
// Returns the 3 bins, the storage node, the line node, and the payload.
func setupThreeBinLine(t *testing.T, db *store.DB) (bins [3]*store.Bin, storageNode, lineNode *store.Node, bp *store.Payload) {
	t.Helper()
	storageNode, lineNode, bp = setupTestData(t, db)

	// Create a quality-hold node (another destination the operator might use)
	qhNode := &store.Node{Name: "QUALITY-HOLD-1", Zone: "Q", Enabled: true}
	if err := db.CreateNode(qhNode); err != nil {
		t.Fatalf("create QH node: %v", err)
	}

	// Create 3 bins at the line node (as if retrieve orders completed)
	for i := 0; i < 3; i++ {
		label := fmt.Sprintf("BIN-LINE-%d", i+1)
		bins[i] = createTestBinAtNode(t, db, bp.Code, lineNode.ID, label)
		// Move bin to line node (createTestBinAtNode puts it at the node we specify,
		// but let's be explicit about the final location)
		if err := db.MoveBin(bins[i].ID, lineNode.ID); err != nil {
			t.Fatalf("move bin %s to line: %v", label, err)
		}
	}

	// Refresh bins so we have current state
	for i := 0; i < 3; i++ {
		var err error
		bins[i], err = db.GetBin(bins[i].ID)
		if err != nil {
			t.Fatalf("refresh bin %d: %v", i, err)
		}
	}

	return
}

// --- TC-23a: Operator tries to move a staged bin while its order is active ---
// Scenario: verifies that store orders cannot steal bins from active staged orders.
//
// Line has 3 bins. One bin is claimed by an active staged order (robot waiting).
// The operator creates a store order to move that bin to quality hold. The store
// order should NOT be able to claim the bin because it's already claimed by the
// staged order.
func TestTC23a_MoveClaimedStagedBin(t *testing.T) {
	db := testDB(t)
	bins, storageNode, lineNode, bp := setupThreeBinLine(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Simulate an active staged order on bin 0: the robot delivered it and is
	// waiting for release. We'll create a complex order that reaches WAITING.
	// But first, let's move bin 0 back to storage so we can retrieve it with staging.
	if err := db.MoveBin(bins[0].ID, storageNode.ID); err != nil {
		t.Fatalf("move bin 0 to storage: %v", err)
	}

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "staged-23a",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: storageNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("staged-23a")
	if err != nil {
		t.Fatalf("get staged order: %v", err)
	}

	// Drive robot to WAITING — order becomes "staged", bin is at line, claimed
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")

	order, err = db.GetOrderByUUID("staged-23a")
	if err != nil {
		t.Fatalf("get order after WAITING: %v", err)
	}
	if order.Status != dispatch.StatusStaged {
		t.Fatalf("order status = %q, want %q", order.Status, dispatch.StatusStaged)
	}

	// Verify bin is claimed
	stagedBin, err := db.GetBin(*order.BinID)
	if err != nil {
		t.Fatalf("get staged bin: %v", err)
	}
	if stagedBin.ClaimedBy == nil {
		t.Fatal("staged bin should be claimed by the active order")
	}
	t.Logf("bin %d claimed by order %d (staged order)", stagedBin.ID, *stagedBin.ClaimedBy)

	// Now operator tries to send this claimed bin to quality hold via a store order.
	// The store order queries the line node, finds bins, but should skip claimed ones.
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "store-23a-steal",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	storeOrder, err := db.GetOrderByUUID("store-23a-steal")
	if err != nil {
		t.Fatalf("get store order: %v", err)
	}

	// The store order should NOT have claimed the staged bin
	if storeOrder.BinID != nil && *storeOrder.BinID == stagedBin.ID {
		t.Errorf("BUG: store order claimed bin %d which is already claimed by staged order %d",
			stagedBin.ID, *stagedBin.ClaimedBy)
	}

	// Verify the staged bin's claim is still intact
	stagedBin, err = db.GetBin(stagedBin.ID)
	if err != nil {
		t.Fatalf("re-check staged bin: %v", err)
	}
	if stagedBin.ClaimedBy == nil || *stagedBin.ClaimedBy != order.ID {
		t.Errorf("staged bin claim changed — was order %d, now %v", order.ID, stagedBin.ClaimedBy)
	} else {
		t.Logf("staged bin correctly still claimed by order %d", order.ID)
	}

	// The store order may have claimed one of the OTHER unclaimed bins at the line.
	// That's fine — the important thing is it didn't steal the staged bin.
	if storeOrder.BinID != nil {
		t.Logf("store order claimed bin %d (not the staged bin) — OK", *storeOrder.BinID)
	} else {
		t.Logf("store order has no bin — it may have dispatched without one (potential issue)")
	}
}

// --- TC-23b: Cancel staged order, then move the freed bin ---
// Scenario: verifies that cancelling a staged order releases the bin claim,
// allowing a subsequent store order to move the freed bin.
//
// Line has 3 bins. Bin 0 is claimed by an active staged order. Operator
// cancels the staged order. The claim should release. Operator then creates
// a store order to move the freed bin. It should succeed. The other 2 bins
// should be completely unaffected.
func TestTC23b_CancelThenMoveBin(t *testing.T) {
	db := testDB(t)
	bins, storageNode, lineNode, bp := setupThreeBinLine(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Move bin 0 to storage so we can create a staged retrieve
	if err := db.MoveBin(bins[0].ID, storageNode.ID); err != nil {
		t.Fatalf("move bin 0 to storage: %v", err)
	}

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "staged-23b",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: storageNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("staged-23b")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Drive to staged
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")

	order, err = db.GetOrderByUUID("staged-23b")
	if err != nil {
		t.Fatalf("get order after WAITING: %v", err)
	}
	claimedBinID := *order.BinID

	// Verify bin is claimed before cancel
	bin0, _ := db.GetBin(claimedBinID)
	if bin0.ClaimedBy == nil {
		t.Fatal("bin should be claimed before cancel")
	}

	// Cancel the staged order
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "staged-23b",
		Reason:    "changeover",
	})

	order, err = db.GetOrderByUUID("staged-23b")
	if err != nil {
		t.Fatalf("get order after cancel: %v", err)
	}
	if order.Status != dispatch.StatusCancelled {
		t.Fatalf("order status after cancel = %q, want %q", order.Status, dispatch.StatusCancelled)
	}

	// KEY CHECK: bin claim should be released
	bin0, err = db.GetBin(claimedBinID)
	if err != nil {
		t.Fatalf("get bin after cancel: %v", err)
	}
	if bin0.ClaimedBy != nil {
		t.Errorf("BUG: bin %d still claimed by %v after order was cancelled", bin0.ID, *bin0.ClaimedBy)
	} else {
		t.Logf("bin %d claim correctly released after cancel", bin0.ID)
	}

	// Now move the freed bin via store order
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "store-23b-move",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	storeOrder, err := db.GetOrderByUUID("store-23b-move")
	if err != nil {
		t.Fatalf("get store order: %v", err)
	}

	if storeOrder.Status == dispatch.StatusFailed {
		t.Errorf("store order failed — freed bin should have been claimable. Status: %s", storeOrder.Status)
	} else {
		t.Logf("store order status: %s", storeOrder.Status)
	}

	if storeOrder.BinID != nil {
		t.Logf("store order claimed bin %d", *storeOrder.BinID)
	} else {
		t.Errorf("store order has no bin — the freed bin should have been available")
	}

	// Verify the other 2 bins are untouched
	for i := 1; i < 3; i++ {
		b, err := db.GetBin(bins[i].ID)
		if err != nil {
			t.Fatalf("get bin %d: %v", i, err)
		}
		if b.NodeID == nil || *b.NodeID != lineNode.ID {
			t.Errorf("bin %d moved unexpectedly — node=%v, want %d", i, b.NodeID, lineNode.ID)
		}
	}
}

// --- TC-23c: Changeover with one bin already gone ---
// Regression: guards against ghost robot dispatch when no bin is available
// at the source node (fixed 2026-03-27 in planStore).
//
// Scenario: Line has 3 bins. Operator already moved bin 0 to quality hold
// (its order completed, claim released, bin physically at QH node). Now
// changeover begins: store orders are issued to clear all bins from the
// line. But only 2 bins are actually there.
//
// Questions this test answers:
// 1. Do the store orders find only the 2 remaining bins?
// 2. If 3 store orders are submitted, does the 3rd one fail gracefully
//    or dispatch a robot with no bin?
// 3. Are the remaining 2 bins handled cleanly?
func TestTC23c_ChangeoverWithMissingBin(t *testing.T) {
	db := testDB(t)
	bins, _, lineNode, _ := setupThreeBinLine(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Move bin 0 away from the line (simulating a completed move to quality hold)
	qhNode, err := db.GetNodeByDotName("QUALITY-HOLD-1")
	if err != nil {
		t.Fatalf("get QH node: %v", err)
	}
	if err := db.MoveBin(bins[0].ID, qhNode.ID); err != nil {
		t.Fatalf("move bin 0 to QH: %v", err)
	}
	t.Logf("bin %d moved to QUALITY-HOLD-1 (simulating prior move order)", bins[0].ID)

	// Verify: only 2 bins remain at the line
	lineBins, err := db.ListBinsByNode(lineNode.ID)
	if err != nil {
		t.Fatalf("list bins at line: %v", err)
	}
	if len(lineBins) != 2 {
		t.Fatalf("line has %d bins, want 2 (one should be at QH)", len(lineBins))
	}

	// Changeover: operator submits 3 store orders to clear the line.
	// In practice, the operator might issue one per bin position, or the system
	// might batch them. We submit 3 to see what happens with the missing bin.
	storeUUIDs := []string{"changeover-store-1", "changeover-store-2", "changeover-store-3"}
	for _, uuid := range storeUUIDs {
		d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
			OrderUUID:  uuid,
			OrderType:  dispatch.OrderTypeStore,
			SourceNode: lineNode.Name,
		})
	}

	// Check each store order
	var claimed []int64
	var noBinOrders []string
	var failedOrders []string

	for _, uuid := range storeUUIDs {
		so, err := db.GetOrderByUUID(uuid)
		if err != nil {
			t.Fatalf("get store order %s: %v", uuid, err)
		}
		t.Logf("store order %s: status=%s, bin_id=%v, vendor_id=%s",
			uuid, so.Status, so.BinID, so.VendorOrderID)

		if so.Status == dispatch.StatusFailed {
			failedOrders = append(failedOrders, uuid)
		} else if so.BinID == nil {
			noBinOrders = append(noBinOrders, uuid)
		} else {
			claimed = append(claimed, *so.BinID)
		}
	}

	t.Logf("--- Summary ---")
	t.Logf("Store orders that claimed a bin: %d (bin IDs: %v)", len(claimed), claimed)
	t.Logf("Store orders with no bin (dispatched empty): %d (%v)", len(noBinOrders), noBinOrders)
	t.Logf("Store orders that failed: %d (%v)", len(failedOrders), failedOrders)

	// EXPECTED: 2 orders claim a bin, 1 order has nothing to do
	if len(claimed) != 2 {
		t.Errorf("expected 2 store orders to claim bins, got %d", len(claimed))
	}

	// The 3rd order should ideally FAIL with a clear error, not dispatch a robot
	// with no bin. A dispatched order with BinID=nil is a ghost robot.
	if len(noBinOrders) > 0 {
		t.Errorf("BUG: %d store order(s) dispatched with no bin — robot sent to line with nothing to pick up: %v",
			len(noBinOrders), noBinOrders)

		// Check if these ghost orders actually sent fleet requests
		for _, uuid := range noBinOrders {
			so, _ := db.GetOrderByUUID(uuid)
			if so.VendorOrderID != "" {
				t.Errorf("BUG: ghost store order %s has vendor_id=%s — fleet will send a real robot for nothing",
					uuid, so.VendorOrderID)
			}
		}
	}

	if len(failedOrders) == 1 {
		t.Logf("3rd store order correctly failed (no bin available)")
	} else if len(failedOrders) == 0 && len(noBinOrders) == 0 && len(claimed) == 2 {
		// One order must have handled "no bins left" somehow — check its status
		t.Logf("only 2 orders were created/dispatched — system may have handled it gracefully")
	}

	// Verify bin 0 was NOT touched (it's at QH, not at the line)
	bin0, err := db.GetBin(bins[0].ID)
	if err != nil {
		t.Fatalf("get bin 0: %v", err)
	}
	if bin0.NodeID == nil || *bin0.NodeID != qhNode.ID {
		t.Errorf("bin 0 was moved from QH — node=%v, want %d", bin0.NodeID, qhNode.ID)
	}
}

// --- TC-23d: Changeover while move-to-quality-hold is still in flight ---
// Scenario: verifies that changeover store orders respect in-flight claims
// and don't steal bins from active move orders.
//
// Line has 3 bins, all unclaimed (delivered). Operator issues a store order
// to send bin 0 to quality hold — the robot is dispatched and bin 0 is now
// claimed by that in-flight order. Before the robot arrives, the operator
// initiates changeover: store orders for all line bins.
//
// Questions this test answers:
// 1. Do the changeover store orders skip bin 0 (claimed by the QH move)?
// 2. Do the changeover orders correctly claim only the 2 unclaimed bins?
// 3. Does the in-flight QH order complete correctly after changeover starts?
func TestTC23d_ChangeoverWhileMoveInFlight(t *testing.T) {
	db := testDB(t)
	bins, _, lineNode, _ := setupThreeBinLine(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Operator sends bin 0 to quality hold via store order
	// First, manually claim bin 0 so the store order picks it up specifically
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "qh-move-23d",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	qhOrder, err := db.GetOrderByUUID("qh-move-23d")
	if err != nil {
		t.Fatalf("get QH order: %v", err)
	}
	if qhOrder.BinID == nil {
		t.Fatal("QH store order should have claimed a bin")
	}
	qhBinID := *qhOrder.BinID
	t.Logf("QH order claimed bin %d, status=%s, vendor_id=%s", qhBinID, qhOrder.Status, qhOrder.VendorOrderID)

	// Robot is in transit — bin is claimed but still at line node
	if qhOrder.VendorOrderID != "" {
		sim.DriveState(qhOrder.VendorOrderID, "RUNNING")
	}

	// Step 2: BEFORE the QH robot arrives, changeover starts.
	// Operator submits 2 more store orders to clear remaining bins.
	changeoverUUIDs := []string{"changeover-23d-1", "changeover-23d-2"}
	for _, uuid := range changeoverUUIDs {
		d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
			OrderUUID:  uuid,
			OrderType:  dispatch.OrderTypeStore,
			SourceNode: lineNode.Name,
		})
	}

	// Check changeover orders
	var changeoverClaimed []int64
	for _, uuid := range changeoverUUIDs {
		so, err := db.GetOrderByUUID(uuid)
		if err != nil {
			t.Fatalf("get changeover order %s: %v", uuid, err)
		}
		t.Logf("changeover order %s: status=%s, bin_id=%v", uuid, so.Status, so.BinID)

		if so.BinID != nil {
			changeoverClaimed = append(changeoverClaimed, *so.BinID)

			// KEY CHECK: changeover must NOT steal the QH order's bin
			if *so.BinID == qhBinID {
				t.Errorf("BUG: changeover order %s claimed bin %d which is in-flight to QH (claimed by order %d)",
					uuid, qhBinID, qhOrder.ID)
			}
		}
	}

	if len(changeoverClaimed) != 2 {
		t.Errorf("expected 2 changeover orders to each claim a bin, got %d", len(changeoverClaimed))
	}

	// Verify the 3 bins are claimed by 3 different orders (no overlaps)
	allClaimed := append([]int64{qhBinID}, changeoverClaimed...)
	seen := map[int64]bool{}
	for _, id := range allClaimed {
		if seen[id] {
			t.Errorf("BUG: bin %d claimed by multiple orders", id)
		}
		seen[id] = true
	}

	// Verify the QH order's bin is still correctly claimed by the QH order
	qhBin, err := db.GetBin(qhBinID)
	if err != nil {
		t.Fatalf("get QH bin: %v", err)
	}
	if qhBin.ClaimedBy == nil || *qhBin.ClaimedBy != qhOrder.ID {
		t.Errorf("QH bin claim changed — expected order %d, got %v", qhOrder.ID, qhBin.ClaimedBy)
	} else {
		t.Logf("QH bin %d still correctly claimed by order %d", qhBinID, qhOrder.ID)
	}

	// Step 3: QH order completes — verify clean state
	if qhOrder.VendorOrderID != "" {
		sim.DriveState(qhOrder.VendorOrderID, "FINISHED")
	}

	qhOrder, err = db.GetOrderByUUID("qh-move-23d")
	if err != nil {
		t.Fatalf("get QH order after finish: %v", err)
	}
	t.Logf("QH order final status: %s", qhOrder.Status)

	// Verify no bins are double-claimed at the end
	for _, b := range bins {
		refreshed, err := db.GetBin(b.ID)
		if err != nil {
			t.Fatalf("get bin %d: %v", b.ID, err)
		}
		if refreshed.ClaimedBy != nil {
			claimOrder, _ := db.GetOrderByUUID(fmt.Sprintf("%d", *refreshed.ClaimedBy))
			t.Logf("bin %d (%s): claimed_by=%d, node=%v",
				refreshed.ID, refreshed.Label, *refreshed.ClaimedBy, refreshed.NodeID)
			_ = claimOrder // just for logging context
		}
	}
}

// --- TC-21: Only available bin is in quality hold ---
// Scenario: verifies that the system does not dispatch a bin in quality hold.
//
// A line requests a part. The only bin of that part in the warehouse is in
// quality hold (flagged for inspection). The system should not dispatch it.
// The order should be queued, not failed — so the fulfillment scanner can
// pick it up later when inventory frees up.
func TestTC21_QualityHoldBinNotDispatched(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a single bin at storage, then put it in quality hold
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-QH")
	if err := db.UpdateBinStatus(bin.ID, "quality_hold"); err != nil {
		t.Fatalf("set bin to quality_hold: %v", err)
	}
	bin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("refresh bin: %v", err)
	}
	if bin.Status != "quality_hold" {
		t.Fatalf("bin status = %q, want quality_hold", bin.Status)
	}
	t.Logf("bin %d (%s) is in quality_hold at %s", bin.ID, bin.Label, storageNode.Name)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Request a retrieve for this payload — only bin is in quality hold
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-qh-21",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("retrieve-qh-21")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	t.Logf("order status: %s, bin_id: %v, vendor_order_id: %s", order.Status, order.BinID, order.VendorOrderID)

	// The order should NOT be dispatched — no eligible bin exists
	if order.Status == dispatch.StatusDispatched {
		t.Errorf("BUG: order was dispatched despite the only bin being in quality_hold")
	}

	// The order should be queued (waiting for inventory), not failed
	if order.Status == dispatch.StatusQueued {
		t.Logf("order correctly queued — waiting for inventory to free up")
	} else if order.Status == dispatch.StatusFailed {
		t.Errorf("order failed instead of being queued — operator gets an error instead of a wait")
	} else {
		t.Logf("order status is %q (not queued or dispatched)", order.Status)
	}

	// No robot should have been sent
	if sim.OrderCount() != 0 {
		t.Errorf("BUG: simulator has %d orders — a robot was dispatched for a quality_hold bin", sim.OrderCount())
	} else {
		t.Logf("no fleet orders — no robot dispatched (correct)")
	}

	// The bin should NOT be claimed
	bin, err = db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin after order: %v", err)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("BUG: quality_hold bin was claimed by order %d", *bin.ClaimedBy)
	} else {
		t.Logf("quality_hold bin correctly not claimed")
	}

	// The bin should still be in quality_hold status (not changed by the dispatch attempt)
	if bin.Status != "quality_hold" {
		t.Errorf("bin status changed to %q — quality_hold should be preserved", bin.Status)
	}
}

// --- TC-30: Failed order creates a return — does the return inherit the reservation? ---
// Scenario: verifies that when a fleet-reported failure triggers an auto-return
// order, the bin claim transfers cleanly from the failed order to the return order.
//
// A retrieve order is dispatched and the fleet accepts it. The robot starts
// moving (RUNNING). Then the fleet reports the order as FAILED (robot broke
// down mid-delivery). The system should:
// 1. Mark the original order as failed
// 2. Release the original order's bin claim
// 3. Create an auto-return order to send the bin back to storage
// 4. Claim the bin for the return order
//
// The bug risk: the fleet-reported failure path (handleVendorStatusChange)
// does NOT call UnclaimOrderBins before emitting EventOrderFailed. The
// EventOrderFailed handler calls maybeCreateReturnOrder, which tries to
// ClaimBin for the return order. But with the ClaimBin fix (AND claimed_by
// IS NULL), this will fail because the bin is still claimed by the original
// order. The return order gets created but can't claim its bin.
func TestTC30_FailedOrderReturnClaimTransfer(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC30")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Dispatch a retrieve order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-tc30",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("retrieve-tc30")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("order status = %q, want dispatched", order.Status)
	}
	if order.BinID == nil {
		t.Fatal("order should have a bin claimed")
	}
	t.Logf("order %d dispatched, bin %d claimed, vendor_id=%s", order.ID, *order.BinID, order.VendorOrderID)

	// Verify bin is claimed by the original order
	bin, err = db.GetBin(*order.BinID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
		t.Fatalf("bin claimed_by = %v, want %d", bin.ClaimedBy, order.ID)
	}

	// Step 2: Robot starts moving
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order, err = db.GetOrderByUUID("retrieve-tc30")
	if err != nil {
		t.Fatalf("get order after RUNNING: %v", err)
	}
	if order.Status != "in_transit" {
		t.Fatalf("after RUNNING: status = %q, want in_transit", order.Status)
	}

	// Step 3: Fleet reports FAILED (robot broke down)
	sim.DriveState(order.VendorOrderID, "FAILED")

	// Give the synchronous event chain a moment to complete
	order, err = db.GetOrderByUUID("retrieve-tc30")
	if err != nil {
		t.Fatalf("get order after FAILED: %v", err)
	}
	if order.Status != dispatch.StatusFailed {
		t.Fatalf("after FAILED: status = %q, want failed", order.Status)
	}
	t.Logf("original order %d is now failed", order.ID)

	// Step 4: Check bin claim state — was it released by the failure handler?
	bin, err = db.GetBin(*order.BinID)
	if err != nil {
		t.Fatalf("get bin after failure: %v", err)
	}
	if bin.ClaimedBy != nil && *bin.ClaimedBy == order.ID {
		t.Errorf("BUG: bin %d still claimed by failed order %d — fleet-reported failure path does not release bin claims",
			bin.ID, order.ID)
	} else if bin.ClaimedBy != nil {
		t.Logf("bin %d claimed by order %d (should be the return order)", bin.ID, *bin.ClaimedBy)
	} else {
		t.Logf("bin %d claim released (claimed_by=nil)", bin.ID)
	}

	// Step 5: Check if a return order was created
	// The return order should have PayloadDesc = "auto_return" and OrderType = "store"
	// We can find it by looking for orders other than the original
	allOrders, err := db.ListOrdersByStation(order.StationID, 50)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}

	var returnOrder *store.Order
	for _, o := range allOrders {
		if o.ID != order.ID && o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}

	if returnOrder == nil {
		t.Logf("no auto-return order was created")
		// This might be OK or might be a bug depending on the guards
	} else {
		t.Logf("return order %d created: type=%s, status=%s, bin_id=%v, source=%s, dest=%s",
			returnOrder.ID, returnOrder.OrderType, returnOrder.Status,
			returnOrder.BinID, returnOrder.SourceNode, returnOrder.DeliveryNode)

		// The return order should have the bin
		if returnOrder.BinID == nil || *returnOrder.BinID != *order.BinID {
			t.Errorf("return order bin_id = %v, want %d (same bin as failed order)", returnOrder.BinID, *order.BinID)
		}

		// KEY CHECK: the bin should be claimed by the RETURN order, not the original
		bin, err = db.GetBin(*order.BinID)
		if err != nil {
			t.Fatalf("get bin for final check: %v", err)
		}

		if bin.ClaimedBy == nil {
			t.Errorf("BUG: bin %d is unclaimed — return order %d exists but couldn't claim the bin (likely because original claim wasn't released first)",
				bin.ID, returnOrder.ID)
		} else if *bin.ClaimedBy == returnOrder.ID {
			t.Logf("bin %d correctly claimed by return order %d", bin.ID, returnOrder.ID)
		} else if *bin.ClaimedBy == order.ID {
			t.Errorf("BUG: bin %d still claimed by failed order %d — return order %d could not take over the claim",
				bin.ID, order.ID, returnOrder.ID)
		} else {
			t.Errorf("bin %d claimed by unexpected order %d (not original %d or return %d)",
				bin.ID, *bin.ClaimedBy, order.ID, returnOrder.ID)
		}

		// The return order should not be in a failed state
		if returnOrder.Status == dispatch.StatusFailed {
			t.Errorf("return order %d is failed — bin may be stranded", returnOrder.ID)
		}
	}
}
