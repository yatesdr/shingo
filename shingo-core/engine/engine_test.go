package engine

import (
	"fmt"
	"sync"
	"testing"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// --- Test helpers (thin wrappers delegating to internal/testdb) ---

func testDB(t *testing.T) *store.DB {
	return testdb.Open(t)
}

func setupTestData(t *testing.T, db *store.DB) (storageNode *store.Node, lineNode *store.Node, bp *store.Payload) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	return sd.StorageNode, sd.LineNode, sd.Payload
}

func createTestBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *store.Bin {
	return testdb.CreateBinAtNode(t, db, payloadCode, nodeID, label)
}

func testEnvelope() *protocol.Envelope {
	return testdb.Envelope()
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
	t.Parallel()
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

	order := testdb.RequireOrderStatus(t, db, "lc-1", dispatch.StatusDispatched)

	// Step 2: Drive RUNNING — event fires, handleVendorStatusChange updates DB
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order = testdb.RequireOrderStatus(t, db, "lc-1", "in_transit")

	// Step 3: Drive FINISHED — handleVendorStatusChange calls handleOrderDelivered
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "lc-1", "delivered")

	// Step 4: Simulate Edge receipt — triggers handleOrderCompleted → ApplyBinArrival
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "lc-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "lc-1", "confirmed")

	// Step 5: Verify bin moved to destination and claim released
	testdb.AssertBinAtNode(t, db, *order.BinID, lineNode.ID)
	testdb.AssertBinUnclaimed(t, db, *order.BinID)
}

// --- TC-2: Staged Complex Order Release ---
// Scenario: verifies staged order release works through the full engine pipeline.
// Creates a complex order with a "wait" step (pickup → dropoff → wait → pickup → dropoff).
// Drives fleet through RUNNING → WAITING so the engine sets DB status to "staged".
// Then sends HandleOrderRelease and verifies post-wait blocks are appended and the
// order completes through the full lifecycle.
func TestSimulator_StagedComplexOrderRelease(t *testing.T) {
	t.Parallel()
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

	order := testdb.RequireOrderStatus(t, db, "staged-tc2", dispatch.StatusDispatched)

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

	order = testdb.RequireOrderStatus(t, db, "staged-tc2", "in_transit")

	// Step 3: Drive WAITING — robot has arrived at wait point and is dwelling.
	// The engine maps WAITING → "staged" and updates the DB.
	sim.DriveState(order.VendorOrderID, "WAITING")

	order = testdb.RequireOrderStatus(t, db, "staged-tc2", dispatch.StatusStaged)

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
	order = testdb.RequireOrderStatus(t, db, "staged-tc2", dispatch.StatusInTransit)

	// Step 5: Drive RUNNING → FINISHED to complete the order
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "staged-tc2", "delivered")
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
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-CLAIM")

	// Order 1 claims the bin
	if err := db.ClaimBin(bin.ID, 100); err != nil {
		t.Fatalf("first ClaimBin: %v", err)
	}

	// Verify claim is set
	testdb.RequireBinClaimedBy(t, db, bin.ID, 100)

	// Order 2 tries to claim the same bin — this SHOULD fail but currently doesn't.
	err := db.ClaimBin(bin.ID, 200)
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

// --- TC-23a: Operator tries to move a claimed bin via a second store order ---
// Scenario: verifies that store orders cannot steal bins from active orders.
//
// Line has 3 bins. Bin 0 is already claimed by an active store order (robot
// in transit to move it). The operator creates another store order at the same
// line. The second order should skip the claimed bin and only take unclaimed ones.
func TestTC23a_MoveClaimedStagedBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, lineNode, _ := setupThreeBinLine(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Claim bin 0 via a store order (simulates an active move-to-QH order)
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "active-23a",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	activeOrder := testdb.RequireOrder(t, db, "active-23a")
	if activeOrder.BinID == nil {
		t.Fatal("active order should have claimed a bin")
	}
	claimedBinID := *activeOrder.BinID
	t.Logf("active order %d claimed bin %d", activeOrder.ID, claimedBinID)

	// Drive robot to in-transit (bin is claimed, robot is moving)
	if activeOrder.VendorOrderID != "" {
		sim.DriveState(activeOrder.VendorOrderID, "RUNNING")
	}

	// Now operator creates another store order at the same line
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "second-23a",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	secondOrder := testdb.RequireOrder(t, db, "second-23a")

	// The second order should NOT have claimed the same bin
	if secondOrder.BinID != nil && *secondOrder.BinID == claimedBinID {
		t.Errorf("BUG: second store order claimed bin %d which is already claimed by active order %d",
			claimedBinID, activeOrder.ID)
	}

	// Verify the active order's bin claim is still intact
	testdb.AssertBinClaimedBy(t, db, claimedBinID, activeOrder.ID)

	// The second order should have claimed one of the OTHER unclaimed bins
	if secondOrder.BinID != nil {
		t.Logf("second order claimed bin %d (not the in-flight bin) — correct", *secondOrder.BinID)
	} else {
		t.Logf("second order has no bin — may have failed (check status: %s)", secondOrder.Status)
	}
}

