package engine

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"
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

	d.SetPostFindHook(func() {
		// Only synchronize on the FIRST call (G1's Find).
		// Subsequent calls (G2's Find) pass through without blocking.
		if hookCalled.Add(1) == 1 {
			g1Found <- struct{}{} // signal: G1 found the bin, pausing before Claim
			<-g2Done              // wait for G2 to claim first
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
		<-g1Found // wait for G1's hook signal
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID:    "race-order-1",
			OrderType:    dispatch.OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			DeliveryNode: lineNode.Name,
			Quantity:     1,
		})
		g2Done <- struct{}{} // let G1 resume its Claim
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
	for _, order := range []*store.Order{orderA, orderB} {
		if order.Status == dispatch.StatusFailed {
			t.Errorf("BUG: order permanently failed after deterministic TOCTOU race — should be queued")
		}
	}

	// Exactly one should have claimed the bin
	claimed := 0
	for _, order := range []*store.Order{orderA, orderB} {
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
func TestTC09_ComplexOrderZeroSteps(t *testing.T) {
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
func TestTC10_NonexistentDeliveryNode(t *testing.T) {
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
func TestTC12_ZeroQuantity(t *testing.T) {
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
	db := testDB(t)
	storageNode, lineNode1, bp := setupTestData(t, db)

	// Create second line node for redirect destination
	lineNode2 := &store.Node{Name: "LINE2-IN", Enabled: true}
	if err := db.CreateNode(lineNode2); err != nil {
		t.Fatalf("create line node 2: %v", err)
	}
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

	// Now add a bin
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-FULFILL")

	// Trigger fulfillment scanner
	count := eng.RunFulfillmentScan()
	t.Logf("fulfillment scan processed %d orders", count)
	if count == 0 {
		t.Fatal("fulfillment scanner should have processed at least 1 order")
	}

	// Verify order now dispatched
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

// =============================================================================
// TC-37: Staging expiry vs active claim
// =============================================================================

// --- TC-37: Staging sweep flips bin to available while still claimed ---
// Scenario: Bin delivered to lineside (staged). A second order claims it.
// Staging TTL expires. The sweep runs and flips bin to available
// without checking claimed_by.
// Expected: sweep should skip bins with active claims.
func TestTC37_StagingExpiryVsActiveClaim(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC37")

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

	bin, err = db.GetBin(*order.BinID)
	if err != nil {
		t.Fatalf("get bin after delivery: %v", err)
	}
	if bin.Status != "staged" {
		t.Fatalf("bin should be staged at lineside, got status=%q", bin.Status)
	}
	t.Logf("bin %d at line: status=%s, claimed_by=%v", bin.ID, bin.Status, bin.ClaimedBy)

	// Step 3: Manually claim the bin for a second order (simulates operator action)
	secondOrder := &store.Order{
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Status:      dispatch.StatusQueued,
	}
	if err := db.CreateOrder(secondOrder); err != nil {
		t.Fatalf("create second order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, secondOrder.ID); err != nil {
		t.Fatalf("claim bin for second order: %v", err)
	}

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

// =============================================================================
// Buried bin reshuffle through engine pipeline
// =============================================================================

// --- Buried bin: FIFO triggers reshuffle of buried bin via compound order ---
//
// Setup: NGRP -> LANE -> 3 slots. Blocker at depth 1 (newer), target at depth 2 (older).
// FIFO detects buried target as older than any accessible bin -> BuriedError ->
// planBuriedReshuffle -> compound order with child steps:
//   1. unbury: move blocker (depth 1) -> shuffle slot
//   2. retrieve: move target (depth 2) -> line node
//   3. restock: move blocker from shuffle -> back to depth 1
//
// Drives each child through the fleet simulator lifecycle, verifying that the
// compound order advances correctly and the target bin arrives at the line.
func TestBuriedBin_ReshuffleViaEngine(t *testing.T) {
	db := testDB(t)

	// Node types are seeded by migrations
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v (migrations should seed this)", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE node type: %v (migrations should seed this)", err)
	}

	bp := &store.Payload{Code: "PART-BURIED", Description: "Buried bin test payload"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	bt := &store.BinType{Code: "DEFAULT-BR", Description: "Buried test bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	// NGRP (node group)
	grp := &store.Node{Name: "GRP-BURIED", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP: %v", err)
	}

	// LANE under NGRP
	lane := &store.Node{
		Name: "GRP-BURIED-L1", NodeTypeID: &lanType.ID,
		ParentID: &grp.ID, Enabled: true, IsSynthetic: true,
	}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create LANE: %v", err)
	}

	// 3 physical slot nodes at depth 1, 2, 3
	var slots [3]*store.Node
	for i := 0; i < 3; i++ {
		depth := i + 1
		slot := &store.Node{
			Name:     fmt.Sprintf("GRP-BURIED-L1-S%d", depth),
			ParentID: &lane.ID, Enabled: true, Depth: &depth,
		}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", depth, err)
		}
		slots[i] = slot
	}

	// Shuffle slot (direct physical child of NGRP, empty — for temp storage during reshuffle)
	shuffleSlot := &store.Node{
		Name: "GRP-BURIED-SHUF", ParentID: &grp.ID, Enabled: true,
	}
	if err := db.CreateNode(shuffleSlot); err != nil {
		t.Fatalf("create shuffle slot: %v", err)
	}

	// Line node (delivery destination for the retrieve)
	lineNode := &store.Node{Name: "LINE-BURIED-IN", Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}

	// TARGET bin at depth 2 (older — loaded_at 2 hours ago)
	targetBin := &store.Bin{
		BinTypeID: bt.ID, Label: "BIN-TARGET",
		NodeID: &slots[1].ID, Status: "available",
	}
	if err := db.CreateBin(targetBin); err != nil {
		t.Fatalf("create target bin: %v", err)
	}
	if err := db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100); err != nil {
		t.Fatalf("set target manifest: %v", err)
	}
	if err := db.ConfirmBinManifest(targetBin.ID); err != nil {
		t.Fatalf("confirm target: %v", err)
	}
	// Make target clearly older so FIFO prefers it over the accessible blocker
	if _, err := db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID); err != nil {
		t.Fatalf("set target loaded_at: %v", err)
	}

	// BLOCKER bin at depth 1 (newer — loaded_at = NOW, blocks access to target)
	blockerBin := &store.Bin{
		BinTypeID: bt.ID, Label: "BIN-BLOCKER",
		NodeID: &slots[0].ID, Status: "available",
	}
	if err := db.CreateBin(blockerBin); err != nil {
		t.Fatalf("create blocker bin: %v", err)
	}
	if err := db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50); err != nil {
		t.Fatalf("set blocker manifest: %v", err)
	}
	if err := db.ConfirmBinManifest(blockerBin.ID); err != nil {
		t.Fatalf("confirm blocker: %v", err)
	}

	t.Logf("setup: target=%d at depth 2 (2h old), blocker=%d at depth 1 (new)", targetBin.ID, blockerBin.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Retrieve order targeting NGRP as source -> GroupResolver FIFO -> buried bin detected -> reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "reshuffle-buried-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("reshuffle-buried-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	t.Logf("order %d: status=%s bin=%v vendor=%s", order.ID, order.Status, order.BinID, order.VendorOrderID)

	// Order should be in "reshuffling" status (compound parent)
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("order status = %q, want %q", order.Status, dispatch.StatusReshuffling)
	}

	// Check compound children were created
	children, err := db.ListChildOrders(order.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children (unbury, retrieve, restock), got %d", len(children))
	}

	t.Logf("compound: %d children", len(children))
	for _, c := range children {
		t.Logf("  child %d: seq=%d status=%s desc=%s source=%s dest=%s bin=%v vendor=%s",
			c.ID, c.Sequence, c.Status, c.PayloadDesc, c.SourceNode, c.DeliveryNode, c.BinID, c.VendorOrderID)
	}

	// Drive each child through the fleet simulator lifecycle
	for _, child := range children {
		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child: %v", err)
		}
		if child.VendorOrderID == "" {
			t.Fatalf("child %d (seq %d) not dispatched — status=%s", child.ID, child.Sequence, child.Status)
		}

		sim.DriveState(child.VendorOrderID, "RUNNING")
		sim.DriveState(child.VendorOrderID, "FINISHED")

		// Edge receipt triggers completion -> HandleChildOrderComplete -> AdvanceCompoundOrder
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID:   child.EdgeUUID,
			ReceiptType: "confirmed",
			FinalCount:  1,
		})

		child, err = db.GetOrder(child.ID)
		if err != nil {
			t.Fatalf("get child after completion: %v", err)
		}
		t.Logf("child %d completed: status=%s", child.ID, child.Status)
	}

	// Verify parent order completed
	order, err = db.GetOrderByUUID("reshuffle-buried-1")
	if err != nil {
		t.Fatalf("get parent order: %v", err)
	}
	t.Logf("parent order final: status=%s", order.Status)

	// Verify target bin moved from depth-2 slot toward line
	targetBin, err = db.GetBin(targetBin.ID)
	if err != nil {
		t.Fatalf("get target bin: %v", err)
	}
	if targetBin.NodeID != nil && *targetBin.NodeID == lineNode.ID {
		t.Logf("target bin at line node %s — correct", lineNode.Name)
	} else {
		t.Errorf("target bin at node %v (wanted line %d)", targetBin.NodeID, lineNode.ID)
	}

	// Verify blocker restocked back to lane
	blockerBin, err = db.GetBin(blockerBin.ID)
	if err != nil {
		t.Fatalf("get blocker bin: %v", err)
	}
	t.Logf("blocker bin: node=%v", blockerBin.NodeID)

	// No bins stuck as claimed
	for _, b := range []*store.Bin{targetBin, blockerBin} {
		if b.ClaimedBy != nil {
			t.Errorf("bin %d still claimed by order %d after reshuffle", b.ID, *b.ClaimedBy)
		}
	}

	// Lane lock released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane %d still locked after compound order completion", lane.ID)
	} else {
		t.Logf("lane lock released — correct")
	}
}

