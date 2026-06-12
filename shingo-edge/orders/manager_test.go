package orders

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

type testEmitter struct{}

func (testEmitter) EmitOrderCreated(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
}
func (testEmitter) EmitOrderStatusChanged(orderID int64, orderUUID string, orderType protocol.OrderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64) {
}
func (testEmitter) EmitOrderCompleted(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
}
func (testEmitter) EmitOrderDelivered(orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID, binID *int64, binUOP *int, binEpoch int64) {
}
func (testEmitter) EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string) {
}

func testManagerDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "orders.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestConfirmDeliveryDoesNotTransitionWhenReceiptEnqueueFails(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusDelivered)), "set delivered status")

	// Force enqueue failure by closing the DB before confirmation.
	testutil.MustNoErr(t, db.Close(), "close db")

	if err := mgr.ConfirmDelivery(orderID, 12); err == nil {
		t.Fatalf("expected confirm delivery to fail when receipt enqueue fails")
	}
}

func TestAbortOrderDoesNotTransitionWhenCancelEnqueueFails(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-abort", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusSubmitted)), "set submitted status")

	testutil.MustNoErr(t, db.Close(), "close db")

	if err := mgr.AbortOrder(orderID); err == nil {
		t.Fatalf("expected abort to fail when cancel enqueue fails")
	}
}

// TestRollbackReleaseRejection_InTransitRollsBack pins scoped-B for the ALN_003
// divergence: a Core release rejection (invalid_state) on an in_transit leg
// rolls it back to staged so the operator can retry, instead of the mirror
// dying terminally.
func TestRollbackReleaseRejection_InTransitRollsBack(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	id, err := db.CreateOrder("uuid-rr-intransit", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(StatusInTransit)), "set in_transit")

	if err := mgr.RollbackReleaseRejection("uuid-rr-intransit", "release error, retry"); err != nil {
		t.Fatalf("RollbackReleaseRejection: %v", err)
	}

	got, err := db.GetOrderByUUID("uuid-rr-intransit")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.Status != StatusStaged {
		t.Errorf("Status = %q, want staged (in_transit rejection must roll back for retry)", got.Status)
	}
}

// TestRollbackReleaseRejection_TerminalIgnored pins the other half of scoped-B:
// a stray release rejection reaching an already-finished leg must NOT resurrect
// or re-fail it — the status is left untouched.
func TestRollbackReleaseRejection_TerminalIgnored(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	id, err := db.CreateOrder("uuid-rr-terminal", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(StatusDelivered)), "set delivered (terminal)")

	if err := mgr.RollbackReleaseRejection("uuid-rr-terminal", "release error, retry"); err != nil {
		t.Fatalf("RollbackReleaseRejection: %v", err)
	}

	got, err := db.GetOrderByUUID("uuid-rr-terminal")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.Status != StatusDelivered {
		t.Errorf("Status = %q, want delivered (terminal leg must not be resurrected by a stray release rejection)", got.Status)
	}
}

func TestRedirectOrderDoesNotPersistWhenRedirectEnqueueFails(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-redirect", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusSubmitted)), "set submitted status")

	testutil.MustNoErr(t, db.Close(), "close db")

	if _, err := mgr.RedirectOrder(orderID, "LINE-2"); err == nil {
		t.Fatalf("expected redirect to fail when redirect enqueue fails")
	}
}

