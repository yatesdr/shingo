//go:build docker

package engine

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// =============================================================================
// Concurrency tests — deterministic and stress
//
// The PostFindHook on PlanningService inserts a synchronization point
// between FindSourceBinFIFO and ClaimBin, allowing tests to
// guarantee the TOCTOU race in the planning path.
// =============================================================================

// --- TestConcurrent_ClaimRaceDeterministic ---
// Uses PostFindHook to guarantee the TOCTOU race between Find and Claim.
//
// Setup: 1 bin at storage. Two orders target the same bin.
// Goroutine 1 hits the hook after Find, pauses. Goroutine 2 starts
// and finds the same bin. Goroutine 2 claims the bin. Goroutine 1
// resumes and tries to claim -- gets claim_failed -> queued.
//
// This test is 100% deterministic. The hook guarantees both goroutines
// enter the TOCTOU window simultaneously.
func TestConcurrent_ClaimRaceDeterministic(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RACE")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	// Flow:
	//   G1: HandleOrderRequest → Find → hook fires → signal g1Found → wait for g2Done → resume Claim (fails)
	//   G2: wait for g1Found → HandleOrderRequest → Find → Claim (succeeds) → signal g2Done
	//
	// This guarantees G2 claims the bin while G1 is paused between Find and Claim,
	// so G1's subsequent Claim returns claim_failed, which queues the order.
	g1Found := make(chan struct{})
	g2Done := make(chan struct{})
	var hookCalled atomic.Int32

	timeout := time.After(10 * time.Second)

	d.SetPostFindHook(func() {
		// Only synchronize on the FIRST call (G1's Find).
		// Subsequent calls (G2's Find) pass through without blocking.
		if hookCalled.Add(1) == 1 {
			select {
			case g1Found <- struct{}{}: // signal: G1 found the bin, pausing before Claim
			case <-timeout:
				t.Error("timeout: G1 blocked sending g1Found signal")
				return
			}
			select {
			case <-g2Done: // wait for G2 to claim first
			case <-timeout:
				t.Error("timeout: G1 blocked waiting for g2Done")
				return
			}
		}
	})

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: calls HandleOrderRequest directly. Hook fires between Find
	// and Claim, pausing G1 until G2 has claimed the bin.
	go func() {
		defer wg.Done()
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID:    "race-order-0",
			OrderType:    dispatch.OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			DeliveryNode: lineNode.Name,
			Quantity:     1,
		})
	}()

	// Goroutine 2: waits for G1 to find the bin, then dispatches and claims it.
	go func() {
		defer wg.Done()
		select {
		case <-g1Found: // wait for G1's hook signal
		case <-timeout:
			t.Error("timeout: G2 blocked waiting for g1Found")
			return
		}
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID:    "race-order-1",
			OrderType:    dispatch.OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			DeliveryNode: lineNode.Name,
			Quantity:     1,
		})
		select {
		case g2Done <- struct{}{}: // let G1 resume its Claim
		case <-timeout:
			t.Error("timeout: G2 blocked sending g2Done signal")
			return
		}
	}()

	wg.Wait()

	// Check results
	orderA, err := db.GetOrderByUUID("race-order-0")
	if err != nil {
		t.Fatalf("get order A: %v", err)
	}
	orderB, err := db.GetOrderByUUID("race-order-1")
	if err != nil {
		t.Fatalf("get order B: %v", err)
	}

	t.Logf("order A: status=%s bin=%v vendor=%s", orderA.Status, orderA.BinID, orderA.VendorOrderID)
	t.Logf("order B: status=%s bin=%v vendor=%s", orderB.Status, orderB.BinID, orderB.VendorOrderID)

	// Neither order should be permanently failed
	for _, order := range []*orders.Order{orderA, orderB} {
		if order.Status == dispatch.StatusFailed {
			t.Errorf("BUG: order permanently failed after deterministic TOCTOU race — should be queued")
		}
	}

	// Exactly one should have claimed the bin
	claimed := 0
	for _, order := range []*orders.Order{orderA, orderB} {
		if order.BinID != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Errorf("expected exactly 1 order to claim the bin, got %d", claimed)
	}
}

