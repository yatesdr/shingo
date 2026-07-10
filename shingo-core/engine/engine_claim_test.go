//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
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
	order1 := testdb.CreateOrder(t, db)
	order2 := testdb.CreateOrder(t, db)

	// Order 1 claims the bin (reserve-then-claim, as the demoted-CAS guard requires)
	testdb.ReserveBin(t, db, order1.ID, bin.ID)
	testutil.MustNoErr(t, db.ClaimBin(bin.ID, order1.ID), "first ClaimBin")

	// Verify claim is set
	testdb.RequireBinClaimedBy(t, db, bin.ID, order1.ID)

	// Order 2 tries to claim the same bin — must fail on the claimed_by IS NULL guard.
	err := db.ClaimBin(bin.ID, order2.ID)
	if err == nil {
		// Regression: second claim silently overwrote the first.
		bin, _ = db.GetBin(bin.ID)
		t.Errorf("BUG: ClaimBin(bin=%d, order=%d) succeeded — silently overwrote claim from order %d. claimed_by is now %v",
			bin.ID, order2.ID, order1.ID, *bin.ClaimedBy)
	} else {
		t.Logf("ClaimBin correctly rejected second claim: %v", err)
	}
}

// --- TC-24: Complex order bin poaching ---
// Regression: complex orders never call ClaimBin, leaving the bin's
// claimed_by field NULL even while a robot is physically carrying it.
// A concurrent order could then claim the same bin.
//
// This is most realistic with empty bins: a complex order moves an empty
// bin from storage to a line, and while the robot is in transit, another
// order sources the same payload and claims the in-flight bin.
//
// Setup: 1 bin at storage. Complex order picks it up (robot RUNNING).
// Then a retrieve order for the same payload tries to source a bin.
// Expected (current behavior): retrieve claims the same bin — BUG.
// Expected (fixed): retrieve finds no free bin and does not poach.
//
// (Originally the poacher was a plain store order; store dispatch was
// removed, so this drives a retrieve — the same source-claim-exclusion path.)
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

	// Now: a retrieve order for the same payload arrives. Its only candidate
	// source is the bin at storage the complex order is carrying; because that
	// bin is claimed, the retrieve must NOT poach it. It delivers to a fresh
	// line node so its own dropoff-capacity gate is clear and it is forced
	// through source-finding (the complex order is already inbound to lineNode).
	line2 := &nodes.Node{Name: "LINE2-IN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line2), "create second line node")
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-poach-24",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: line2.Name,
		Quantity:     1,
	})

	poachOrder := testdb.RequireOrder(t, db, "retrieve-poach-24")

	// KEY CHECK: the retrieve must NOT have claimed the bin the robot is carrying
	if poachOrder.BinID != nil && *poachOrder.BinID == bin.ID {
		t.Errorf("BUG: retrieve order %d claimed bin %d which is being carried by complex order %d",
			poachOrder.ID, bin.ID, complexOrder.ID)
	} else {
		t.Logf("retrieve order correctly did not poach bin %d (status=%s, bin_id=%v)",
			bin.ID, poachOrder.Status, poachOrder.BinID)
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