// --- Regression: Bug 5+6 — Terminal→terminal transition returns nil, not error ---
// When an order is already in a terminal state (confirmed, cancelled, failed)
// and a duplicate transition to the same or another terminal state arrives,
// Transition should return nil (idempotent) instead of an error.
func TestRegression_TerminalTransitionIdempotent(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	// Create and move order to confirmed (terminal)
	orderID, err := db.CreateOrder("uuid-term-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusDelivered)), "set delivered")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusConfirmed)), "set confirmed")

	// confirmed → confirmed should be nil (idempotent), not an error
	if err := mgr.lifecycle.Transition(orderID, StatusConfirmed, "duplicate confirm"); err != nil {
		t.Errorf("confirmed→confirmed should be nil, got: %v", err)
	}

	// confirmed → cancelled should be nil (terminal→terminal), not an error
	if err := mgr.lifecycle.Transition(orderID, StatusCancelled, "late cancel"); err != nil {
		t.Errorf("confirmed→cancelled should be nil, got: %v", err)
	}

	// confirmed → failed should be nil (terminal→terminal), not an error
	if err := mgr.lifecycle.Transition(orderID, StatusFailed, "late fail"); err != nil {
		t.Errorf("confirmed→failed should be nil, got: %v", err)
	}
}

// Plant 2026-05-12 (ALN_002): Core marked the complex order Skipped via
// SkipOrderAtomic on the no_source_bin path, but Edge's local view had
// already advanced to Acknowledged by the time the OrderSkipped envelope
// arrived. Pre-fix, HandleSkipped called TransitionOrder which validated
// the transition against the protocol state machine — and Acknowledged →
// Skipped is intentionally disallowed for client-initiated transitions
// (don't let a stale client drop in-flight work). So Edge stayed at
// Acknowledged while Core showed Skipped; the HMI faithfully rendered
// Edge's stuck status. The fix routes HandleSkipped through
// ForceTransition: Core's planner is authoritative for "the work was
// never needed" and the local status machine must yield.
func TestHandleSkipped_OverridesAcknowledged(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-skip-ack", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Walk to Acknowledged the way real flow would: pending → submitted →
	// acknowledged. UpdateOrderStatus bypasses the state machine; that's
	// fine here — we just need the row at acknowledged when HandleSkipped fires.
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusAcknowledged)), "set acknowledged")

	testutil.MustNoErr(t, mgr.HandleSkipped("uuid-skip-ack", "no_source_bin", "every pickup empty"), "HandleSkipped")

	got, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.Status != StatusSkipped {
		t.Errorf("status = %q, want %q (Core authority must override the state-machine rejection of acknowledged→skipped)", got.Status, StatusSkipped)
	}
}

// Verify cancelled→cancelled is also idempotent (Bug 6 — cancel spam)
func TestRegression_CancelledToCancelledIdempotent(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-cancel-2", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusCancelled)), "set cancelled")

	if err := mgr.lifecycle.Transition(orderID, StatusCancelled, "duplicate cancel"); err != nil {
		t.Errorf("cancelled→cancelled should be nil, got: %v", err)
	}
}

// Verify that valid transitions still work normally (non-regression)
func TestRegression_ValidTransitionsStillWork(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-valid-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// pending → submitted is valid and should succeed
	testutil.MustNoErr(t, mgr.lifecycle.Transition(orderID, StatusSubmitted, "test submit"), "pending→submitted should succeed, got")

	// Verify status actually changed
	order, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusSubmitted {
		t.Errorf("status = %q, want %q", order.Status, StatusSubmitted)
	}

	// submitted → invalid state should still error
	if err := mgr.lifecycle.Transition(orderID, StatusDelivered, "bad transition"); err == nil {
		t.Errorf("submitted→delivered should fail, got nil")
	}
}

// Verify failed→failed is idempotent (last uncovered terminal state)
func TestRegression_FailedToFailedIdempotent(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-failed-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(StatusFailed)), "set failed")

	if err := mgr.lifecycle.Transition(orderID, StatusFailed, "duplicate fail"); err != nil {
		t.Errorf("failed→failed should be nil, got: %v", err)
	}

	// Also verify failed→confirmed is nil (terminal→terminal)
	if err := mgr.lifecycle.Transition(orderID, StatusConfirmed, "late confirm"); err != nil {
		t.Errorf("failed→confirmed should be nil (terminal→terminal), got: %v", err)
	}
}
func (testEmitter) EmitOrderFaulted(orderID int64, orderUUID, reason string) {}
