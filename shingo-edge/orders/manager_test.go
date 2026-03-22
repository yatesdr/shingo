package orders

import (
	"path/filepath"
	"testing"

	"shingoedge/store"
)

type testEmitter struct{}

func (testEmitter) EmitOrderCreated(orderID int64, orderUUID, orderType string, payloadID, opNodeID *int64) {}
func (testEmitter) EmitOrderStatusChanged(orderID int64, orderUUID, orderType, oldStatus, newStatus, eta string, payloadID, opNodeID *int64) {
}
func (testEmitter) EmitOrderCompleted(orderID int64, orderUUID, orderType string, payloadID, opNodeID *int64) {}
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

	orderID, err := db.CreateOrder("uuid-1", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false)
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

	orderID, err := db.CreateOrder("uuid-abort", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false)
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

	orderID, err := db.CreateOrder("uuid-redirect", TypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false)
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