// --- TestConcurrent_DispatchStress ---
// Statistical verification: many concurrent dispatches competing for bins.
func TestConcurrent_DispatchStress(t *testing.T) {
	t.Parallel()
	old := runtime.GOMAXPROCS(runtime.NumCPU())
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })

	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create 10 bins at storage
	for i := 0; i < 10; i++ {
		label := fmt.Sprintf("BIN-STRESS-%d", i)
		createTestBinAtNode(t, db, bp.Code, storageNode.ID, label)
	}

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// 20 concurrent orders targeting the same payload
	simulator.ParallelGroup(20, func(i int) {
		uuid := fmt.Sprintf("stress-order-%d", i)
		d.HandleOrderRequest(env, &protocol.OrderRequest{
			OrderUUID:    uuid,
			OrderType:    dispatch.OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			DeliveryNode: lineNode.Name,
			Quantity:     1,
		})
	})

	// Verify: each bin claimed by at most 1 order
	claimedBins := map[int64]int{} // binID -> count
	for i := 0; i < 10; i++ {
		label := fmt.Sprintf("BIN-STRESS-%d", i)
		bin, err := db.GetBinByLabel(label)
		if err != nil {
			t.Fatalf("get bin %s: %v", label, err)
		}
		if bin.ClaimedBy != nil {
			claimedBins[bin.ID]++
		}
	}
	for binID, count := range claimedBins {
		if count > 1 {
			t.Errorf("BUG: bin %d claimed by %d orders (double dispatch)", binID, count)
		}
	}

	// Verify: no orders permanently failed
	for i := 0; i < 20; i++ {
		uuid := fmt.Sprintf("stress-order-%d", i)
		order, err := db.GetOrderByUUID(uuid)
		if err != nil {
			continue // order may not exist if creation failed
		}
		if order.Status == dispatch.StatusFailed {
			t.Errorf("BUG: order %s permanently failed under stress (status=%s)", uuid, order.Status)
		}
	}
}

// =============================================================================
// Malformed input tests (TC-9/10/12)
// =============================================================================

// --- TC-9: Complex order with zero steps ---
// Scenario: Edge sends a complex order request with no steps.
// Expected: order fails gracefully, no panic, no fleet orders.
func TestComplexOrder_ZeroSteps(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "tc9-empty-steps",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps:       []protocol.ComplexOrderStep{},
	})

	order, err := db.GetOrderByUUID("tc9-empty-steps")
	if err != nil {
		// Order was never created — the handler rejected it before persisting.
		// This is acceptable behavior (graceful rejection).
		t.Logf("order not created (rejected before persist): %v", err)
		if sim.OrderCount() > 0 {
			t.Errorf("BUG: fleet received %d orders for a zero-step complex order (expected 0)", sim.OrderCount())
		}
		return
	}
	t.Logf("order status=%s bin=%v vendor=%s", order.Status, order.BinID, order.VendorOrderID)

	// Order should NOT be dispatched to fleet
	if sim.OrderCount() > 0 {
		t.Errorf("BUG: fleet received %d orders for a zero-step complex order (expected 0)", sim.OrderCount())
	}
	if order.Status == dispatch.StatusDispatched {
		t.Errorf("BUG: zero-step order dispatched to fleet (status=%s)", order.Status)
	}
}

// --- TC-10: Order references nonexistent delivery node ---
// Scenario: Retrieve order with DeliveryNode that doesn't exist in the database.
// Expected: order fails with clear error, no fleet orders.
func TestComplexOrder_NonexistentDeliveryNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC10")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "tc10-bad-node",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: "NOSUCH-NODE-XYZ",
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("tc10-bad-node")
	if err != nil {
		// Order may not have been created at all — lifecycle rejected it before persisting
		t.Logf("order not created (rejected by lifecycle) — correct")
		return
	}
	t.Logf("order status=%s bin=%v vendor=%s", order.Status, order.BinID, order.VendorOrderID)

	if sim.OrderCount() > 0 {
		t.Errorf("BUG: fleet received order for nonexistent node (expected 0)")
	}
	if order.Status == dispatch.StatusDispatched {
		t.Errorf("BUG: order dispatched to nonexistent node (status=%s)", order.Status)
	}
}

// --- TC-12: Order requests zero quantity ---
// Scenario: Retrieve order with quantity=0.
// Expected: system handles gracefully — no panic.
func TestComplexOrder_ZeroQuantity(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC12")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "tc12-zero-qty",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     0,
	})

	order, err := db.GetOrderByUUID("tc12-zero-qty")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	t.Logf("order status=%s bin=%v vendor=%s", order.Status, order.BinID, order.VendorOrderID)
}

