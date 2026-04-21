//go:build docker

package engine

import (
	"sync/atomic"
	"testing"
	"time"

	"shingocore/store"
)

// reconciliation_service_test.go — coverage tests for
// reconciliation_service.go.
//
// ReconciliationService is a thin layer over *store.DB:
//
//   newReconciliationService — constructor, copies deps and zero-inits hook
//   Summary                  — delegates to DB.GetReconciliationSummary
//   ListAnomalies            — delegates to DB.ListReconciliationAnomalies
//   ListRecoveryActions      — delegates to DB.ListRecoveryActions
//   RequeueOutbox            — delegates to DB.RequeueOutbox
//   ListDeadLetterOutbox     — delegates to DB.ListDeadLetterOutbox
//   Loop                     — ticker-driven summary+auto-confirm; timing
//                              tested by the explicit AutoConfirm* method
//                              below to keep the test deterministic.
//   AutoConfirmStuckDeliveredOrders — flips delivered→confirmed past
//                              a timeout and invokes onOrderCompleted.
//
// Tests focus on seeding DB state (stuck orders, expired staged bins,
// dead-lettered outbox rows) and asserting the returned data matches.
// Auto-confirm tests bypass the ticker in Loop so the timing is explicit.

// newReconService builds a bare ReconciliationService wired to a fresh DB
// without spinning the full Engine. Mirrors the edge-side harness.
func newReconService(t *testing.T, db *store.DB) *ReconciliationService {
	t.Helper()
	return newReconciliationService(db, t.Logf)
}

// ── constructor ─────────────────────────────────────────────────────

func TestNewReconciliationService_WiresDeps(t *testing.T) {
	db := testDB(t)
	svc := newReconciliationService(db, t.Logf)
	if svc == nil {
		t.Fatal("newReconciliationService returned nil")
	}
	if svc.db != db {
		t.Error("db not stored on service")
	}
	if svc.logFn == nil {
		t.Error("logFn should be wired")
	}
	if svc.onOrderCompleted != nil {
		t.Error("onOrderCompleted should default to nil (wired by Engine.New)")
	}
}

// ── Summary — fresh DB ──────────────────────────────────────────────

func TestReconciliationService_Summary_FreshDB(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)
	summary, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary == nil {
		t.Fatal("summary is nil")
	}
	if summary.Status != "ok" {
		t.Errorf("fresh DB status = %q, want ok", summary.Status)
	}
	if summary.TotalAnomalies != 0 {
		t.Errorf("TotalAnomalies = %d, want 0", summary.TotalAnomalies)
	}
	if summary.DeadLetters != 0 {
		t.Errorf("DeadLetters = %d, want 0", summary.DeadLetters)
	}
	if summary.OutboxPending != 0 {
		t.Errorf("OutboxPending = %d, want 0", summary.OutboxPending)
	}
}

// ── Summary — degraded by stuck order ───────────────────────────────

func TestReconciliationService_Summary_StuckOrderDegrades(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)
	svc := newReconService(t, db)

	// Seed a dispatched order and backdate updated_at past the stuck-age
	// threshold (30 minutes). Must be older than stuckOrderAge in
	// store/reconciliation.go.
	order := &store.Order{
		EdgeUUID:     "stuck-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "dispatched",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate order: %v", err)
	}

	summary, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.StuckOrders < 1 {
		t.Errorf("StuckOrders = %d, want >= 1", summary.StuckOrders)
	}
	if summary.Status != "degraded" && summary.Status != "critical" {
		t.Errorf("status = %q, want degraded or critical", summary.Status)
	}
}

// ── Summary — critical by dead letter ───────────────────────────────

func TestReconciliationService_Summary_DeadLetterCritical(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)

	// Enqueue one outbox row and push it past MaxOutboxRetries.
	if err := db.EnqueueOutbox("t1", []byte(`{"msg":1}`), "test.event", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Fetch its ID — EnqueueOutbox doesn't return one.
	pending, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending row, got %d", len(pending))
	}
	id := pending[0].ID
	for i := 0; i < store.MaxOutboxRetries; i++ {
		if err := db.IncrementOutboxRetries(id); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}

	summary, err := svc.Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.DeadLetters != 1 {
		t.Errorf("DeadLetters = %d, want 1", summary.DeadLetters)
	}
	if summary.Status != "critical" {
		t.Errorf("status = %q, want critical", summary.Status)
	}
}

// ── ListAnomalies ───────────────────────────────────────────────────