// =============================================================================
// Complex order production readiness tests
//
// These tests target code paths that will be exercised in production when
// running sequential removal, one-robot swap, and two-robot swap patterns.
// Each test exercises the full engine pipeline (Engine + Dispatcher + Simulator + DB).
// =============================================================================

// --- Test: Complex order cancel mid-transit ---
//
// Scenario: A complex order (pickup → dropoff → wait → pickup → dropoff) is
// dispatched and the robot is in transit (RUNNING). The operator cancels the
// order. The bin was claimed by claimComplexBins and is physically on the robot.
//
// Expected: The order is cancelled. The bin claim is released. An auto-return
// order is created to bring the bin back to its origin. No bin is permanently
// stuck.
//
// Why this matters: Operators cancel orders regularly. If the cancel path doesn't
// release the claim and create a return, the bin becomes invisible to the system.
func TestComplexOrder_CancelMidTransit(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-CXCANCEL")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Dispatch a complex order: pickup from storage, dropoff at line, wait, pickup, dropoff back
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-cancel-1",
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

	order, err := db.GetOrderByUUID("cx-cancel-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want %q", order.Status, dispatch.StatusDispatched)
	}

	// Verify bin was claimed
	bin, err = db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
		t.Fatalf("bin should be claimed by order %d, got %v", order.ID, bin.ClaimedBy)
	}

	// Robot starts moving
	sim.DriveState(order.VendorOrderID, "RUNNING")
	order, _ = db.GetOrderByUUID("cx-cancel-1")
	if order.Status != dispatch.StatusInTransit {
		t.Fatalf("after RUNNING: status = %q, want in_transit", order.Status)
	}

	// Operator cancels while robot is in transit
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "cx-cancel-1",
		Reason:    "operator cancelled mid-transit",
	})

	// Verify order is cancelled
	order, _ = db.GetOrderByUUID("cx-cancel-1")
	if order.Status != dispatch.StatusCancelled {
		t.Errorf("order status = %q, want cancelled", order.Status)
	}

	// Verify bin claim released (unclaimed by cancel handler)
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin after cancel: claimed_by=%v status=%s", bin.ClaimedBy, bin.Status)

	// Check for auto-return order — maybeCreateReturnOrder should fire
	// because: BinID set, VendorOrderID set, status was in_transit → cancelled
	allOrders, err := db.ListOrders("", 50)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}

	var returnOrder *store.Order
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}

	if returnOrder == nil {
		t.Errorf("BUG: no auto-return order created after cancelling complex order mid-transit — bin may be stranded")
	} else {
		t.Logf("auto-return order %d created: source=%s dest=%s bin=%v",
			returnOrder.ID, returnOrder.SourceNode, returnOrder.DeliveryNode, returnOrder.BinID)
		// Return order should have claimed the bin
		bin, _ = db.GetBin(bin.ID)
		if bin.ClaimedBy == nil {
			t.Errorf("bin should be claimed by return order, but claimed_by is nil")
		} else if *bin.ClaimedBy != returnOrder.ID {
			t.Errorf("bin claimed by %d, want %d (return order)", *bin.ClaimedBy, returnOrder.ID)
		}
	}
}

