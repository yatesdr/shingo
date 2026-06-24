//go:build docker

package engine

// TestComplexSourcingWindow_RecoveryPath documents and pins the two-sweep
// recovery sequence for the orphaned-hold window in complex_dispatch.go
// (between ApplyComplexPlan at line ~442 and MoveToSourcing at line ~482).
//
// After a crash inside that window:
//   - order is in StatusQueued with bins.claimed_by = order.ID
//   - ReleaseOrphanedClaims alone is insufficient: it only sweeps bins claimed
//     by terminal orders, and StatusQueued is not terminal
//   - AbandonStuckOrders alone is insufficient: it moves the order to terminal,
//     but does not reach into the bins table
//   - the correct sequence is: AbandonStuckOrders → (order terminal) →
//     ReleaseOrphanedClaims → (bin unclaimed)
//
// The test seeds this state directly, bypassing DispatchPreparedComplex, to
// pin the recovery contract independently of dispatch internals.

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	recon "shingocore/store/reconciliation"
	"shingocore/store/orders"
)

func TestComplexSourcingWindow_RecoveryPath(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	// Seed the orphaned-hold window state: a queued order with a claimed bin.
	// This mirrors what ApplyComplexPlan leaves behind on a process crash.
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-WINDOW-RECON")
	order := &orders.Order{
		EdgeUUID:     "window-recon-1",
		StationID:    "line-1",
		OrderType:    "complex",
		Status:       "queued",
		SourceNode:   storageNode.Name,
		DeliveryNode: storageNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	_, err := db.Exec(`UPDATE bins SET claimed_by = $1, updated_at = NOW() WHERE id = $2`, order.ID, bin.ID)
	testutil.MustNoErr(t, err, "claim bin (simulate ApplyComplexPlan side-effect)")

	// Age the order past the AbandonStuckOrders cutoff.
	_, err = db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID)
	testutil.MustNoErr(t, err, "backdate order")

	// Leg 1: ReleaseOrphanedClaims must NOT sweep the bin — StatusQueued is not terminal.
	// If it did, a legitimately-queued order that happens to have a claimed bin
	// (e.g. a retry mid-dispatch) would lose its claim spuriously.
	released, err := recon.ReleaseOrphanedClaims(db.DB)
	testutil.MustNoErr(t, err, "ReleaseOrphanedClaims (before abandon)")
	if released != 0 {
		t.Errorf("ReleaseOrphanedClaims before abandon: released %d bin(s), want 0 (order is queued, not terminal)", released)
	}

	// Leg 2: AbandonStuckOrders moves the aged queued order to a terminal status.
	// The test callback directly sets status=cancelled so ReleaseOrphanedClaims can
	// see it as terminal. Production wires this to LifecycleService.CancelOrder.
	svc := newReconService(t, db)
	svc.abandonOrder = func(o *orders.Order, reason string) error {
		_, err := db.Exec(`UPDATE orders SET status='cancelled', updated_at=NOW() WHERE id=$1`, o.ID)
		return err
	}
	abandoned, err := svc.AbandonStuckOrders(time.Hour)
	testutil.MustNoErr(t, err, "AbandonStuckOrders")
	if abandoned != 1 {
		t.Errorf("AbandonStuckOrders: abandoned %d order(s), want 1", abandoned)
	}

	// Leg 3: ReleaseOrphanedClaims now finds the bin claimed by the cancelled
	// (terminal) order and releases it.
	released, err = recon.ReleaseOrphanedClaims(db.DB)
	testutil.MustNoErr(t, err, "ReleaseOrphanedClaims (after abandon)")
	if released < 1 {
		t.Errorf("ReleaseOrphanedClaims after abandon: released %d bin(s), want >= 1", released)
	}

	// Verify the bin is unclaimed.
	var claimedBy *int64
	err = db.QueryRow(`SELECT claimed_by FROM bins WHERE id = $1`, bin.ID).Scan(&claimedBy)
	testutil.MustNoErr(t, err, "query bin claimed_by")
	if claimedBy != nil {
		t.Errorf("bin %d still claimed by %d after full recovery sweep", bin.ID, *claimedBy)
	}
}