func TestReconciliationService_ListAnomalies_Empty(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)
	anomalies, err := svc.ListAnomalies()
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	if len(anomalies) != 0 {
		t.Errorf("fresh DB anomalies = %d, want 0", len(anomalies))
	}
}

func TestReconciliationService_ListAnomalies_StuckOrder(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)
	svc := newReconService(t, db)

	order := &store.Order{
		EdgeUUID:  "anom-uuid",
		StationID: "line-1",
		OrderType: "retrieve",
		Status:    "dispatched",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	anomalies, err := svc.ListAnomalies()
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	found := false
	for _, a := range anomalies {
		if a.Issue == "active_order_stuck" && a.OrderID != nil && *a.OrderID == order.ID {
			found = true
			if a.Category != "order_runtime" {
				t.Errorf("anomaly category = %q, want order_runtime", a.Category)
			}
			if a.RecommendedAction != "cancel_stuck_order" {
				t.Errorf("recommended action = %q", a.RecommendedAction)
			}
			break
		}
	}
	if !found {
		t.Errorf("stuck order not surfaced in anomalies: %+v", anomalies)
	}
}

func TestReconciliationService_ListAnomalies_ExpiredStagedBin(t *testing.T) {
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	svc := newReconService(t, db)

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "STAGED-1")
	// Stage the bin with an already-past expiration. StageBin uses the ts arg.
	past := time.Now().Add(-10 * time.Minute)
	if err := db.StageBin(bin.ID, &past); err != nil {
		t.Fatalf("stage bin: %v", err)
	}

	anomalies, err := svc.ListAnomalies()
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	found := false
	for _, a := range anomalies {
		if a.Issue == "staged_bin_expired" && a.BinID != nil && *a.BinID == bin.ID {
			found = true
			if a.Category != "bin_staging" {
				t.Errorf("category = %q, want bin_staging", a.Category)
			}
			break
		}
	}
	if !found {
		t.Errorf("expired staged bin not surfaced in anomalies: %+v", anomalies)
	}
}

// ── RequeueOutbox + ListDeadLetterOutbox ────────────────────────────

func TestReconciliationService_ListDeadLetterAndRequeue(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)

	if err := db.EnqueueOutbox("t1", []byte(`{"msg":"dl"}`), "test", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	pending, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending row, got %d", len(pending))
	}
	id := pending[0].ID
	for i := 0; i < store.MaxOutboxRetries; i++ {
		if err := db.IncrementOutboxRetries(id); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}

	dead, err := svc.ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != id {
		t.Fatalf("dead-letter = %+v, want [%d]", dead, id)
	}

	// RequeueOutbox zeros the retries — the row moves off the dead-letter list.
	if err := svc.RequeueOutbox(id); err != nil {
		t.Fatalf("RequeueOutbox: %v", err)
	}
	dead2, err := svc.ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox after requeue: %v", err)
	}
	if len(dead2) != 0 {
		t.Errorf("after requeue, dead-letter list should be empty, got %d", len(dead2))
	}
	// And it should be pending again.
	pending2, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list pending after requeue: %v", err)
	}
	if len(pending2) != 1 || pending2[0].ID != id {
		t.Errorf("after requeue, pending list = %+v, want [%d]", pending2, id)
	}
}

// ── ListRecoveryActions ─────────────────────────────────────────────

func TestReconciliationService_ListRecoveryActions(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)

	// Empty → empty list.
	acts, err := svc.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("ListRecoveryActions: %v", err)
	}
	if len(acts) != 0 {
		t.Errorf("fresh DB should have no actions, got %d", len(acts))
	}

	// Seed two rows directly via the store — the service should surface both,
	// newest-first (DESC by id per the store query).
	if err := db.RecordRecoveryAction("release_claim", "order", 1, "first", "sys"); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := db.RecordRecoveryAction("auto_confirm_delivered", "order", 2, "second", "sys"); err != nil {
		t.Fatalf("record 2: %v", err)
	}

	acts, err = svc.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("ListRecoveryActions: %v", err)
	}
	if len(acts) != 2 {
		t.Fatalf("got %d actions, want 2", len(acts))
	}
	// Newest first: action 2 precedes action 1.
	if acts[0].TargetID != 2 || acts[1].TargetID != 1 {
		t.Errorf("order = [%d, %d], want [2, 1]", acts[0].TargetID, acts[1].TargetID)
	}

	// Limit parameter is forwarded to DB.
	limited, err := svc.ListRecoveryActions(1)
	if err != nil {
		t.Fatalf("ListRecoveryActions(limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 returned %d rows", len(limited))
	}
}

// ── AutoConfirmStuckDeliveredOrders ─────────────────────────────────