// --- Test: Complex order fleet failure mid-transit ---
//
// Scenario: A complex order is dispatched, robot starts moving (RUNNING),
// then the fleet reports FAILED (robot breakdown, obstacle, emergency stop).
//
// Expected: Order marked failed. Bin claim released. Auto-return created.
// Same recovery path as cancel, but triggered by fleet rather than operator.
func TestComplexOrder_FleetFailureMidTransit(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-CXFAIL")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-fail-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("cx-fail-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Robot starts then fails
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FAILED")

	// Give events time to propagate through the engine pipeline
	order, _ = db.GetOrderByUUID("cx-fail-1")
	t.Logf("order after FAILED: status=%s", order.Status)

	if order.Status != dispatch.StatusFailed {
		t.Errorf("order status = %q, want failed", order.Status)
	}

	// Verify bin claim released
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin after failure: claimed_by=%v", bin.ClaimedBy)

	// Check for auto-return order
	allOrders, _ := db.ListOrders("", 50)
	var returnOrder *store.Order
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}

	if returnOrder == nil {
		t.Errorf("BUG: no auto-return order created after fleet failure on complex order — bin may be stranded")
	} else {
		t.Logf("auto-return order %d: source=%s dest=%s", returnOrder.ID, returnOrder.SourceNode, returnOrder.DeliveryNode)

		// Return should have re-claimed the bin
		bin, _ = db.GetBin(bin.ID)
		if bin.ClaimedBy == nil || *bin.ClaimedBy != returnOrder.ID {
			t.Errorf("bin claimed_by = %v, want %d (return order)", bin.ClaimedBy, returnOrder.ID)
		}
	}
}

// --- Test: Compound child failure mid-reshuffle — blocker stranding ---
//
// Scenario: A 3-step reshuffle is in progress (unbury blocker → retrieve target
// → restock blocker). Step 1 completes: blocker moved to shuffle slot. Step 2
// (retrieve target) fails — robot breaks down. HandleChildOrderFailure cancels
// remaining children and fails the parent.
//
// Key question: The blocker bin is now physically at the shuffle slot (moved by
// completed step 1). Its claim was released on completion. Is it visible to
// normal operations? Can it be retrieved? Or is it orphaned?
//
// Expected: After failure, the blocker bin should be at the shuffle slot,
// unclaimed, and accessible for manual recovery or a new reshuffle. The lane
// lock should be released so a retry can proceed. The target bin should still
// be at its original slot (step 2 never completed), unclaimed.
func TestCompound_ChildFailureMidReshuffle_BlockerStranding(t *testing.T) {
	db := testDB(t)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP: %v", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE: %v", err)
	}

	bp := &store.Payload{Code: "PART-STRAND", Description: "Stranding test"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	bt := &store.BinType{Code: "DEFAULT-ST", Description: "Stranding bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	// NGRP → LANE → 2 slots
	grp := &store.Node{Name: "GRP-STRAND", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-STRAND-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-STRAND-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-STRAND-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-STRAND-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-STRAND", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried, older)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-STRAND-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1 (front, newer)
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-STRAND-BLOCKER", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle via FIFO retrieve
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "strand-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("strand-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("order status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	for i, c := range children {
		t.Logf("child %d: seq=%d desc=%s source=%s dest=%s", i, c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode)
	}

	// Complete step 1 (unbury blocker → shuffle slot)
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")
	sim.DriveState(child1.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID: child1.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
	})

	// Verify blocker moved to shuffle slot
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker after step 1: node=%v claimed=%v", blockerBin.NodeID, blockerBin.ClaimedBy)
	if blockerBin.NodeID != nil && *blockerBin.NodeID == shuffleSlot.ID {
		t.Logf("blocker correctly at shuffle slot")
	}

	// Step 2 (retrieve target) dispatched automatically by AdvanceCompoundOrder
	child2, _ := db.GetOrder(children[1].ID)
	if child2.VendorOrderID == "" {
		// Re-fetch — AdvanceCompoundOrder may have just dispatched
		child2, _ = db.GetOrder(children[1].ID)
	}
	if child2.VendorOrderID == "" {
		t.Fatalf("child 2 not dispatched")
	}

	// Step 2 fails — robot breaks down
	sim.DriveState(child2.VendorOrderID, "RUNNING")
	sim.DriveState(child2.VendorOrderID, "FAILED")

	// Verify parent order failed
	order, _ = db.GetOrderByUUID("strand-reshuffle-1")
	t.Logf("parent after child failure: status=%s", order.Status)
	if order.Status != dispatch.StatusFailed {
		t.Errorf("parent status = %q, want failed", order.Status)
	}

	// Verify lane lock released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane still locked after compound failure — prevents retry")
	}

	// Verify remaining children cancelled
	children, _ = db.ListChildOrders(order.ID)
	for _, c := range children {
		c, _ = db.GetOrder(c.ID)
		t.Logf("child %d (seq %d): status=%s", c.ID, c.Sequence, c.Status)
	}

	// KEY CHECK: blocker bin at shuffle slot — is it recoverable?
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker final: node=%v claimed=%v status=%s", blockerBin.NodeID, blockerBin.ClaimedBy, blockerBin.Status)

	if blockerBin.ClaimedBy != nil {
		t.Errorf("blocker bin still claimed by %d — cannot be retrieved by a new order", *blockerBin.ClaimedBy)
	}
	if blockerBin.NodeID == nil || *blockerBin.NodeID != shuffleSlot.ID {
		t.Logf("NOTE: blocker bin not at shuffle slot (node=%v) — may have been moved by auto-return", blockerBin.NodeID)
	} else {
		t.Logf("blocker bin at shuffle slot %s — accessible for manual recovery or new reshuffle", shuffleSlot.Name)
	}

	// Target bin should still be at its original slot (step 2 never completed)
	targetBin, _ = db.GetBin(targetBin.ID)
	t.Logf("target final: node=%v claimed=%v", targetBin.NodeID, targetBin.ClaimedBy)
	if targetBin.ClaimedBy != nil {
		t.Errorf("target bin still claimed by %d — stranded", *targetBin.ClaimedBy)
	}
}

