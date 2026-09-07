package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// The pickup's UOP zero, and the reader that saw the ghost.
//
// ClearActiveBin used to leave remaining_uop_cached holding the DEPARTED
// bin's count. Every reader in the pickup→delivery window then saw material
// that was no longer at the slot. This file pins the departure write itself
// and the one exposed reader named when the change was made:
// flipTargetReady's changeover arm ("holds no material to feed the line").

// TestBinPickedUp_ZeroesTheCountWithTheBin pins the departure write: a bin
// picked up at the slot leaves the runtime reading UOP zero, atomically with
// the pointer clear. Pre-fix the count froze at the departed bin's last value.
func TestBinPickedUp_ZeroesTheCountWithTheBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "PICK-ZERO",
		PayloadCode: "PART-PZ",
		UOPCapacity: 100,
		InitialUOP:  40,
	})

	const binID int64 = 21001
	bid := binID
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 40), "seed a half-drawn bin")

	const orderUUID = "uuid-pick-zero"
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "PICK-ZERO-NODE", "", "", "", false, "PART-PZ")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	eng.HandleBinPickedUp(orderUUID, binID, "PICK-ZERO-NODE")

	rtRt, errRt := db.GetProcessNodeRuntime(nodeID)
	rt := testutil.Must(t, rtRt, errRt, "load node runtime")
	if rt.ActiveBinID != nil {
		t.Errorf("ActiveBinID = %v, want nil (the bin left the slot)", rt.ActiveBinID)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOPCached = %d, want 0 — the count was the departed bin's; held over it reads as stock in the pickup→delivery window",
			rt.RemainingUOPCached)
	}
	// The claim and the cell-busy pointer are not the pickup's to clear.
	if rt.ActiveClaimID == nil || *rt.ActiveClaimID != claimID {
		t.Errorf("ActiveClaimID = %v, want %d (the claim survives the carrier)", rt.ActiveClaimID, claimID)
	}
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != orderID {
		t.Errorf("ActiveOrderID = %v, want %d — orderWorksTheCell owns this pointer, not the pickup",
			rt.ActiveOrderID, orderID)
	}
}

// TestBinPickedUp_FlipReadinessReadsTheZeroedCount verifies the one exposed
// reader: flipTargetReady's changeover arm asks "does this position hold
// material" of remaining_uop_cached. Under the zeroed count the answer is the
// honest "no" — the position is bare until its carrier lands — instead of
// inheriting the departed bin's ghost count as phantom material.
func TestBinPickedUp_FlipReadinessReadsTheZeroedCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, nodeID, toStyleID, fromClaimID, _ := seedDirectChangeover(t, db)
	ctx := releaseCtx(t, eng, db, processID, nodeID, toStyleID)

	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")

	// The changeover's material order delivered, the runtime points at the
	// incoming style's claim — and the OLD carrier was picked up, zeroing the
	// count. The new carrier has not landed. Pre-fix the row still carried the
	// old carrier's count and the arm could read phantom material.
	markOrderTerminal(db, ctx.order.ID)
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &fromClaimID, 47), "seed the pre-pickup ghost")

	// Reproduce the pickup's effect through the production door: the handler
	// gated on bin identity, so bind the bin first, then pick it up.
	const binID int64 = 4600
	bid := binID
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bid), "bind the departing bin")
	orderUUID := ctx.order.UUID
	_ = orderUUID
	// Drive ClearActiveBin through the inventory sink exactly as the handler
	// does (handler_bin_picked_up.go), which is the write under test.
	if err := eng.inventoryDelta.ClearActiveBin(nodeID); err != nil {
		t.Fatalf("clear active bin: %v", err)
	}

	rtRt, errRt := db.GetProcessNodeRuntime(nodeID)
	rt := testutil.Must(t, rtRt, errRt, "load node runtime")
	if rt.RemainingUOPCached != 0 {
		t.Fatalf("fixture: RemainingUOPCached = %d, want 0 after the pickup's clear", rt.RemainingUOPCached)
	}

	reason := eng.flipTargetReady(node)
	if !strings.Contains(reason, "holds no material") {
		t.Errorf("flip readiness = %q, want the no-material refusal — the zeroed count must read as a bare position, not phantom stock", reason)
	}
}
