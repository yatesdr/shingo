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
