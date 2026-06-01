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

// Simulates the production bug where a bin (Core_Testing30001) shows as "claimed"
// on the Nodes and Bins pages but has no visible active orders. Root cause:
// UpdateOrderStatus(failed) and UnclaimOrderBins are separate SQL statements.
// If unclaim fails silently (connection drop, deadlock), the bin stays claimed
// forever by a terminal order.
//
// The fix adds FailOrderAtomic / CancelOrderAtomic that wrap status + unclaim
// in a single transaction. This test verifies:
//  1. Reconciliation anomalies detect orphaned claims
//  2. ReleaseOrphanedClaims sweep fixes them
//  3. FailOrderAtomic prevents the leak in the first place
func TestOrphanedBinClaim_ReconciliationDetectsAndFixes(t *testing.T) {
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
	testutil.MustNoErr(t, db.FailOrderAtomic(order2.ID, "atomic failure test"), "FailOrderAtomic")

	// Verify order is failed AND bin is unclaimed in one shot
	order2 = testdb.AssertOrderStatus(t, db, "retrieve-tc80b", dispatch.StatusFailed)

	testdb.AssertBinUnclaimed(t, db, bin2.ID)
	t.Logf("FailOrderAtomic: order %d failed, bin %d unclaimed — no leak possible", order2.ID, bin2.ID)
}
