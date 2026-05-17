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

// --- TC-21: Only available bin is in quality hold ---
// Scenario: verifies that the system does not dispatch a bin in quality hold.
//
// A line requests a part. The only bin of that part in the warehouse is in
// quality hold (flagged for inspection). The system should not dispatch it.
// The order should be queued, not failed — so the fulfillment scanner can
// pick it up later when inventory frees up.
func TestDispatch_QualityHoldBin_QueuedNotFailed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a single bin at storage, then put it in quality hold
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-QH")
	testutil.MustNoErr(t, db.UpdateBinStatus(bin.ID, "quality_hold"), "set bin to quality_hold")
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