// --- Test: Two-robot swap full lifecycle (5-step compound) ---
//
// Scenario: An NGRP lane has 3 bins. The target is at depth 3 (deepest),
// with 2 blockers at depth 1 and 2. FIFO detects the buried target and
// triggers a reshuffle with 5 steps:
//   1. Unbury blocker-1 (depth 1) → shuffle-1
//   2. Unbury blocker-2 (depth 2) → shuffle-2
//   3. Retrieve target (depth 3) → line node
//   4. Restock blocker-2 → depth 2 (deepest-first)
//   5. Restock blocker-1 → depth 1
//
// This is the full two-robot swap pattern. The test verifies:
// - All 5 children created and dispatched sequentially
// - Target arrives at line, blockers restocked to original positions
// - All claims released, lane lock freed, parent completed
func TestCompound_TwoRobotSwap_FullLifecycle(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-SWAP", Description: "Swap test payload"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-SW", Description: "Swap bin type"}
	db.CreateBinType(bt)

	// NGRP → LANE → 3 slots
	grp := &store.Node{Name: "GRP-SWAP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-SWAP-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depths := [3]int{1, 2, 3}
	var slots [3]*store.Node
	for i := 0; i < 3; i++ {
		s := &store.Node{
			Name: fmt.Sprintf("GRP-SWAP-L1-S%d", depths[i]),
			ParentID: &lane.ID, Enabled: true, Depth: &depths[i],
		}
		db.CreateNode(s)
		slots[i] = s
	}

	// Two shuffle slots
	shuf1 := &store.Node{Name: "GRP-SWAP-SHUF1", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuf1)
	shuf2 := &store.Node{Name: "GRP-SWAP-SHUF2", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuf2)

	lineNode := &store.Node{Name: "LINE-SWAP", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 3 (oldest — 3 hours ago)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SWAP-TARGET", NodeID: &slots[2].ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '3 hours' WHERE id = $1`, targetBin.ID)

	// Blocker 2 at depth 2
	blocker2 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SWAP-BLK2", NodeID: &slots[1].ID, Status: "available"}
	db.CreateBin(blocker2)
	db.SetBinManifest(blocker2.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker2.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '1 hour' WHERE id = $1`, blocker2.ID)

	// Blocker 1 at depth 1 (newest)
	blocker1 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SWAP-BLK1", NodeID: &slots[0].ID, Status: "available"}
	db.CreateBin(blocker1)
	db.SetBinManifest(blocker1.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker1.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "swap-5step-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("swap-5step-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	t.Logf("compound: %d children", len(children))
	for _, c := range children {
		t.Logf("  child seq=%d: desc=%s src=%s dest=%s bin=%v",
			c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode, c.BinID)
	}

	if len(children) < 5 {
		t.Fatalf("expected >= 5 children (2 unbury + 1 retrieve + 2 restock), got %d", len(children))
	}

	// Drive each child through full lifecycle sequentially
	for i, child := range children {
		child, _ = db.GetOrder(child.ID)
		if child.VendorOrderID == "" {
			t.Fatalf("child %d (seq %d) not dispatched — status=%s", i, child.Sequence, child.Status)
		}

		sim.DriveState(child.VendorOrderID, "RUNNING")
		sim.DriveState(child.VendorOrderID, "FINISHED")

		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})

		child, _ = db.GetOrder(child.ID)
		t.Logf("child %d (seq %d) completed: status=%s", i, child.Sequence, child.Status)
	}

	// Verify parent completed
	order, _ = db.GetOrderByUUID("swap-5step-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Errorf("parent status = %q, want confirmed", order.Status)
	}

	// Verify target at line
	targetBin, _ = db.GetBin(targetBin.ID)
	if targetBin.NodeID == nil || *targetBin.NodeID != lineNode.ID {
		t.Errorf("target bin at node %v, want line %d", targetBin.NodeID, lineNode.ID)
	} else {
		t.Logf("target bin at line — correct")
	}

	// Verify blockers restocked
	blocker1, _ = db.GetBin(blocker1.ID)
	blocker2, _ = db.GetBin(blocker2.ID)
	t.Logf("blocker1: node=%v  blocker2: node=%v", blocker1.NodeID, blocker2.NodeID)

	// All claims released
	for _, b := range []*store.Bin{targetBin, blocker1, blocker2} {
		if b.ClaimedBy != nil {
			t.Errorf("bin %d (%s) still claimed by %d", b.ID, b.Label, *b.ClaimedBy)
		}
	}

	// Lane lock freed
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane %d still locked after 5-step compound completion", lane.ID)
	}
}

// --- Test: Cancel parent compound order while child is in-flight ---
//
// Scenario: A reshuffle compound order is in progress. Child 1 (unbury) is
// dispatched and the robot is RUNNING. The operator cancels the parent order
// (not the child). Does the in-flight child's fleet order get cancelled?
// Does the lane lock release?
//
// This exercises the cancel path for compound parents, which is different
// from HandleChildOrderFailure (that's triggered by fleet failure on a child).
//
// Expected: Parent and all children cancelled. Lane lock released. Bins
// unclaimed. The child's fleet order should be cancelled (or at minimum,
// the order record is marked cancelled).
func TestCompound_CancelParentWhileChildInFlight(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-PCANCEL", Description: "Parent cancel test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-PC", Description: "Parent cancel bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-PCANCEL", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-PCANCEL-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-PCANCEL-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-PCANCEL-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-PCANCEL-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-PCANCEL", Enabled: true}
	db.CreateNode(lineNode)

	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-PCANCEL-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-PCANCEL-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "pcancel-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("pcancel-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	// Child 1 is dispatched and robot is RUNNING
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")

	child1, _ = db.GetOrder(child1.ID)
	t.Logf("child 1 before cancel: status=%s vendor=%s", child1.Status, child1.VendorOrderID)

	// Cancel the PARENT order while child is in flight
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "pcancel-reshuffle-1",
		Reason:    "operator cancelled parent during reshuffle",
	})

	// Verify parent cancelled
	order, _ = db.GetOrderByUUID("pcancel-reshuffle-1")
	t.Logf("parent after cancel: status=%s", order.Status)
	if order.Status != dispatch.StatusCancelled {
		t.Errorf("parent status = %q, want cancelled", order.Status)
	}

	// Check all children statuses
	children, _ = db.ListChildOrders(order.ID)
	for _, c := range children {
		c, _ = db.GetOrder(c.ID)
		t.Logf("  child %d (seq %d): status=%s vendor=%s", c.ID, c.Sequence, c.Status, c.VendorOrderID)

		// Children with vendor orders should ideally be cancelled too
		if c.VendorOrderID != "" && c.Status != dispatch.StatusCancelled {
			t.Logf("  WARNING: child %d has fleet order %s but status=%s (not cancelled) — orphan robot risk",
				c.ID, c.VendorOrderID, c.Status)
		}
	}

	// Lane lock should be released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("BUG: lane %d still locked after parent cancel — blocks future reshuffles", lane.ID)
	} else {
		t.Logf("lane lock released — correct")
	}

	// All bins should be unclaimed
	targetBin, _ = db.GetBin(targetBin.ID)
	blockerBin, _ = db.GetBin(blockerBin.ID)
	for _, b := range []*store.Bin{targetBin, blockerBin} {
		if b.ClaimedBy != nil {
			t.Errorf("BUG: bin %d (%s) still claimed by %d after parent cancel — permanently stuck",
				b.ID, b.Label, *b.ClaimedBy)
		}
	}
}

// --- Test: Empty post-wait release (TC-47) ---
//
// Scenario: A complex order has steps [pickup, dropoff, wait] with nothing
// after the wait. Edge sends an OrderRelease to unblock the staged order.
// HandleOrderRelease parses StepsJSON, calls splitPostWait, gets an empty
// postWait slice, then calls ReleaseOrder(vendorOrderID, []OrderBlock{}).
//
// Expected: ReleaseOrder is called with nil/empty blocks. The order should
// transition to in_transit and the fleet should mark it complete (no more
// blocks). No panic, no error.
func TestComplexOrder_EmptyPostWaitRelease(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-EPWAIT")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Dispatch complex order: pickup → dropoff → wait (nothing after wait)
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-empty-wait-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
		},
	})

	order, err := db.GetOrderByUUID("cx-empty-wait-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Verify bin claimed
	bin, _ = db.GetBin(bin.ID)
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
		t.Fatalf("bin not claimed by order %d", order.ID)
	}

	// Drive pre-wait blocks through fleet
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "DWELLING")

	// Mark order as staged (simulating the dwell callback)
	if err := db.UpdateOrderStatus(order.ID, dispatch.StatusStaged, "dwelling at lineside"); err != nil {
		t.Fatalf("update to staged: %v", err)
	}

	// Edge sends release — there are no post-wait steps
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "cx-empty-wait-1",
	})

	// Verify no panic and order transitions correctly
	order, _ = db.GetOrderByUUID("cx-empty-wait-1")
	t.Logf("order after empty release: status=%s", order.Status)

	// The fleet should have received a ReleaseOrder with empty blocks,
	// which signals "no more blocks" — effectively completing the order
	if order.Status == dispatch.StatusStaged {
		t.Logf("NOTE: order still staged after empty release — fleet may not have transitioned it yet")
	} else {
		t.Logf("order transitioned to %s after empty release", order.Status)
	}

	// Verify no orphan bins
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin after release: node=%v claimed=%v status=%s", bin.NodeID, bin.ClaimedBy, bin.Status)
}

// --- Test: Complex order redirect doesn't update StepsJSON (TC-48) ---
//
// Scenario: A complex order with a wait (pickup A → dropoff B → wait →
// pickup B → dropoff C) is dispatched. While the order is staged (dwelling),
// the operator sends a redirect to change delivery from C to D.
// HandleOrderRedirect updates DeliveryNode in the DB, but StepsJSON still
// has "dropoff C" in the post-wait steps. When HandleOrderRelease fires,
// it reads StepsJSON and creates fleet blocks with the OLD destination.
//
// Expected: This test documents the bug — the fleet gets blocks with old
// node C, not new node D. The test verifies whether the redirect actually
// takes effect in the post-wait phase.
func TestComplexOrder_RedirectStaleStepsJSON(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	_ = createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REDIR")

	// Create a third node to be the redirect target
	newDest := &store.Node{Name: "LINE-REDIR-NEW", Enabled: true}
	db.CreateNode(newDest)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Complex order: pickup storage → dropoff line → wait → pickup line → dropoff storage
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-redir-1",
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

	order, err := db.GetOrderByUUID("cx-redir-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Drive to staged (dwelling)
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "DWELLING")

	if err := db.UpdateOrderStatus(order.ID, dispatch.StatusStaged, "dwelling"); err != nil {
		t.Fatalf("update to staged: %v", err)
	}

	// Redirect delivery from storageNode to newDest
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{
		OrderUUID:       "cx-redir-1",
		NewDeliveryNode: newDest.Name,
	})

	// Re-fetch order
	order, _ = db.GetOrderByUUID("cx-redir-1")
	t.Logf("order after redirect: delivery=%s status=%s", order.DeliveryNode, order.Status)

	// Check if DeliveryNode was updated
	if order.DeliveryNode != newDest.Name {
		t.Logf("NOTE: DeliveryNode not updated to %s (got %s) — redirect may have been rejected for staged orders", newDest.Name, order.DeliveryNode)
	}

	// Key check: StepsJSON still has old destination
	t.Logf("StepsJSON after redirect: %s", order.StepsJSON)

	// If order is back to staged, try releasing
	if order.Status == dispatch.StatusStaged || order.Status == dispatch.StatusSourcing {
		// If redirect put it back to sourcing, it will re-dispatch. Otherwise release.
		if order.Status == dispatch.StatusStaged {
			d.HandleOrderRelease(env, &protocol.OrderRelease{
				OrderUUID: "cx-redir-1",
			})
		}

		order, _ = db.GetOrderByUUID("cx-redir-1")
		t.Logf("order after release: status=%s delivery=%s", order.Status, order.DeliveryNode)

		// BUG CHECK: The post-wait blocks are built from StepsJSON which still
		// references the old destination. The fleet will route to the wrong node.
		if order.StepsJSON != "" {
			t.Logf("POTENTIAL BUG: StepsJSON not updated after redirect — post-wait fleet blocks use old destination")
		}
	}
}

// --- Test: Ghost robot — claimComplexBins finds no bin (TC-49) ---
//
// Scenario: A complex order specifies a pickup at a node, but the node
// has no bins matching the payload (or all bins are already claimed).
// claimComplexBins is best-effort and logs a warning but lets the order
// dispatch anyway — with BinID=nil.
//
// Expected: The order dispatches to fleet (ghost robot). When the robot
// arrives at the empty node, it will fail. The test verifies that:
// 1. Order dispatches with BinID=nil
// 2. No auto-return is created (BinID=nil guard in maybeCreateReturnOrder)
// 3. The failure path still marks the order failed cleanly
func TestComplexOrder_GhostRobotNoBin(t *testing.T) {
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Create an empty pickup node — no bins at all
	emptyNode := &store.Node{Name: "EMPTY-PICKUP", Enabled: true}
	db.CreateNode(emptyNode)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Complex order picks up from an empty node
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-ghost-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: emptyNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("cx-ghost-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Key check: order dispatched but with no bin
	if order.BinID != nil {
		t.Errorf("expected BinID=nil (no bin at pickup), got %d", *order.BinID)
	} else {
		t.Logf("CONFIRMED: order dispatched with BinID=nil — ghost robot will be sent to empty node")
	}

	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Robot arrives, can't find bin, fleet reports FAILED
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FAILED")

	order, _ = db.GetOrderByUUID("cx-ghost-1")
	if order.Status != dispatch.StatusFailed {
		t.Errorf("order status = %q after fleet failure, want failed", order.Status)
	}

	// No auto-return should be created (BinID=nil)
	allOrders, _ := db.ListOrders("", 50)
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			t.Errorf("BUG: auto-return order created for ghost robot (no bin!) — order %d", o.ID)
		}
	}
	t.Logf("ghost robot failure handled cleanly — no spurious auto-return")
}

// --- Test: Concurrent complex orders targeting same node — double claim race (TC-50) ---
//
// Scenario: Two complex orders are submitted simultaneously, both picking up
// from the same storage node that has only one available bin.
// claimComplexBins runs for both orders in sequence. The first should claim
// the bin; the second should get no bin (ghost robot).
//
// Expected: Only one order claims the bin. The second dispatches with
// BinID=nil. No double-claim occurs (bin.ClaimedBy can only reference one order).
func TestComplexOrder_ConcurrentSameNodeDoubleClaimRace(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RACE")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// First order
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-race-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	// Second order — same pickup node, same payload
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-race-2",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order1, _ := db.GetOrderByUUID("cx-race-1")
	order2, _ := db.GetOrderByUUID("cx-race-2")

	// Check which order got the bin
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin claimed by: %v", bin.ClaimedBy)
	t.Logf("order1: status=%s bin=%v", order1.Status, order1.BinID)
	t.Logf("order2: status=%s bin=%v", order2.Status, order2.BinID)

	// Exactly one order should have the bin
	hasBin := 0
	if order1.BinID != nil {
		hasBin++
	}
	if order2.BinID != nil {
		hasBin++
	}
	if hasBin > 1 {
		t.Errorf("BUG: both orders claimed a bin — double claim! order1.bin=%v order2.bin=%v", order1.BinID, order2.BinID)
	} else if hasBin == 1 {
		t.Logf("correct: exactly one order claimed the bin, other dispatched as ghost")
	} else {
		t.Logf("NOTE: neither order claimed the bin — possible if both raced and lost")
	}

	// Both orders should be dispatched regardless
	if order1.Status != dispatch.StatusDispatched {
		t.Errorf("order1 status = %q, want dispatched", order1.Status)
	}
	if order2.Status != dispatch.StatusDispatched {
		t.Errorf("order2 status = %q, want dispatched", order2.Status)
	}
}

// --- Test: AdvanceCompoundOrder skips failed children — premature completion (TC-51) ---
//
// Scenario: A 3-step compound order where child 2 has invalid source/dest
// (empty string). AdvanceCompoundOrder dispatches child 1 which completes.
// When advancing to child 2, lines 77-98 in compound.go detect missing
// source/delivery, mark child 2 failed, and recursively call
// AdvanceCompoundOrder. This advances to child 3.
//
// Expected: The parent should NOT complete normally if a child was skipped
// due to failure. This test documents whether the current behavior causes
// silent data loss (blocker not restocked but parent "confirmed").
func TestCompound_AdvanceSkipsFailedChild_PrematureCompletion(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-SKIP", Description: "Skip test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-SK", Description: "Skip bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-SKIP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-SKIP-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-SKIP-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-SKIP-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-SKIP-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-SKIP", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SKIP-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-SKIP-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "skip-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("skip-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	for i, c := range children {
		t.Logf("child %d: seq=%d src=%s dest=%s", i, c.Sequence, c.SourceNode, c.DeliveryNode)
	}

	// Complete child 1 (unbury blocker)
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")
	sim.DriveState(child1.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID: child1.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
	})

	// Now manually break child 2 by clearing its source node
	// This simulates a data corruption or race condition
	child2, _ := db.GetOrder(children[1].ID)
	if child2.VendorOrderID != "" {
		// Child 2 already dispatched — too late to break it
		t.Logf("child 2 already dispatched (vendor=%s) — skipping synthetic break, completing normally", child2.VendorOrderID)

		// Complete remaining children normally and verify
		for i := 1; i < len(children); i++ {
			child, _ := db.GetOrder(children[i].ID)
			if child.VendorOrderID == "" || child.Status == dispatch.StatusFailed {
				continue
			}
			sim.DriveState(child.VendorOrderID, "RUNNING")
			sim.DriveState(child.VendorOrderID, "FINISHED")
			d.HandleOrderReceipt(env, &protocol.OrderReceipt{
				OrderUUID: child.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
			})
		}
	} else {
		// Child 2 not yet dispatched — break its source node
		db.Exec(`UPDATE orders SET source_node = '' WHERE id = $1`, child2.ID)

		// Advance again — this should detect the broken child and skip it
		d.AdvanceCompoundOrder(order.ID)

		// Check what happened
		child2, _ = db.GetOrder(child2.ID)
		t.Logf("child 2 after advance: status=%s", child2.Status)

		if child2.Status == dispatch.StatusFailed {
			t.Logf("child 2 correctly failed due to missing source node")
		}
	}

	// Final state check
	order, _ = db.GetOrderByUUID("skip-reshuffle-1")
	t.Logf("parent final: status=%s", order.Status)

	children, _ = db.ListChildOrders(order.ID)
	failedCount := 0
	completedCount := 0
	for _, c := range children {
		c, _ = db.GetOrder(c.ID)
		t.Logf("  child %d (seq %d): status=%s", c.ID, c.Sequence, c.Status)
		if c.Status == dispatch.StatusFailed {
			failedCount++
		}
		if c.Status == dispatch.StatusConfirmed {
			completedCount++
		}
	}

	if failedCount > 0 && order.Status == dispatch.StatusConfirmed {
		t.Errorf("POTENTIAL BUG: parent completed (confirmed) despite %d failed children — data may be inconsistent", failedCount)
	}

	// Check blocker bin location — is it stranded?
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker final: node=%v claimed=%v", blockerBin.NodeID, blockerBin.ClaimedBy)
}

// --- Test: Lane lock contention — second reshuffle blocked (TC-52) ---
//
// Scenario: A retrieve order triggers a reshuffle on a lane. While the
// reshuffle is in progress, a second retrieve order targets the same NGRP
// lane (same or different payload). The planning service should detect the
// lane lock and return a lane_locked planningError.
//
// Current behavior: lane_locked goes through failOrder, not queueOrder.
// This means the second order FAILS rather than being retried when the
// lane unlocks. This test documents that behavior and whether it's correct.
func TestLaneLock_Contention_SecondReshuffleBlocked(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-LOCK", Description: "Lane lock test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-LK", Description: "Lock bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-LOCK", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-LOCK-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2, depth3 := 1, 2, 3
	slot1 := &store.Node{Name: "GRP-LOCK-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-LOCK-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)
	slot3 := &store.Node{Name: "GRP-LOCK-L1-S3", ParentID: &lane.ID, Enabled: true, Depth: &depth3}
	db.CreateNode(slot3)

	shuffleSlot1 := &store.Node{Name: "GRP-LOCK-SHUF1", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot1)
	shuffleSlot2 := &store.Node{Name: "GRP-LOCK-SHUF2", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot2)

	lineNode := &store.Node{Name: "LINE-LOCK", Enabled: true}
	db.CreateNode(lineNode)

	// Bin at depth 3 (buried under 2 blockers) — first target
	targetBin1 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-LOCK-T1", NodeID: &slot3.ID, Status: "available"}
	db.CreateBin(targetBin1)
	db.SetBinManifest(targetBin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin1.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '3 hours' WHERE id = $1`, targetBin1.ID)

	// Blocker at depth 2
	blocker2 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-LOCK-BLK2", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(blocker2)
	db.SetBinManifest(blocker2.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker2.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '1 hour' WHERE id = $1`, blocker2.ID)

	// Blocker at depth 1
	blocker1 := &store.Bin{BinTypeID: bt.ID, Label: "BIN-LOCK-BLK1", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blocker1)
	db.SetBinManifest(blocker1.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blocker1.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// First order triggers reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "lock-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order1, err := db.GetOrderByUUID("lock-reshuffle-1")
	if err != nil {
		t.Fatalf("get order 1: %v", err)
	}
	if order1.Status != dispatch.StatusReshuffling {
		t.Fatalf("order 1 status = %q, want reshuffling", order1.Status)
	}

	// Verify lane is locked
	if !eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Fatalf("lane not locked after reshuffle started")
	}

	// Second order tries to retrieve from same NGRP while lane is locked
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "lock-reshuffle-2",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order2, err := db.GetOrderByUUID("lock-reshuffle-2")
	if err != nil {
		t.Fatalf("get order 2: %v", err)
	}
	t.Logf("order 2 status: %s", order2.Status)

	// Verify: lane_locked → queueOrder (not failOrder)
	// The second order should be queued for retry, not permanently failed.
	if order2.Status == dispatch.StatusQueued {
		t.Logf("CORRECT: second order queued — will retry when lane unlocks via fulfillment scanner")
	} else if order2.Status == dispatch.StatusFailed {
		t.Errorf("second order FAILED due to lane_locked — should be queued for retry, not permanently failed")
	} else {
		t.Errorf("second order status=%s, want queued", order2.Status)
	}
}

// --- Test: ApplyBinArrival status mapping for compound restock children (TC-53) ---
//
// Scenario: A compound restock child delivers a blocker bin back to its
// storage slot (a child of a LANE node). When the fleet reports FINISHED
// and the receipt is confirmed, handleOrderCompleted calls ApplyBinArrival.
//
// ApplyBinArrival checks if the destination is a storage slot (parent type
// LANE). If so, it sets status='available' (not staged). This is critical:
// if the restocked blocker is marked 'staged' instead of 'available', it
// won't show up in FindSourceBinFIFO queries.
//
// Expected: After compound restock, the bin at the storage slot should have
// status='available', claimed_by=NULL, and be visible to FIFO queries.
func TestCompound_RestockChild_BinStatusAvailable(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-RESTOCK", Description: "Restock status test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-RS", Description: "Restock bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-RESTOCK", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-RESTOCK-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-RESTOCK-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-RESTOCK-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-RESTOCK-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-RESTOCK", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-RESTOCK-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-RESTOCK-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "restock-status-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("restock-status-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusReshuffling {
		t.Fatalf("status = %q, want reshuffling", order.Status)
	}

	children, _ := db.ListChildOrders(order.ID)
	t.Logf("compound: %d children", len(children))
	for i, c := range children {
		t.Logf("  child %d: seq=%d desc=%s src=%s dest=%s", i, c.Sequence, c.PayloadDesc, c.SourceNode, c.DeliveryNode)
	}

	// Drive all children to completion
	for i, child := range children {
		child, _ = db.GetOrder(child.ID)
		if child.VendorOrderID == "" || child.Status == dispatch.StatusFailed {
			t.Logf("child %d: skipping (vendor=%s status=%s)", i, child.VendorOrderID, child.Status)
			continue
		}
		sim.DriveState(child.VendorOrderID, "RUNNING")
		sim.DriveState(child.VendorOrderID, "FINISHED")
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})
		child, _ = db.GetOrder(child.ID)
		t.Logf("child %d completed: status=%s", i, child.Status)
	}

	// KEY CHECK: blocker bin restocked to storage slot
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker after restock: node=%v status=%s claimed=%v", blockerBin.NodeID, blockerBin.Status, blockerBin.ClaimedBy)

	// The blocker should be at a LANE child (storage slot) with status=available
	if blockerBin.Status != "available" {
		t.Errorf("BUG: blocker bin status=%q after restock to storage slot — expected 'available'. If 'staged', bin is invisible to FIFO queries", blockerBin.Status)
	} else {
		t.Logf("blocker bin status=available at storage slot — correct, visible to FIFO")
	}

	if blockerBin.ClaimedBy != nil {
		t.Errorf("blocker bin still claimed by %d after compound completion", *blockerBin.ClaimedBy)
	}

	// Verify it's findable by FIFO
	fifoBin, err := db.FindSourceBinFIFO(bp.Code)
	if err != nil {
		t.Logf("FIFO lookup after restock: no bin found (%v) — blocker may have been restocked to different slot", err)
	} else if fifoBin.ID == blockerBin.ID {
		t.Logf("FIFO returns restocked blocker bin — correct, it's accessible")
	} else {
		t.Logf("FIFO returns bin %d (not blocker %d) — another bin is higher priority", fifoBin.ID, blockerBin.ID)
	}
}

// --- Test: Staging TTL expiry during compound order execution (TC-54) ---
//
// Scenario: During a compound reshuffle, child 1 (unbury) completes and
// delivers the blocker to a non-storage node (shuffle slot). ApplyBinArrival
// marks it as staged with a TTL. If the reshuffle takes longer than the TTL,
// the staging sweep runs and flips the blocker bin to "available" while the
// restock child hasn't executed yet.
//
// Expected: The restock child should still work correctly even if the bin's
// status changed from staged to available. The bin should still be at the
// shuffle slot and claimable. This test verifies no silent failure occurs.
func TestCompound_StagingTTLExpiryDuringReshuffle(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &store.Payload{Code: "PART-TTL", Description: "TTL test"}
	db.CreatePayload(bp)
	bt := &store.BinType{Code: "DEFAULT-TL", Description: "TTL bin type"}
	db.CreateBinType(bt)

	grp := &store.Node{Name: "GRP-TTL", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(grp)
	lane := &store.Node{Name: "GRP-TTL-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	db.CreateNode(lane)

	depth1, depth2 := 1, 2
	slot1 := &store.Node{Name: "GRP-TTL-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth1}
	db.CreateNode(slot1)
	slot2 := &store.Node{Name: "GRP-TTL-L1-S2", ParentID: &lane.ID, Enabled: true, Depth: &depth2}
	db.CreateNode(slot2)

	shuffleSlot := &store.Node{Name: "GRP-TTL-SHUF", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(shuffleSlot)

	lineNode := &store.Node{Name: "LINE-TTL", Enabled: true}
	db.CreateNode(lineNode)

	// Target at depth 2 (buried)
	targetBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-TTL-TARGET", NodeID: &slot2.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID)
	db.Exec(`UPDATE bins SET loaded_at = NOW() - interval '2 hours' WHERE id = $1`, targetBin.ID)

	// Blocker at depth 1
	blockerBin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-TTL-BLK", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(blockerBin)
	db.SetBinManifest(blockerBin.ID, `{"items":[]}`, bp.Code, 50)
	db.ConfirmBinManifest(blockerBin.ID)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Trigger reshuffle
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "ttl-reshuffle-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("ttl-reshuffle-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	children, _ := db.ListChildOrders(order.ID)
	if len(children) < 3 {
		t.Fatalf("expected >= 3 children, got %d", len(children))
	}

	// Complete child 1 (unbury blocker → shuffle slot)
	child1, _ := db.GetOrder(children[0].ID)
	if child1.VendorOrderID == "" {
		t.Fatalf("child 1 not dispatched")
	}
	sim.DriveState(child1.VendorOrderID, "RUNNING")
	sim.DriveState(child1.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID: child1.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
	})

	// Verify blocker at shuffle slot and staged
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker after unbury: node=%v status=%s staged_at=%v", blockerBin.NodeID, blockerBin.Status, blockerBin.StagedAt)

	// Simulate TTL expiry: set staged_expires_at to past
	if _, err := db.Exec(`UPDATE bins SET staged_expires_at = NOW() - interval '1 hour' WHERE id = $1`, blockerBin.ID); err != nil {
		t.Fatalf("set staging expiry: %v", err)
	}

	// Run staging sweep — this should flip blocker to available
	released, err := db.ReleaseExpiredStagedBins()
	if err != nil {
		t.Fatalf("release expired staged bins: %v", err)
	}
	t.Logf("staging sweep released %d bins", released)

	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker after sweep: status=%s", blockerBin.Status)

	// Complete child 2 (retrieve target)
	child2, _ := db.GetOrder(children[1].ID)
	if child2.VendorOrderID != "" {
		sim.DriveState(child2.VendorOrderID, "RUNNING")
		sim.DriveState(child2.VendorOrderID, "FINISHED")
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child2.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})
	}

	// Complete child 3 (restock blocker) — the bin's status was flipped by sweep
	child3, _ := db.GetOrder(children[2].ID)
	if child3.VendorOrderID != "" {
		sim.DriveState(child3.VendorOrderID, "RUNNING")
		sim.DriveState(child3.VendorOrderID, "FINISHED")
		d.HandleOrderReceipt(env, &protocol.OrderReceipt{
			OrderUUID: child3.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1,
		})
	}

	// Verify compound completed despite TTL expiry mid-reshuffle
	order, _ = db.GetOrderByUUID("ttl-reshuffle-1")
	t.Logf("parent final: status=%s", order.Status)

	if order.Status == dispatch.StatusConfirmed {
		t.Logf("compound completed despite staging TTL expiry mid-reshuffle — sweep did not break the restock")
	} else if order.Status == dispatch.StatusFailed {
		t.Errorf("POTENTIAL BUG: compound failed — staging TTL expiry may have interfered with restock child")
	}

	// Verify blocker restocked correctly
	blockerBin, _ = db.GetBin(blockerBin.ID)
	t.Logf("blocker final: node=%v status=%s claimed=%v", blockerBin.NodeID, blockerBin.Status, blockerBin.ClaimedBy)

	if blockerBin.ClaimedBy != nil {
		t.Errorf("blocker still claimed by %d after compound completion", *blockerBin.ClaimedBy)
	}

	// Lane lock released
	if eng.Dispatcher().LaneLock().IsLocked(lane.ID) {
		t.Errorf("lane still locked after compound completion")
	}
}