// --- TC-23b: Cancel in-flight store order — return order claims bin ---
// Scenario: verifies cancel → unclaim → auto-return-order → re-claim flow.
//
// Line has 3 bins. Bin is claimed by active store order, robot is RUNNING.
// Operator cancels. The system unclaims the bin, then maybeCreateReturnOrder
// creates a return order that immediately re-claims the same bin to bring
// it back. The bin is never truly "free" — it transfers from the original
// order to the return order. A subsequent store order should claim one of
// the OTHER unclaimed bins, not the one held by the return order.
func TestTC23b_CancelThenMoveBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bins, _, lineNode, _ := setupThreeBinLine(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Create a store order that claims a bin and dispatches a robot
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "active-23b",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	order := testdb.RequireOrder(t, db, "active-23b")
	if order.BinID == nil {
		t.Fatal("store order should have claimed a bin")
	}
	claimedBinID := *order.BinID
	t.Logf("active order %d claimed bin %d", order.ID, claimedBinID)

	// Drive robot to in-transit
	if order.VendorOrderID != "" {
		sim.DriveState(order.VendorOrderID, "RUNNING")
	}

	// Verify bin is claimed before cancel
	bin0, _ := db.GetBin(claimedBinID)
	if bin0.ClaimedBy == nil {
		t.Fatal("bin should be claimed before cancel")
	}

	// Cancel the active order
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "active-23b",
		Reason:    "changeover",
	})

	order = testdb.RequireOrderStatus(t, db, "active-23b", dispatch.StatusCancelled)

	// KEY CHECK: bin should now be claimed by the auto-return order, not free.
	// Cancel flow: unclaim original → maybeCreateReturnOrder → return claims bin.
	bin0 = testdb.RequireBin(t, db, claimedBinID)
	if bin0.ClaimedBy == nil {
		t.Logf("bin %d is unclaimed after cancel (no return order created)", bin0.ID)
	} else if *bin0.ClaimedBy == order.ID {
		t.Errorf("BUG: bin %d still claimed by cancelled order %d — unclaim failed", bin0.ID, order.ID)
	} else {
		t.Logf("bin %d claim transferred from cancelled order %d to return order %d — correct",
			bin0.ID, order.ID, *bin0.ClaimedBy)
	}

	// A subsequent store order should claim one of the OTHER bins, not the
	// one held by the return order.
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "store-23b-move",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	storeOrder := testdb.RequireOrder(t, db, "store-23b-move")

	if storeOrder.BinID != nil && *storeOrder.BinID == claimedBinID {
		t.Errorf("BUG: store order stole bin %d from the return order", claimedBinID)
	} else if storeOrder.BinID != nil {
		t.Logf("store order claimed different bin %d — correct (return order protects bin %d)",
			*storeOrder.BinID, claimedBinID)
	} else {
		t.Logf("store order has no bin (status=%s)", storeOrder.Status)
	}

	// Verify remaining bins
	for i := 0; i < 3; i++ {
		b := testdb.RequireBin(t, db, bins[i].ID)
		claimStr := "unclaimed"
		if b.ClaimedBy != nil {
			claimStr = fmt.Sprintf("claimed_by=%d", *b.ClaimedBy)
		}
		t.Logf("bin %d (%s): node=%v, %s", b.ID, b.Label, b.NodeID, claimStr)
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
	t.Parallel()
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
	testdb.AssertBinAtNode(t, db, bins[0].ID, qhNode.ID)
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
	t.Parallel()
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

	qhOrder := testdb.RequireOrder(t, db, "qh-move-23d")
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
	testdb.AssertBinClaimedBy(t, db, qhBinID, qhOrder.ID)

	// Step 3: QH order completes — verify clean state
	if qhOrder.VendorOrderID != "" {
		sim.DriveState(qhOrder.VendorOrderID, "FINISHED")
	}

	qhOrder = testdb.RequireOrder(t, db, "qh-move-23d")
	t.Logf("QH order final status: %s", qhOrder.Status)

	// Verify no bins are double-claimed at the end
	for _, b := range bins {
		refreshed := testdb.RequireBin(t, db, b.ID)
		if refreshed.ClaimedBy != nil {
			t.Logf("bin %d (%s): still claimed by order %d, node=%v",
				refreshed.ID, refreshed.Label, *refreshed.ClaimedBy, refreshed.NodeID)
		}
	}
}

// --- TC-24: Complex order bin poaching ---
// Regression: complex orders never call ClaimBin, leaving the bin's
// claimed_by field NULL even while a robot is physically carrying it.
// A concurrent store order can claim the same bin.
//
// This is most realistic with empty bins: a complex order moves an empty
// bin from storage to a line, and while the robot is in transit, a store
// order targets the same storage node and claims the in-flight bin.
//
// Setup: 1 bin at storage. Complex order picks it up (robot RUNNING).
// Then a store order tries to clear a bin from the same storage node.
// Expected (current behavior): store order claims the same bin — BUG.
// Expected (fixed): store order fails or picks a different bin.
func TestTC24_ComplexOrderBinPoaching(t *testing.T) {
	t.Parallel()
	// TC-24a: complex orders now claim bins at dispatch, preventing poaching.
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Single bin at storage — this is the one the complex order will move
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-POACH-1")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Complex order: pick up at storage, drop off at line
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-24",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	complexOrder := testdb.RequireOrder(t, db, "complex-24")
	t.Logf("complex order %d: status=%s, bin_id=%v, vendor_id=%s",
		complexOrder.ID, complexOrder.Status, complexOrder.BinID, complexOrder.VendorOrderID)

	// Complex order must now claim the bin at dispatch time
	if complexOrder.BinID == nil {
		t.Fatalf("complex order has BinID=nil — claimComplexBins did not set it")
	}
	if *complexOrder.BinID != bin.ID {
		t.Fatalf("complex order claimed bin %d, want %d", *complexOrder.BinID, bin.ID)
	}
	t.Logf("complex order claimed bin %d — protected from poaching", *complexOrder.BinID)

	// Drive robot to RUNNING — it's physically carrying the bin now
	if complexOrder.VendorOrderID != "" {
		sim.DriveState(complexOrder.VendorOrderID, "RUNNING")
	}

	// Verify the bin is claimed by the complex order
	testdb.RequireBinClaimedBy(t, db, bin.ID, complexOrder.ID)

	// Now: a store order arrives targeting the same storage node.
	// Because the bin is claimed, the store order must NOT get it.
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "store-poach-24",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: storageNode.Name,
	})

	storeOrder := testdb.RequireOrder(t, db, "store-poach-24")

	// KEY CHECK: store order must NOT have claimed the bin the robot is carrying
	if storeOrder.BinID != nil && *storeOrder.BinID == bin.ID {
		t.Errorf("BUG: store order %d claimed bin %d which is being carried by complex order %d",
			storeOrder.ID, bin.ID, complexOrder.ID)
	} else {
		t.Logf("store order correctly did not poach bin %d (status=%s, bin_id=%v)",
			bin.ID, storeOrder.Status, storeOrder.BinID)
	}
}

