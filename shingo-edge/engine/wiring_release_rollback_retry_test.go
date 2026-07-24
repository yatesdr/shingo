package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
)

// testEngineWithOrderBridge builds an engine whose order manager emits onto the
// engine's own event bus — the production wiring (adapters.go orderEmitter) —
// instead of testEngine's NoOpOrderEmitter. That makes a status transition
// driven through the order manager actually fire EventOrderDelivered and bind
// the runtime, so the P2-C2 failed-release retry tests can observe the whole
// delivered→bind handoff end to end rather than in two disconnected halves.
func testEngineWithOrderBridge(t *testing.T, db *store.DB) *Engine {
	t.Helper()
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	return eng
}

// runFailedReleaseRetry drives one failed-release rollback path end to end:
// a leg that Core bounced (rolled back to staged) is retried, Core re-delivers
// with the bin snapshot on the envelope, and the runtime must bind the bin at
// the snapshot count. rollback is the exact order-manager method the matching
// edge_handler.go arm invokes (RollbackForRetry for manifest_sync_failed,
// RollbackReleaseRejection for invalid_state).
func runFailedReleaseRetry(t *testing.T, rollback func(m *orders.Manager, uuid, detail string) error) {
	t.Helper()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REL-RETRY", PayloadCode: "PART-REL", UOPCapacity: 200, InitialUOP: 0,
	})

	// Unbound slot: the release moved the leg toward dispatch and nothing is
	// bound at the node yet (the incident's staged-but-unbound window).
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "clear active bin")

	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	const uuid = "uuid-rel-retry"
	const newBinID int64 = 512
	orderID, err := db.CreateOrder(uuid, orders.TypeRetrieve, &nodeID, false, 1,
		node.CoreNodeName, "", "", "", false, "PART-REL")
	testutil.MustNoErr(t, err, "create order")
	// The release click already ran on Edge, so the leg is in_transit when Core
	// bounces it — the precondition RollbackReleaseRejection requires, and a
	// realistic state for RollbackForRetry too.
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "set in_transit")

	eng := testEngineWithOrderBridge(t, db)

	// The failed release rolls the leg back to staged for a retry.
	testutil.MustNoErr(t, rollback(eng.orderMgr, uuid, "release failed, click release to retry"), "rollback")
	got, err := db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "get order after rollback")
	if got.Status != orders.StatusStaged {
		t.Fatalf("after rollback: status=%s, want staged (recoverable, not failed)", got.Status)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime after rollback")
	if rt.ActiveBinID != nil {
		t.Fatalf("after rollback: ActiveBinID=%v, want nil (still unbound, awaiting retry)", rt.ActiveBinID)
	}

	// Successful retry: Core re-delivers, carrying the bin's count snapshot on
	// the OrderDelivered envelope (uop=175, epoch=7).
	const snapshotUOP = 175
	bid := newBinID
	uop := snapshotUOP
	testutil.MustNoErr(t,
		eng.orderMgr.HandleDeliveredWithExpiry(uuid, "delivered on retry", nil, &bid, &uop, 7, node.CoreNodeName, ""),
		"handle delivered on retry")

	// The delivered event fired and bound the runtime to the new bin at the
	// snapshot count — the failed-release rollback did not leave the bin
	// permanently unbindable.
	got, err = db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "get order after retry")
	if got.Status != orders.StatusDelivered {
		t.Errorf("after retry: status=%s, want delivered", got.Status)
	}
	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime after retry")
	if rt.ActiveBinID == nil || *rt.ActiveBinID != newBinID {
		t.Errorf("after retry: ActiveBinID=%v, want %d (bin bound on redelivery)", rt.ActiveBinID, newBinID)
	}
	if rt.RemainingUOPCached != snapshotUOP {
		t.Errorf("after retry: RemainingUOPCached=%d, want %d (count snapshot from the delivered envelope)",
			rt.RemainingUOPCached, snapshotUOP)
	}
	if rt.ActiveBinEpoch != 7 {
		t.Errorf("after retry: ActiveBinEpoch=%d, want 7 (epoch from the delivered envelope)", rt.ActiveBinEpoch)
	}
}

// TestReleaseRetry_ManifestSyncFailed_RebindsOnRetry pins the manifest_sync_failed
// rollback path (edge_handler.go): Core couldn't sync the bin manifest at release,
// the leg rolls back to staged, and a successful retry re-delivers and binds the
// bin at the count snapshot. Verifies the recoverable-error path never strands a
// bin unbindable.
func TestReleaseRetry_ManifestSyncFailed_RebindsOnRetry(t *testing.T) {
	t.Parallel()
	runFailedReleaseRetry(t, (*orders.Manager).RollbackForRetry)
}

// TestReleaseRetry_InvalidState_RebindsOnRetry pins the invalid_state rollback
// path (edge_handler.go): Core rejected the release precondition (the ALN_003
// divergence family), the in_transit leg rolls back to staged, and a successful
// retry re-delivers and binds. Verifies the release-rejection path is likewise
// recoverable into a bound state.
func TestReleaseRetry_InvalidState_RebindsOnRetry(t *testing.T) {
	t.Parallel()
	runFailedReleaseRetry(t, (*orders.Manager).RollbackReleaseRejection)
}