func TestReconciliationService_AutoConfirm_NoTimeout(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)
	// timeout <= 0 is a no-op and must never touch the DB.
	n, err := svc.AutoConfirmStuckDeliveredOrders(0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	n, err = svc.AutoConfirmStuckDeliveredOrders(-1 * time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestReconciliationService_AutoConfirm_NothingDelivered(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)
	// Fresh DB — no delivered rows, must return 0.
	n, err := svc.AutoConfirmStuckDeliveredOrders(1 * time.Second)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 on empty DB", n)
	}
}

func TestReconciliationService_AutoConfirm_ConfirmsStuckDelivered(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)
	svc := newReconService(t, db)

	// Seed a delivered order with updated_at well past the timeout.
	order := &store.Order{
		EdgeUUID:     "auto-confirm-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Wire the onOrderCompleted hook so we assert it's invoked.
	var hookCount atomic.Int64
	var hookOrderID atomic.Int64
	svc.onOrderCompleted = func(orderID int64, edgeUUID, stationID string) {
		hookCount.Add(1)
		hookOrderID.Store(orderID)
	}

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 1 {
		t.Errorf("confirmed count = %d, want 1", n)
	}

	// Verify the status transition and completed_at.
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "confirmed" {
		t.Errorf("order status = %q, want confirmed", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be set after auto-confirm")
	}

	// Hook fired once with the right order ID.
	if hookCount.Load() != 1 {
		t.Errorf("onOrderCompleted invoked %d times, want 1", hookCount.Load())
	}
	if hookOrderID.Load() != order.ID {
		t.Errorf("hook order id = %d, want %d", hookOrderID.Load(), order.ID)
	}

	// A recovery_actions row is written for audit.
	acts, err := db.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	found := false
	for _, a := range acts {
		if a.Action == "auto_confirm_delivered" && a.TargetID == order.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no recovery_actions row for auto_confirm_delivered on order %d: %+v", order.ID, acts)
	}
}

func TestReconciliationService_AutoConfirm_SkipsFreshDelivered(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)
	svc := newReconService(t, db)

	// A delivered order updated moments ago — too fresh for auto-confirm.
	order := &store.Order{
		EdgeUUID:     "fresh-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 0 {
		t.Errorf("confirmed count = %d, want 0 (too fresh)", n)
	}
	// Status untouched.
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != "delivered" {
		t.Errorf("status = %q, want delivered (unchanged)", got.Status)
	}
	if got.CompletedAt != nil {
		t.Error("CompletedAt should not be set when skipped")
	}
}

func TestReconciliationService_AutoConfirm_SkipsNonDelivered(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)
	svc := newReconService(t, db)

	// A stuck but not-delivered order — query only matches status='delivered'.
	order := &store.Order{
		EdgeUUID:  "ndo-uuid",
		StationID: "line-1",
		OrderType: "retrieve",
		Status:    "in_transit",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 0 {
		t.Errorf("confirmed = %d, want 0 (non-delivered should not be confirmed)", n)
	}
}

func TestReconciliationService_AutoConfirm_NoHookIsSafe(t *testing.T) {
	// If onOrderCompleted is nil (e.g. service built outside Engine), the
	// auto-confirm path must skip the hook call without panicking.
	db := testDB(t)
	setupTestData(t, db)
	svc := newReconService(t, db)
	if svc.onOrderCompleted != nil {
		t.Fatal("onOrderCompleted should default to nil on a bare service")
	}

	order := &store.Order{
		EdgeUUID:     "no-hook-uuid",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := svc.AutoConfirmStuckDeliveredOrders(30 * time.Minute)
	if err != nil {
		t.Fatalf("AutoConfirm: %v", err)
	}
	if n != 1 {
		t.Errorf("confirmed = %d, want 1 even without hook", n)
	}
}

// ── Loop — smoke test ───────────────────────────────────────────────

// TestReconciliationService_Loop_StopsOnSignal verifies Loop returns
// promptly when stopCh is closed. Guards against the goroutine leak
// that would happen if the select missed a shutdown.
func TestReconciliationService_Loop_StopsOnSignal(t *testing.T) {
	db := testDB(t)
	svc := newReconService(t, db)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		// 1-second tick — but we close stop immediately so it shouldn't fire.
		svc.Loop(stop, 1*time.Second, 0)
		close(done)
	}()
	close(stop)
	select {
	case <-done:
		// expected — returned within 2s of stop signal.
	case <-time.After(3 * time.Second):
		t.Fatal("Loop did not return within 3s of stopCh close")
	}
}