// --- TC-24b: Stale bin location after complex order completes ---
// Regression: complex orders never call ApplyBinArrival because BinID is nil,
// so the bin's node_id in the database is never updated after the robot
// delivers it. The bin physically moves but the DB still shows the old node.
//
// Setup: 1 bin at storage. Complex order moves it to line. Order completes
// (FINISHED). Check: does the bin's node_id in the DB reflect the line node?
// Expected (current behavior): bin still shows storage node — BUG.
func TestTC24b_StaleBinLocationAfterComplexOrder(t *testing.T) {
	t.Parallel()
	// TC-24b: complex orders now set BinID, so ApplyBinArrival fires on
	// completion and moves the bin to the delivery node in the DB.
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-STALE-24b")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Complex order: pick up at storage, drop off at line
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-24b",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order := testdb.RequireOrder(t, db, "complex-24b")
	if order.BinID == nil {
		t.Fatalf("complex order BinID=nil — claimComplexBins did not set it")
	}
	t.Logf("complex order %d: vendor_id=%s, bin_id=%d", order.ID, order.VendorOrderID, *order.BinID)

	// Verify bin is at storage before robot moves
	testdb.RequireBinAtNode(t, db, bin.ID, storageNode.ID)

	// Drive the order through RUNNING → FINISHED (delivered)
	if order.VendorOrderID != "" {
		sim.DriveState(order.VendorOrderID, "RUNNING")
		sim.DriveState(order.VendorOrderID, "FINISHED")
	}

	// Simulate Edge receipt — this triggers handleOrderCompleted → ApplyBinArrival.
	// FINISHED alone only sets status to "delivered". The Edge receipt confirms
	// physical delivery and runs the bin lifecycle.
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "complex-24b",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	// KEY CHECK: bin's location must update to the line node
	testdb.AssertBinAtNode(t, db, bin.ID, lineNode.ID)

	// Bin should be unclaimed after completion (ApplyBinArrival unclaims)
	if bin.ClaimedBy != nil {
		t.Errorf("bin %d should be unclaimed after completion, got claimed_by=%d",
			bin.ID, *bin.ClaimedBy)
	}

	t.Logf("bin %d final state: node_id=%v, claimed_by=%v, status=%s",
		bin.ID, bin.NodeID, bin.ClaimedBy, bin.Status)
}

// --- TC-24c: Phantom inventory — retrieve dispatched to empty node ---
// Regression: because complex orders don't update bin location in the DB,
// a bin moved by a complex order still appears at its original node. A
// retrieve order targeting that node will dispatch a robot to pick up a
// bin that isn't physically there.
//
// Setup: 1 bin at storage. Complex order moves it to line (FINISHED).
// Then a retrieve order targets storage. The retrieve should find the bin
// (it's still listed there in the DB) and dispatch a robot to an empty slot.
// Expected (current behavior): robot dispatched to empty node — BUG.
func TestTC24c_PhantomInventoryRetrieve(t *testing.T) {
	t.Parallel()
	// TC-24c: with the bin claimed and BinID set, ApplyBinArrival runs on
	// completion, moving the bin to the line node. A subsequent retrieve
	// targeting storage will NOT find a phantom bin at the old location.
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-PHANTOM-24c")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Complex order moves bin from storage to line
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-24c",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order := testdb.RequireOrder(t, db, "complex-24c")
	if order.BinID == nil {
		t.Fatalf("complex order BinID=nil — claimComplexBins did not set it")
	}

	// Complete the complex order — drive RUNNING → FINISHED, then Edge receipt
	if order.VendorOrderID != "" {
		sim.DriveState(order.VendorOrderID, "RUNNING")
		sim.DriveState(order.VendorOrderID, "FINISHED")
	}
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "complex-24c",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	// Verify bin location updated to line node (not stale at storage)
	bin = testdb.RequireBin(t, db, bin.ID)
	if bin.NodeID != nil && *bin.NodeID == storageNode.ID {
		t.Errorf("bin %d still at storage node %d — ApplyBinArrival did not fire", bin.ID, storageNode.ID)
	} else if bin.NodeID != nil && *bin.NodeID == lineNode.ID {
		t.Logf("bin %d correctly at line node %d after complex order", bin.ID, lineNode.ID)
	}

	// Now: a retrieve order requests a bin of this payload.
	// The bin is now at line with status=staged (lineside delivery),
	// so FindSourceBinFIFO should NOT find it.
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-24c",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	retrieveOrder := testdb.RequireOrder(t, db, "retrieve-24c")

	t.Logf("retrieve order %d: status=%s, bin_id=%v",
		retrieveOrder.ID, retrieveOrder.Status, retrieveOrder.BinID)

	// The retrieve must NOT have claimed the bin that was moved to line —
	// no phantom dispatch to an empty storage slot.
	if retrieveOrder.BinID != nil && *retrieveOrder.BinID == bin.ID {
		t.Errorf("retrieve order %d claimed bin %d which was moved to line — phantom dispatch",
			retrieveOrder.ID, bin.ID)
	} else {
		t.Logf("no phantom dispatch — retrieve correctly did not find bin at old storage location")
	}
}

