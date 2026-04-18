//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// Characterization tests for handlers_test_orders.go — pinned before the
// Stage 1 refactor that replaces h.engine.DB() with named query methods. The
// focus here is apiDirectOrderReceipt, which routes confirmation through the
// dispatcher's Lifecycle service (not directly through the DB). The refactor
// must preserve that routing: the handler does NOT shortcut to
// db.UpdateOrderStatus / db.CompleteOrder — it goes through
// Lifecycle().ConfirmReceipt so that the completion event is emitted and
// order_history records the transition.

// --- apiDirectOrderReceipt --------------------------------------------------

// TestApiDirectOrderReceipt_HappyPath pins the lifecycle-driven completion
// contract: an order in "delivered" status flips to "confirmed" with a
// populated CompletedAt timestamp. If a refactor bypassed the lifecycle and
// only ran UpdateOrderStatus, CompletedAt would not be set.
func TestApiDirectOrderReceipt_HappyPath(t *testing.T) {
	h, db := testHandlers(t)

	// Seed an order already in the "delivered" state — the only status from
	// which Lifecycle().ConfirmReceipt proceeds.
	o := &store.Order{
		EdgeUUID:  "rcpt-happy-1",
		StationID: "line-1",
		OrderType: "move",
		Status:    "delivered",
		Quantity:  1,
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}

	rec := postJSON(t, h.apiDirectOrderReceipt, "/api/direct-order/receipt",
		map[string]any{
			"order_uuid":   o.EdgeUUID,
			"receipt_type": "full",
			"final_count":  int64(5),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status    string `json:"status"`
		OrderUUID string `json:"order_uuid"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "confirmed" || resp.OrderUUID != o.EdgeUUID {
		t.Errorf("response: got %+v, want status=confirmed order_uuid=%s", resp, o.EdgeUUID)
	}

	got := testdb.RequireOrder(t, db, o.EdgeUUID)
	if got.Status != "confirmed" {
		t.Errorf("order status: got %q, want confirmed", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("order CompletedAt should be set after ConfirmReceipt")
	}
}

// TestApiDirectOrderReceipt_DefaultsReceiptType pins the "" → "full" default:
// omitting receipt_type in the payload must not produce a 400. This is a
// contract most characterization refactors would preserve by accident, but
// it's worth pinning explicitly since the default is a visible API behavior.
func TestApiDirectOrderReceipt_DefaultsReceiptType(t *testing.T) {
	h, db := testHandlers(t)

	o := &store.Order{
		EdgeUUID:  "rcpt-default-1",
		StationID: "line-1",
		OrderType: "move",
		Status:    "delivered",
		Quantity:  1,
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}

	rec := postJSON(t, h.apiDirectOrderReceipt, "/api/direct-order/receipt",
		map[string]any{"order_uuid": o.EdgeUUID, "final_count": int64(1)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (default receipt_type); body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiDirectOrderReceipt_MissingOrderUUID pins the validation branch: no
// order_uuid → 400 without touching the DB.
func TestApiDirectOrderReceipt_MissingOrderUUID(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiDirectOrderReceipt, "/api/direct-order/receipt",
		map[string]any{"order_uuid": "", "receipt_type": "full"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiDirectOrderReceipt_OrderNotFound pins the GetOrderByUUID miss → 404.
func TestApiDirectOrderReceipt_OrderNotFound(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiDirectOrderReceipt, "/api/direct-order/receipt",
		map[string]any{"order_uuid": "does-not-exist", "receipt_type": "full"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiDirectOrderReceipt_WrongStatusReturns500 pins the
// ConfirmReceipt-rejects-non-delivered path: an order in "pending" cannot be
// confirmed, the lifecycle returns an error, and the handler surfaces 500
// with the error message in the body. Critically, the order's status and
// CompletedAt MUST remain untouched.
func TestApiDirectOrderReceipt_WrongStatusReturns500(t *testing.T) {
	h, db := testHandlers(t)

	o := &store.Order{
		EdgeUUID:  "rcpt-wrong-status-1",
		StationID: "line-1",
		OrderType: "move",
		Status:    "pending",
		Quantity:  1,
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}

	rec := postJSON(t, h.apiDirectOrderReceipt, "/api/direct-order/receipt",
		map[string]any{"order_uuid": o.EdgeUUID, "receipt_type": "full"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}

	got := testdb.RequireOrder(t, db, o.EdgeUUID)
	if got.Status != "pending" {
		t.Errorf("order status after 500: got %q, want unchanged pending", got.Status)
	}
	if got.CompletedAt != nil {
		t.Error("order CompletedAt should remain nil after lifecycle error")
	}
}

// TestApiDirectOrderReceipt_AlreadyCompleted pins the idempotency branch: if
// the order has already been completed (CompletedAt != nil) ConfirmReceipt
// returns (false, nil) and the handler surfaces 400 "order already completed"
// — NOT a 5xx. This guards against refactors that flip the ok/err check.
func TestApiDirectOrderReceipt_AlreadyCompleted(t *testing.T) {
	h, db := testHandlers(t)

	o := &store.Order{
		EdgeUUID:  "rcpt-done-1",
		StationID: "line-1",
		OrderType: "move",
		Status:    "confirmed",
		Quantity:  1,
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Mark as completed so ConfirmReceipt short-circuits to (false, nil).
	if err := db.CompleteOrder(o.ID); err != nil {
		t.Fatalf("complete order: %v", err)
	}

	rec := postJSON(t, h.apiDirectOrderReceipt, "/api/direct-order/receipt",
		map[string]any{"order_uuid": o.EdgeUUID, "receipt_type": "full"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (already completed); body=%s", rec.Code, rec.Body.String())
	}
}
