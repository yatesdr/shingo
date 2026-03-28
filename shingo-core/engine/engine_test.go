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