// --- TC-25: Store order correctly claims staged bin at core node ---
// Investigated and DISMISSED as a false positive. Original concern was that
// planStore/planMove could "poach" a staged bin at a lineside core node.
//
// Analysis: physical constraint — a core node holds exactly ONE bin. After a
// retrieve delivers a bin (staged, unclaimed), the only bin at that node IS
// the bin the operator wants to act on. Store and move orders targeting a
// core node as source SHOULD claim the staged bin — that's how the operator
// releases it (store-back, quality hold move, partial release, etc.).
// Filtering out staged bins would break these legitimate operator workflows.
//
// Setup: retrieve delivers bin to line. Bin is staged and unclaimed.
// A store order targets line as source (operator releasing the bin).
// Expected: store order SHOULD claim the staged bin — correct behavior.
func TestTC25_StoreOrderClaimsStagedBinAtCoreNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-STAGED-25")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Retrieve order delivers bin to line
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-25",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "retrieve-25")

	// Drive to completion
	sim.DriveSimpleLifecycle(order.VendorOrderID)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "retrieve-25",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	// Verify bin is at line, staged, unclaimed
	testdb.RequireBinAtNode(t, db, bin.ID, lineNode.ID)
	bin = testdb.RequireBin(t, db, bin.ID)
	if bin.Status != "staged" {
		t.Fatalf("bin status should be 'staged' at lineside, got %q", bin.Status)
	}
	if bin.ClaimedBy != nil {
		t.Fatalf("bin should be unclaimed after ApplyBinArrival, got claimed_by=%d", *bin.ClaimedBy)
	}

	// Store order targets line node as source — operator releasing the bin
	// (e.g. store-back, quality hold, partial release). This SHOULD succeed.
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "store-release-25",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	storeOrder := testdb.RequireOrder(t, db, "store-release-25")

	// KEY CHECK: store order SHOULD claim the staged bin — it's the only bin
	// at the node and the operator is intentionally releasing it.
	if storeOrder.BinID == nil || *storeOrder.BinID != bin.ID {
		t.Errorf("store order should have claimed staged bin %d at %s, got bin_id=%v (status=%s)",
			bin.ID, lineNode.Name, storeOrder.BinID, storeOrder.Status)
	} else {
		t.Logf("store order %d correctly claimed staged bin %d at %s for operator release",
			storeOrder.ID, bin.ID, lineNode.Name)
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
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a single bin at storage, then put it in quality hold
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-QH")
	if err := db.UpdateBinStatus(bin.ID, "quality_hold"); err != nil {
		t.Fatalf("set bin to quality_hold: %v", err)
	}
	bin = testdb.RequireBin(t, db, bin.ID)
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

	order := testdb.RequireOrder(t, db, "retrieve-qh-21")

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
	testdb.AssertBinUnclaimed(t, db, bin.ID)

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
	t.Parallel()
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

	order := testdb.RequireOrderStatus(t, db, "retrieve-tc30", dispatch.StatusDispatched)
	if order.BinID == nil {
		t.Fatal("order should have a bin claimed")
	}
	t.Logf("order %d dispatched, bin %d claimed, vendor_id=%s", order.ID, *order.BinID, order.VendorOrderID)

	// Verify bin is claimed by the original order
	testdb.RequireBinClaimedBy(t, db, *order.BinID, order.ID)

	// Step 2: Robot starts moving
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order = testdb.RequireOrderStatus(t, db, "retrieve-tc30", "in_transit")

	// Step 3: Fleet reports FAILED (robot broke down)
	sim.DriveState(order.VendorOrderID, "FAILED")

	// Give the synchronous event chain a moment to complete
	order = testdb.RequireOrderStatus(t, db, "retrieve-tc30", dispatch.StatusFailed)
	t.Logf("original order %d is now failed", order.ID)

	// Step 4: Check bin claim state — was it released by the failure handler?
	bin = testdb.RequireBin(t, db, *order.BinID)
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
		bin = testdb.RequireBin(t, db, *order.BinID)

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

// --- TC-28: Two lines request the same part at the same time ---
// Scenario: verifies that concurrent retrieve orders for the same payload
// each get a different bin, with no double-assignment.
//
// Two storage nodes each hold one PART-A bin (one bin per node — physical
// constraint). Two retrieve orders fire back-to-back for the same payload.
// Expected: each order claims a different bin. No bin is double-claimed.
//
// Risk: FindSourceBinFIFO returns the oldest unclaimed bin. If both orders
// SELECT the same bin before either calls ClaimBin, the second ClaimBin
// fails (WHERE claimed_by IS NULL). planRetrieve does not retry — it
// returns claim_failed and the order dies. This test checks whether the
// system handles this correctly or whether we need retry logic.
func TestTC28_ConcurrentRetrieveSamePart(t *testing.T) {
	db := testDB(t)

	// Two storage nodes, each with one bin of PART-A
	storageNode1 := &store.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
	if err := db.CreateNode(storageNode1); err != nil {
		t.Fatalf("create storage node 1: %v", err)
	}
	storageNode2 := &store.Node{Name: "STORAGE-A2", Zone: "A", Enabled: true}
	if err := db.CreateNode(storageNode2); err != nil {
		t.Fatalf("create storage node 2: %v", err)
	}

	// Two line nodes (two different production lines)
	lineNode1 := &store.Node{Name: "LINE1-IN", Enabled: true}
	if err := db.CreateNode(lineNode1); err != nil {
		t.Fatalf("create line node 1: %v", err)
	}
	lineNode2 := &store.Node{Name: "LINE2-IN", Enabled: true}
	if err := db.CreateNode(lineNode2); err != nil {
		t.Fatalf("create line node 2: %v", err)
	}

	bp := &store.Payload{Code: "PART-A", Description: "Steel bracket tote"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	bt := &store.BinType{Code: "DEFAULT", Description: "Default test bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	bin1 := createTestBinAtNode(t, db, bp.Code, storageNode1.ID, "BIN-A1")
	bin2 := createTestBinAtNode(t, db, bp.Code, storageNode2.ID, "BIN-A2")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Line 1 requests PART-A
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-line1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode1.Name,
		Quantity:     1,
	})

	// Line 2 requests PART-A immediately after
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-line2",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode2.Name,
		Quantity:     1,
	})

	order1 := testdb.RequireOrder(t, db, "retrieve-line1")
	order2 := testdb.RequireOrder(t, db, "retrieve-line2")

	t.Logf("order 1: status=%s, bin_id=%v, vendor_id=%s", order1.Status, order1.BinID, order1.VendorOrderID)
	t.Logf("order 2: status=%s, bin_id=%v, vendor_id=%s", order2.Status, order2.BinID, order2.VendorOrderID)

	// Both orders should have dispatched successfully
	bothDispatched := order1.VendorOrderID != "" && order2.VendorOrderID != ""
	if !bothDispatched {
		t.Errorf("expected both orders to dispatch — order1 vendor=%q, order2 vendor=%q",
			order1.VendorOrderID, order2.VendorOrderID)
		if order1.VendorOrderID == "" {
			t.Logf("order 1 failed to dispatch (status=%s) — possible TOCTOU race in FindSourceBinFIFO → ClaimBin", order1.Status)
		}
		if order2.VendorOrderID == "" {
			t.Logf("order 2 failed to dispatch (status=%s) — possible TOCTOU race in FindSourceBinFIFO → ClaimBin", order2.Status)
		}
	}

	// Each order should have claimed a DIFFERENT bin
	if order1.BinID != nil && order2.BinID != nil {
		if *order1.BinID == *order2.BinID {
			t.Errorf("BUG: both orders claimed the same bin %d — double assignment", *order1.BinID)
		} else {
			t.Logf("correct: order 1 claimed bin %d, order 2 claimed bin %d — no collision", *order1.BinID, *order2.BinID)
		}
	}

	// Verify bins are claimed by the correct orders
	bin1 = testdb.RequireBin(t, db, bin1.ID)
	bin2 = testdb.RequireBin(t, db, bin2.ID)

	claimedBins := 0
	if bin1.ClaimedBy != nil {
		claimedBins++
		t.Logf("bin %d (%s) claimed by order %d", bin1.ID, bin1.Label, *bin1.ClaimedBy)
	}
	if bin2.ClaimedBy != nil {
		claimedBins++
		t.Logf("bin %d (%s) claimed by order %d", bin2.ID, bin2.Label, *bin2.ClaimedBy)
	}

	if claimedBins != 2 {
		t.Errorf("expected 2 bins claimed, got %d — one order may have failed at ClaimBin", claimedBins)
	}
}