// =============================================================================
// Redirect mid-transit
// =============================================================================

// --- Redirect: order redirected while robot is in transit ---
// Scenario: Dispatch retrieve, drive to RUNNING (in_transit), redirect to different line.
// Expected: old vendor order cancelled, new one dispatched, bin claim intact.
func TestRedirect_MidTransit(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode1, bp := setupTestData(t, db)

	// Create second line node for redirect destination
	lineNode2 := &nodes.Node{Name: "LINE2-IN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(lineNode2), "create line node 2")
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REDIR")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Dispatch retrieve to LINE1-IN
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "redirect-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode1.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("redirect-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("order status = %q, want dispatched", order.Status)
	}
	claimedBinID := *order.BinID
	t.Logf("order %d dispatched: bin=%d, vendor=%s", order.ID, claimedBinID, order.VendorOrderID)

	// Step 2: Drive to RUNNING (in_transit)
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order, err = db.GetOrderByUUID("redirect-1")
	if err != nil {
		t.Fatalf("get order after RUNNING: %v", err)
	}
	if order.Status != dispatch.StatusInTransit {
		t.Fatalf("after RUNNING: status = %q, want in_transit", order.Status)
	}

	// Step 3: Redirect to LINE2-IN
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{
		OrderUUID:       "redirect-1",
		NewDeliveryNode: lineNode2.Name,
	})

	order, err = db.GetOrderByUUID("redirect-1")
	if err != nil {
		t.Fatalf("get order after redirect: %v", err)
	}
	t.Logf("order after redirect: status=%s bin=%v vendor=%s", order.Status, order.BinID, order.VendorOrderID)

	// Bin claim should be intact
	if order.BinID == nil {
		t.Fatalf("order lost bin claim after redirect")
	}
	if *order.BinID != claimedBinID {
		t.Errorf("bin changed after redirect: got %d, want %d", *order.BinID, claimedBinID)
	}
	bin, err := db.GetBin(claimedBinID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if bin.ClaimedBy != nil && *bin.ClaimedBy == order.ID {
		t.Logf("bin %d claim intact after redirect (claimed_by=%d)", claimedBinID, *bin.ClaimedBy)
	} else {
		t.Errorf("bin claim state after redirect: claimed_by=%v (expected order %d)", bin.ClaimedBy, order.ID)
	}
}

// =============================================================================
// Fulfillment scanner round-trip
// =============================================================================

// --- Fulfillment scanner: queued order dispatched when bin becomes available ---
// Scenario: Order queued (no bins). Bin appears. Scanner picks it up and dispatches.
func TestFulfillmentScanner_QueueToDispatch(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	// NO bins created — order should queue

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "fulfill-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("fulfill-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusQueued {
		t.Fatalf("order should be queued (no bins), got status=%s", order.Status)
	}
	if sim.OrderCount() != 0 {
		t.Fatalf("fleet should have 0 orders, got %d", sim.OrderCount())
	}
	t.Logf("order queued (no bins available) — correct")

	// Now add a bin — this fires EventBinUpdated, which triggers an async
	// fulfillment scan via the engine's event wiring. The explicit scan below
	// acts as a safety net in case the goroutine hasn't run yet.
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-FULFILL")

	eng.RunFulfillmentScan()

	// Verify order now dispatched (may have been fulfilled by the event-driven
	// scan or by the explicit one above — either way is correct).
	order, err = db.GetOrderByUUID("fulfill-1")
	if err != nil {
		t.Fatalf("get order after scan: %v", err)
	}
	t.Logf("order after scan: status=%s bin=%v vendor=%s", order.Status, order.BinID, order.VendorOrderID)
	if order.Status != dispatch.StatusDispatched {
		t.Errorf("BUG: order not dispatched after fulfillment scan (status=%s)", order.Status)
	}
	if order.BinID == nil {
		t.Errorf("BUG: order has no bin after fulfillment scan")
	} else {
		t.Logf("order dispatched with bin %d after fulfillment scan", *order.BinID)
	}

	// Drive lifecycle to completion
	sim.DriveSimpleLifecycle(order.VendorOrderID)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "fulfill-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order, err = db.GetOrderByUUID("fulfill-1")
	if err != nil {
		t.Fatalf("get order after receipt: %v", err)
	}
	if order.Status != dispatch.StatusConfirmed {
		t.Fatalf("after receipt: status = %q, want confirmed", order.Status)
	}
	bin, err := db.GetBin(*order.BinID)
	if err != nil {
		t.Fatalf("get bin after completion: %v", err)
	}
	if bin.NodeID == nil || *bin.NodeID != lineNode.ID {
		t.Errorf("bin at wrong node after completion")
	} else {
		t.Logf("bin correctly at %s after fulfillment round-trip", lineNode.Name)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("BUG: bin still claimed after completion")
	} else {
		t.Logf("bin claim released — correct")
	}
}

