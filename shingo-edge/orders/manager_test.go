package orders

import (
	"path/filepath"
	"testing"

	"shingoedge/store"
)

type testEmitter struct{}

func (testEmitter) EmitOrderCreated(orderID int64, orderUUID, orderType string, payloadID, processNodeID *int64) {}
func (testEmitter) EmitOrderStatusChanged(orderID int64, orderUUID, orderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64) {
}
func (testEmitter) EmitOrderCompleted(orderID int64, orderUUID, orderType string, payloadID, processNodeID *int64) {}
func (testEmitter) EmitOrderFailed(orderID int64, orderUUID, orderType, reason string)              {}

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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, StatusDelivered); err != nil {
		t.Fatalf("set delivered status: %v", err)
	}

	// Force enqueue failure by closing the DB before confirmation.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if err := mgr.ConfirmDelivery(orderID, 12); err == nil {
		t.Fatalf("expected confirm delivery to fail when receipt enqueue fails")
	}
}

func TestAbortOrderDoesNotTransitionWhenCancelEnqueueFails(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-abort", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, StatusSubmitted); err != nil {
		t.Fatalf("set submitted status: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if err := mgr.AbortOrder(orderID); err == nil {
		t.Fatalf("expected abort to fail when cancel enqueue fails")
	}
}

func TestRedirectOrderDoesNotPersistWhenRedirectEnqueueFails(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-redirect", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, StatusSubmitted); err != nil {
		t.Fatalf("set submitted status: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := mgr.RedirectOrder(orderID, "LINE-2"); err == nil {
		t.Fatalf("expected redirect to fail when redirect enqueue fails")
	}
}

// --- Regression: Bug 5+6 — Terminal→terminal transition returns nil, not error ---
// When an order is already in a terminal state (confirmed, cancelled, failed)
// and a duplicate transition to the same or another terminal state arrives,
// Transition should return nil (idempotent) instead of an error.
func TestRegression_TerminalTransitionIdempotent(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	// Create and move order to confirmed (terminal)
	orderID, err := db.CreateOrder("uuid-term-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, StatusDelivered); err != nil {
		t.Fatalf("set delivered: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, StatusConfirmed); err != nil {
		t.Fatalf("set confirmed: %v", err)
	}

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

// Verify cancelled→cancelled is also idempotent (Bug 6 — cancel spam)
func TestRegression_CancelledToCancelledIdempotent(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-cancel-2", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, StatusCancelled); err != nil {
		t.Fatalf("set cancelled: %v", err)
	}

	if err := mgr.lifecycle.Transition(orderID, StatusCancelled, "duplicate cancel"); err != nil {
		t.Errorf("cancelled→cancelled should be nil, got: %v", err)
	}
}

// Verify that valid transitions still work normally (non-regression)
func TestRegression_ValidTransitionsStillWork(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-valid-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// pending → submitted is valid and should succeed
	if err := mgr.lifecycle.Transition(orderID, StatusSubmitted, "test submit"); err != nil {
		t.Fatalf("pending→submitted should succeed, got: %v", err)
	}

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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge.station")

	orderID, err := db.CreateOrder("uuid-failed-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, StatusFailed); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	if err := mgr.lifecycle.Transition(orderID, StatusFailed, "duplicate fail"); err != nil {
		t.Errorf("failed→failed should be nil, got: %v", err)
	}

	// Also verify failed→confirmed is nil (terminal→terminal)
	if err := mgr.lifecycle.Transition(orderID, StatusConfirmed, "late confirm"); err != nil {
		t.Errorf("failed→confirmed should be nil (terminal→terminal), got: %v", err)
	}
}