// --- TC-36: Retrieve claim failure — queue instead of fail ---
// Scenario: Two concurrent retrieve orders compete for the only bin of a payload.
// Both find the same bin via FindSourceBinFIFO (TOCTOU gap between SELECT and
// UPDATE). The first ClaimBin succeeds. The second ClaimBin fails with
// "bin is locked, already claimed, or does not exist".
//
// Bug: planRetrieve returns planningError{Code: "claim_failed"}, which causes
// HandleOrderRequest to call failOrder — permanently failing the order. But
// claim_failed is a transient condition: bins of the right payload DO exist,
// one was just claimed by another order in the race window. The order should be
// queued so the fulfillment scanner retries when a bin becomes available.
//
// Fix: HandleOrderRequest now checks for planErr.Code == "claim_failed" and
// calls queueOrder instead of failOrder. The fulfillment scanner will retry
// on its next sweep.
//
// Note: This test uses concurrent goroutines to trigger the TOCTOU race. The
// race is not guaranteed on every run — if the Go runtime serializes the
// goroutines, both orders succeed sequentially. The test always passes when
// no race occurs; it only fails when the race exposes the claim_failed path
// and the fix is not applied.
// TestMaybeCreateReturnOrder_SourceNode verifies that auto-return orders use the
// original order's SourceNode (where the bin actually is in the DB), not DeliveryNode
// (where the bin was headed but never reached). This was Bug 1: maybeCreateReturnOrder
// used order.DeliveryNode for SourceNode, sending the recovery robot to the wrong node.
//
// This test isolates the bug without relying on TC-42's round-trip symmetry
// (where SourceNode == DeliveryNode masks the defect).
func TestMaybeCreateReturnOrder_SourceNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RETSRC")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Dispatch a retrieve order: storage → line
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "ret-src-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "ret-src-1")
	if order.SourceNode != storageNode.Name {
		t.Fatalf("order SourceNode = %q, want %q (storage)", order.SourceNode, storageNode.Name)
	}
	if order.DeliveryNode != lineNode.Name {
		t.Fatalf("order DeliveryNode = %q, want %q (line)", order.DeliveryNode, lineNode.Name)
	}

	// Robot starts moving then fails mid-transit
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FAILED")

	// Find auto-return order
	allOrders, _ := db.ListOrders("", 50)
	var returnOrder *store.Order
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}
	if returnOrder == nil {
		t.Fatalf("no auto-return order created after fleet failure")
	}

	// KEY CHECK: return order SourceNode must be the original order's SourceNode
	// (storage, where the bin IS), NOT DeliveryNode (line, where the bin never reached)
	if returnOrder.SourceNode != order.SourceNode {
		t.Errorf("BUG: return order SourceNode = %q, want %q (order.SourceNode = storage where bin actually is). "+
			"maybeCreateReturnOrder used order.DeliveryNode which is WRONG for mid-transit failures — "+
			"recovery robot goes to line to pick up a bin that's still at storage",
			returnOrder.SourceNode, order.SourceNode)
	}

	// Verify bin re-claimed by return order
	bin, _ = db.GetBin(bin.ID)
	if bin.ClaimedBy == nil || *bin.ClaimedBy != returnOrder.ID {
		t.Errorf("bin claimed_by = %v, want %d (return order)", bin.ClaimedBy, returnOrder.ID)
	}
}

func TestTC36_RetrieveClaimFailure_QueueNotFail(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Single bin — both orders compete for the same bin
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC36")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Fire two concurrent retrieve orders for the same payload.
	// Both will call FindSourceBinFIFO → find the same unclaimed bin → both
	// try ClaimBin. One wins, the other gets claim_failed.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-start
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID:    "tc36-a",
			OrderType:    dispatch.OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			DeliveryNode: lineNode.Name,
			Quantity:     1,
		})
	}()

	go func() {
		defer wg.Done()
		<-start
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID:    "tc36-b",
			OrderType:    dispatch.OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			DeliveryNode: lineNode.Name,
			Quantity:     1,
		})
	}()

	close(start) // fire both goroutines simultaneously
	wg.Wait()

	orderA := testdb.RequireOrder(t, db, "tc36-a")
	orderB := testdb.RequireOrder(t, db, "tc36-b")

	t.Logf("order A: status=%s bin=%v vendor=%s", orderA.Status, orderA.BinID, orderA.VendorOrderID)
	t.Logf("order B: status=%s bin=%v vendor=%s", orderB.Status, orderB.BinID, orderB.VendorOrderID)

	// Neither order should be permanently failed for a transient claim race.
	for _, order := range []*store.Order{orderA, orderB} {
		if order.Status == dispatch.StatusFailed {
			t.Errorf("BUG: order %s permanently failed after claim_failed — should be queued for retry",
				order.EdgeUUID)
		}
	}

	// One should be dispatched, the other queued (not failed, not sourcing)
	dispatched := 0
	queued := 0
	for _, order := range []*store.Order{orderA, orderB} {
		switch order.Status {
		case dispatch.StatusDispatched, dispatch.StatusInTransit:
			dispatched++
		case dispatch.StatusQueued:
			queued++
		}
	}

	if dispatched == 1 && queued == 1 {
		t.Logf("correct: one dispatched, one queued — fulfillment scanner will retry")
	} else if dispatched == 2 {
		t.Logf("both dispatched — race did not trigger (scheduler serialized), no bug exposed this run")
	} else {
		t.Logf("unexpected distribution: dispatched=%d queued=%d (statuses: A=%s B=%s)",
			dispatched, queued, orderA.Status, orderB.Status)
	}
}