// --- Staging sweep flips bin to available while still claimed ---
// Scenario: Bin delivered to lineside (staged). A second order claims it.
// Staging TTL expires. The sweep runs and flips bin to available
// without checking claimed_by.
// Expected: sweep should skip bins with active claims.
func TestStagingExpiry_DoesNotExpireActiveClaim(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	// createTestBinAtNode's returned bin is refetched below after delivery
	// (line ~546 via db.GetBin), so its initial value is never read.
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC37")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Dispatch and deliver bin to lineside
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "tc37-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("tc37-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("order status = %q, want dispatched", order.Status)
	}

	// Step 2: Drive through FINISHED and receipt
	sim.DriveSimpleLifecycle(order.VendorOrderID)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "tc37-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	bin, err := db.GetBin(*order.BinID)
	if err != nil {
		t.Fatalf("get bin after delivery: %v", err)
	}
	if bin.Status != "staged" {
		t.Fatalf("bin should be staged at lineside, got status=%q", bin.Status)
	}
	t.Logf("bin %d at line: status=%s, claimed_by=%v", bin.ID, bin.Status, bin.ClaimedBy)

	// Step 3: Manually claim the bin for a second order (simulates operator action)
	secondOrder := &orders.Order{
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Status:       dispatch.StatusQueued,
	}
	testutil.MustNoErr(t, db.CreateOrder(secondOrder), "create second order")
	testdb.ClaimBinForTest(t, db, bin.ID, secondOrder.ID)

	// Set staging expiry to past
	if _, err := db.Exec(`UPDATE bins SET staged_expires_at = NOW() - interval '1 hour' WHERE id = $1`, bin.ID); err != nil {
		t.Fatalf("set staging expiry: %v", err)
	}

	// Step 4: Run staging sweep
	released, err := db.ReleaseExpiredStagedBins()
	if err != nil {
		t.Fatalf("release expired staged bins: %v", err)
	}
	t.Logf("staging sweep released %d bins", released)

	// Step 5: Check bin state
	bin, err = db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin after sweep: %v", err)
	}
	t.Logf("bin after sweep: status=%s, claimed_by=%v", bin.Status, bin.ClaimedBy)
	if bin.Status == "available" && bin.ClaimedBy != nil {
		t.Errorf("BUG: staging sweep flipped bin to available while still claimed by order %d — "+
			"ReleaseExpiredStagedBins should check claimed_by IS NULL", *bin.ClaimedBy)
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
func TestConcurrentRetrieve_SamePart(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Two storage nodes, each with one bin of PART-A
	storageNode1 := &nodes.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(storageNode1), "create storage node 1")
	storageNode2 := &nodes.Node{Name: "STORAGE-A2", Zone: "A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(storageNode2), "create storage node 2")

	// Two line nodes (two different production lines)
	lineNode1 := &nodes.Node{Name: "LINE1-IN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(lineNode1), "create line node 1")
	lineNode2 := &nodes.Node{Name: "LINE2-IN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(lineNode2), "create line node 2")

	bp := &payloads.Payload{Code: "PART-A", Description: "Steel bracket tote"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")
	bt := &bins.BinType{Code: "DEFAULT", Description: "Default test bin type"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

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

func TestRetrieveClaimFailure_QueueSurvives(t *testing.T) {
	t.Parallel()
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
	for _, order := range []*orders.Order{orderA, orderB} {
		if order.Status == dispatch.StatusFailed {
			t.Errorf("BUG: order %s permanently failed after claim_failed — should be queued for retry",
				order.EdgeUUID)
		}
	}

	// One should be dispatched, the other queued (not failed, not sourcing)
	dispatched := 0
	queued := 0
	for _, order := range []*orders.Order{orderA, orderB} {
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
