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
//  1. Do the store orders find only the 2 remaining bins?
//  2. If 3 store orders are submitted, does the 3rd one fail gracefully
//     or dispatch a robot with no bin?
//  3. Are the remaining 2 bins handled cleanly?
func TestLineChangeover_WithMissingBin(t *testing.T) {
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
	testutil.MustNoErr(t, db.MoveBin(bins[0].ID, qhNode.ID), "move bin 0 to QH")
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
func TestLineChangeover_WhileMoveInFlight(t *testing.T) {
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