// --- TC-38: Cancel delivered order must not create return order / receipt on cancelled order ---
// Scenario (reproduces production incident from 2026-03-30):
//   1. Retrieve order dispatched, robot runs and finishes → status = "delivered"
//   2. Admin cancels the order (before operator confirms receipt) → status = "cancelled"
//   3. Operator confirms receipt on the already-cancelled order → status = "confirmed"
//
// Two bugs exposed:
//   Bug A: maybeCreateReturnOrder fires on the cancelled event and creates a return order
//           that claims the bin, even though the bin was already delivered to lineside.
//           The return order has SourceNode = warehouse (wrong — bin is physically at lineside).
//   Bug B: ConfirmReceipt does not guard against cancelled orders. It overwrites status
//           from "cancelled" back to "confirmed" and calls ApplyBinArrival, moving the bin
//           in the DB to lineside while it's claimed by the return order.
//
// Result: bin is at lineside in DB, claimed by a return order that thinks it's at the
// warehouse. Return order can't dispatch. Bin is permanently locked. Team can't release
// lineside or run new orders against that bin.
func TestTC38_CancelDeliveredOrder_NoReturnOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC38")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Create retrieve order — bin at storage, robot dispatched.
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "tc38-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrderStatus(t, db, "tc38-1", dispatch.StatusDispatched)
	binID := *order.BinID

	// Step 2: Robot runs and finishes. Order status = "delivered".
	// Bin is physically at lineside, but DB still has it at source (ApplyBinArrival
	// only fires on receipt confirmation, which hasn't happened yet).
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "tc38-1", "delivered")
	t.Logf("after fleet FINISHED: order=%d status=delivered bin=%d", order.ID, binID)

	// Step 3: Admin cancels the delivered order (3:21 PM in production incident).
	// BUG A: maybeCreateReturnOrder fires and creates a spurious return order.
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "tc38-1",
		Reason:    "cancelled by admin",
	})

	order = testdb.RequireOrderStatus(t, db, "tc38-1", dispatch.StatusCancelled)

	// Verify no spurious auto_return order was created.
	t.Logf("checking for spurious return orders after cancel of delivered order %d", order.ID)
	returnOrders, err := db.ListOrdersByBin(binID, 10)
	if err != nil {
		t.Fatalf("list orders for bin %d: %v", binID, err)
	}
	for _, o := range returnOrders {
		if o.PayloadDesc == "auto_return" {
			t.Errorf("BUG A: auto_return order %d created for delivered order %d — "+
				"delivered orders must not generate return orders (bin is at lineside)",
				o.ID, order.ID)
		}
	}

	// Verify bin is unclaimed after cancel (no return order holding it).
	testdb.AssertBinUnclaimed(t, db, binID)

	// Step 4: Operator confirms receipt on the now-cancelled order (3:36 PM in production).
	// BUG B: ConfirmReceipt does not reject receipts on cancelled orders.
	// It overwrites status from "cancelled" to "confirmed" and runs ApplyBinArrival.
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "tc38-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order, _ = db.GetOrderByUUID("tc38-1")
	t.Logf("after receipt on cancelled order: status=%s (should still be cancelled)", order.Status)

	if order.Status == dispatch.StatusConfirmed {
		t.Errorf("BUG B: receipt on cancelled order %d changed status to %q — "+
			"ConfirmReceipt must reject cancelled orders", order.ID, order.Status)
	}

	// Final state: bin should be at lineside, unclaimed, and usable.
	testdb.AssertBinUnclaimed(t, db, binID)
	bin := testdb.RequireBin(t, db, binID)
	t.Logf("final: bin=%d node=%v claimed=%v", binID, bin.NodeID, bin.ClaimedBy)
}

// --- TC-39: TerminateOrder rejects terminal statuses ---
// Scenario: Admin clicks Terminate on a confirmed/completed order. The API must reject
// this because the bin is already at its destination and the order is final.
//
// Bug: TerminateOrder (orders.go:76) has no status guard. It unclaims bins, overwrites
// status to "cancelled", and emits EventOrderCancelled — which triggers
// maybeCreateReturnOrder. The return order claims the bin with the wrong source node,
// permanently locking it at lineside.
//
// This is the root enabler for the TC-38 incident. If TerminateOrder rejected terminal
// statuses, the spurious return order and receipt-on-cancelled bugs wouldn't trigger.
func TestTC39_TerminateOrder_RejectsTerminalStatuses(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC39")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()
	var err error

	// Full lifecycle: dispatch → run → finish → confirm receipt
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "tc39-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "tc39-1")
	binID := *order.BinID

	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "tc39-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "tc39-1", dispatch.StatusConfirmed)

	bin, _ := db.GetBin(binID)
	t.Logf("pre-terminate: order=%d status=%s bin=%d node=%v claimed=%v",
		order.ID, order.Status, binID, bin.NodeID, bin.ClaimedBy)

	// Try to terminate the confirmed/completed order via TerminateOrder (web UI path).
	// BUG C: This should be rejected, but currently it succeeds.
	err = eng.TerminateOrder(order.ID, "admin")
	if err == nil {
		t.Errorf("BUG C: TerminateOrder accepted order %d in status %q — should reject terminal statuses",
			order.ID, order.Status)

		// Verify the damage: check for spurious return order
		returnOrders, _ := db.ListOrdersByBin(binID, 10)
		for _, o := range returnOrders {
			if o.PayloadDesc == "auto_return" {
				t.Errorf("BUG C (cascade): auto_return order %d created after terminating confirmed order",
					o.ID)
			}
		}

		bin, _ = db.GetBin(binID)
		if bin.ClaimedBy != nil {
			t.Errorf("BUG C (cascade): bin %d claimed_by = %d after terminating confirmed order",
				binID, *bin.ClaimedBy)
		}
	} else {
		t.Logf("correct: TerminateOrder rejected terminal status %q: %v", order.Status, err)
		}
	}

