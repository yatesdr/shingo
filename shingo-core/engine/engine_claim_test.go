//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

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
	testutil.MustNoErr(t, db.ClaimBin(bin.ID, 100), "first ClaimBin")

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

// --- TC-23a: Operator tries to move a claimed bin via a second store order ---
// Scenario: verifies that store orders cannot steal bins from active orders.
//
// Line has 3 bins. Bin 0 is already claimed by an active store order (robot
// in transit to move it). The operator creates another store order at the same
// line. The second order should skip the claimed bin and only take unclaimed ones.
func TestMoveBin_StoreOrderCannotStealClaimedBin(t *testing.T) {
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
func TestStoreOrder_ClaimsStagedBinAtCoreNode(t *testing.T) {
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
func TestComplexOrder_BinPoachingPrevention(t *testing.T) {
	t.Parallel()
	// complex orders now claim bins at dispatch, preventing poaching.
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
		t.Fatalf("complex order has BinID=nil — ApplyComplexPlan did not set it")
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
func TestComplexOrder_StaleBinLocation(t *testing.T) {
	t.Parallel()
	// complex orders now set BinID, so ApplyBinArrival fires on
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
		t.Fatalf("complex order BinID=nil — ApplyComplexPlan did not set it")
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
func TestComplexOrder_PhantomInventoryRetrieve(t *testing.T) {
	t.Parallel()
	// with the bin claimed and BinID set, ApplyBinArrival runs on
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
		t.Fatalf("complex order BinID=nil — ApplyComplexPlan did not set it")
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