// TC-80: Orphaned bin claim after terminal order — reconciliation detects and sweep fixes.
//
// Simulates the production bug where a bin (Core_Testing30001) shows as "claimed"
// on the Nodes and Bins pages but has no visible active orders. Root cause:
// UpdateOrderStatus(failed) and UnclaimOrderBins are separate SQL statements.
// If unclaim fails silently (connection drop, deadlock), the bin stays claimed
// forever by a terminal order.
//
// The fix adds FailOrderAtomic / CancelOrderAtomic that wrap status + unclaim
// in a single transaction. This test verifies:
//   1. Reconciliation anomalies detect orphaned claims
//   2. ReleaseOrphanedClaims sweep fixes them
//   3. FailOrderAtomic prevents the leak in the first place
func TestTC80_OrphanedBinClaim_ReconciliationDetectsAndSweepFixes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC80")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Dispatch a retrieve order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-tc80",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "retrieve-tc80")
	if order.BinID == nil {
		t.Fatal("order should have a bin claimed")
	}
	t.Logf("order %d dispatched, bin %d claimed", order.ID, *order.BinID)

	// Verify bin is claimed
	testdb.RequireBinClaimedBy(t, db, *order.BinID, order.ID)

	// Step 2: Simulate the pre-fix bug — manually set order to failed
	// WITHOUT releasing the claim (simulates UnclaimOrderBins failing silently)
	_, err := db.Exec(`UPDATE orders SET status='failed', error_detail='simulated partial failure', updated_at=NOW() WHERE id=$1`, order.ID)
	if err != nil {
		t.Fatalf("manual fail order: %v", err)
	}
	// Bin should still be claimed (the bug state)
	bin = testdb.RequireBin(t, db, *order.BinID)
	if bin.ClaimedBy == nil {
		t.Fatal("bin should still be claimed (simulating leaked claim)")
	}
	t.Logf("simulated bug: order %d is failed but bin %d still claimed_by %d", order.ID, bin.ID, *bin.ClaimedBy)

	// Step 3: Verify reconciliation detects the anomaly
	anomalies, err := db.ListOrderCompletionAnomalies()
	if err != nil {
		t.Fatalf("list anomalies: %v", err)
	}
	found := false
	for _, a := range anomalies {
		if a.OrderID == order.ID && a.Issue == "terminal_order_still_claims_bin" {
			found = true
			t.Logf("reconciliation detected: order %d issue=%s bin_id=%v", a.OrderID, a.Issue, a.BinID)
			break
		}
	}
	if !found {
		t.Error("reconciliation did NOT detect terminal_order_still_claims_bin anomaly")
	}

	// Step 4: Verify full anomaly list includes it via ListReconciliationAnomalies
	reconAnomalies, err := db.ListReconciliationAnomalies()
	if err != nil {
		t.Fatalf("list recon anomalies: %v", err)
	}
	foundRecon := false
	for _, a := range reconAnomalies {
		if a.Issue == "terminal_order_still_claims_bin" && a.OrderID != nil && *a.OrderID == order.ID {
			foundRecon = true
			t.Logf("full reconciliation: category=%s severity=%s action=%s",
				a.Category, a.Severity, a.RecommendedAction)
			break
		}
	}
	if !foundRecon {
		t.Error("ListReconciliationAnomalies did NOT detect terminal_order_still_claims_bin")
	}

	// Step 5: Run the orphan claim sweep
	released, err := db.ReleaseOrphanedClaims()
	if err != nil {
		t.Fatalf("release orphaned claims: %v", err)
	}
	if released != 1 {
		t.Errorf("expected 1 orphaned claim released, got %d", released)
	}

	// Step 6: Verify bin is now unclaimed
	testdb.AssertBinUnclaimed(t, db, *order.BinID)
	t.Logf("sweep released orphaned claim — bin %d now unclaimed", bin.ID)

	// Step 7: Verify anomalies no longer detect it
	anomaliesAfter, err := db.ListOrderCompletionAnomalies()
	if err != nil {
		t.Fatalf("list anomalies after sweep: %v", err)
	}
	for _, a := range anomaliesAfter {
		if a.OrderID == order.ID && a.Issue == "terminal_order_still_claims_bin" {
			t.Error("anomaly still present after sweep")
		}
	}
	t.Logf("anomalies after sweep: %d (should not include order %d)", len(anomaliesAfter), order.ID)

	// Step 8: Verify FailOrderAtomic prevents the leak entirely
	bin2 := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC80b")
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-tc80b",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order2 := testdb.RequireOrder(t, db, "retrieve-tc80b")
	t.Logf("order2 %d dispatched, bin2 %d claimed", order2.ID, bin2.ID)

	// Fail atomically
	if err := db.FailOrderAtomic(order2.ID, "atomic failure test"); err != nil {
		t.Fatalf("FailOrderAtomic: %v", err)
	}

	// Verify order is failed AND bin is unclaimed in one shot
	order2 = testdb.AssertOrderStatus(t, db, "retrieve-tc80b", dispatch.StatusFailed)

	testdb.AssertBinUnclaimed(t, db, bin2.ID)
	t.Logf("FailOrderAtomic: order %d failed, bin %d unclaimed — no leak possible", order2.ID, bin2.ID)
}

// --- Regression: Bug 1+2 — Bin moves to destination on DELIVERED, not CONFIRMED ---
// Verifies that after fleet reports FINISHED (order transitions to delivered),
// the bin's node_id is already at the delivery node — NOT still at source.
// Prior to fix, bin only moved on confirmed (after Edge round-trip), leaving
// telemetry stale during the delivery→confirmation window.
func TestRegression_BinMovesOnDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REGR-DELIVER")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "regr-deliver-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "regr-deliver-1")

	// Drive to FINISHED — fleet physically delivered the bin
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "regr-deliver-1", "delivered")

	// KEY ASSERTION: bin should already be at the line node BEFORE confirmation
	testdb.AssertBinAtNode(t, db, *order.BinID, lineNode.ID)
	testdb.AssertBinUnclaimed(t, db, *order.BinID)

	// Confirmation should still work (idempotent — bin already there)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "regr-deliver-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "regr-deliver-1", "confirmed")

	// Bin still at line node after confirmation
	testdb.AssertBinAtNode(t, db, *order.BinID, lineNode.ID)
}

// --- Regression: Bug 4 — Cancel with empty EdgeUUID does not notify Edge ---
// Auto-return orders created by Core have no EdgeUUID and are never dispatched
// to the fleet, so cancellation is engine-internal (recovery/timeout) and
// emits EventOrderCancelled directly — not through handleVendorStatusChange.
// The guard at wiring.go:98 prevents sendToEdge with an empty UUID.
func TestRegression_CancelEmptyEdgeUUID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create a real bin for the auto-return order to reference
	bin := createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-REGR-CANCEL")

	// Create an order with empty EdgeUUID (simulates auto-return order)
	autoReturn := &store.Order{
		EdgeUUID:     "",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeStore,
		Status:       dispatch.StatusPending,
		SourceNode:   lineNode.Name,
		DeliveryNode: storageNode.Name,
		BinID:        &bin.ID,
		PayloadDesc:  "auto_return",
	}
	if err := db.CreateOrder(autoReturn); err != nil {
		t.Fatalf("create auto-return order: %v", err)
	}

	// Cancel via event — should NOT send to Edge or panic
	eng.Events.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:        autoReturn.ID,
		EdgeUUID:       "",
		StationID:      "line-1",
		Reason:         "test cancel",
		PreviousStatus: dispatch.StatusPending,
	}})

	// Assertion 1: No cancel message in outbox (Edge was NOT notified)
	var outboxCount int
	err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'order_cancelled'`).Scan(&outboxCount)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if outboxCount != 0 {
		t.Errorf("outbox has %d order_cancelled messages, want 0 — empty EdgeUUID should skip Edge notification", outboxCount)
	}

	// Assertion 2: No auto-return order was created (payload_desc=auto_return prevents loops,
	// but verify it didn't slip through)
	var returnCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM orders WHERE payload_desc = 'auto_return' AND source_node = $1`, storageNode.Name).Scan(&returnCount)
	if err != nil {
		t.Fatalf("query return orders: %v", err)
	}
	if returnCount != 0 {
		t.Errorf("auto-return order was created for an already-return order — loop guard may be broken")
	}
}

// --- Regression: Multi-bin order moves ALL bins on DELIVERED ---
// Verifies that when a complex order has multiple claimed bins (order_bins junction
// rows), ALL bins move to their destinations on fleet FINISHED — not just one.
// The single-bin path is already covered by TestRegression_BinMovesOnDelivered.
func TestRegression_MultiBinMovesOnDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, inboundStaging, outboundStaging, outboundDest, bp := setupProductionNodes(t, db)

	// Two bins: new material at storage, old material at line
	newBin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REGR-MB-NEW")
	oldBin := createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-REGR-MB-OLD")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "regr-multibin-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: inboundStaging.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: outboundStaging.Name},
			{Action: "pickup", Node: inboundStaging.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "pickup", Node: outboundStaging.Name},
			{Action: "dropoff", Node: outboundDest.Name},
		},
	})

	order := testdb.RequireOrder(t, db, "regr-multibin-1")

	// Verify junction table was populated (multi-bin path)
	orderBins, err := db.ListOrderBins(order.ID)
	if err != nil {
		t.Fatalf("list order bins: %v", err)
	}
	if len(orderBins) < 2 {
		t.Fatalf("expected >= 2 order_bins rows, got %d", len(orderBins))
	}

	// Drive to FINISHED — fleet physically delivered
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "regr-multibin-1", "delivered")

	// KEY ASSERTION: both bins should have moved to their resolved destinations.
	// Step simulation: newBin (storage→inbound→line), oldBin (line→outbound-staging→outbound-dest)
	// Final destinations: newBin → lineNode, oldBin → outboundDest.
	testdb.RequireBinAtNode(t, db, newBin.ID, lineNode.ID)
	testdb.RequireBinAtNode(t, db, oldBin.ID, outboundDest.ID)

	// Both bins should be unclaimed after delivery
	testdb.AssertBinUnclaimed(t, db, newBin.ID)
	testdb.AssertBinUnclaimed(t, db, oldBin.ID)

	// Confirmation should work (idempotent — bins already at destinations)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "regr-multibin-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "regr-multibin-1", "confirmed")

	// Verify junction table was cleaned up on completion
	orderBinsAfter, _ := db.ListOrderBins(order.ID)
	if len(orderBinsAfter) != 0 {
		t.Errorf("order_bins rows after confirmation = %d, want 0 (should be cleaned up)", len(orderBinsAfter))
	}
}

// --- Regression: handleOrderCompleted is idempotent for single-bin ---
// Verifies that calling handleOrderCompleted (confirmation) after bins already
// moved on delivery does NOT move them again or cause errors.
func TestRegression_CompletionIdempotentAfterDelivery(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REGR-IDEMP")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "regr-idemp-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "regr-idemp-1")

	// Drive to FINISHED — bin moves to line node
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	binAfterDelivery := testdb.RequireBin(t, db, *order.BinID)

	// Record the bin state after delivery — confirmation must not change it
	nodeAfterDelivery := *binAfterDelivery.NodeID

	// Confirm — handleOrderCompleted runs but should be idempotent
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "regr-idemp-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	binAfterConfirm := testdb.RequireBin(t, db, *order.BinID)

	// Bin should still be at the same node — no double-move
	if binAfterConfirm.NodeID == nil || *binAfterConfirm.NodeID != nodeAfterDelivery {
		t.Errorf("bin moved during confirmation: was at %d, now at %v — completion should be idempotent",
			nodeAfterDelivery, binAfterConfirm.NodeID)
	}
	if binAfterConfirm.ClaimedBy != nil {
		t.Errorf("bin still claimed after confirmation: %v", binAfterConfirm.ClaimedBy)
	}
}