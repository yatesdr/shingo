//go:build docker

package service

import (
	"fmt"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

func newOrderSvc(db *store.DB, fail bool) (*OrderService, *testdb.MockBackend) {
	var m *testdb.MockBackend
	if fail {
		m = testdb.NewFailingBackend()
	} else {
		m = testdb.NewSuccessBackend()
	}
	return NewOrderService(db, m), m
}

func makeOrder(t *testing.T, db *store.DB, nodeName string) *store.Order {
	t.Helper()
	o := &store.Order{
		EdgeUUID:     fmt.Sprintf("svc-order-%s", t.Name()),
		StationID:    "test-station",
		OrderType:    "move",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: nodeName,
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

func TestOrderService_Create_InsertsRow(t *testing.T) {
	db := testDB(t)
	svc, _ := newOrderSvc(db, false)

	o := &store.Order{
		EdgeUUID:     "order-create-1",
		StationID:    "st-1",
		OrderType:    "move",
		Status:       "pending",
		Quantity:     2,
		DeliveryNode: "dest",
	}
	if err := svc.Create(o); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.ID == 0 {
		t.Fatal("expected ID to be populated after Create")
	}

	got, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.EdgeUUID != "order-create-1" {
		t.Errorf("EdgeUUID = %q, want %q", got.EdgeUUID, "order-create-1")
	}
	if got.Quantity != 2 {
		t.Errorf("Quantity = %d, want 2", got.Quantity)
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want pending", got.Status)
	}
}

func TestOrderService_UpdateStatus_TransitionAndHistory(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name)

	if err := svc.UpdateStatus(o.ID, "queued", "ready"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, _ := db.GetOrder(o.ID)
	if got.Status != "queued" {
		t.Errorf("Status = %q, want queued", got.Status)
	}
	// Non-terminal statuses clear error_detail per UpdateStatus semantics.
	if got.ErrorDetail != "" {
		t.Errorf("ErrorDetail = %q, want empty for non-terminal status", got.ErrorDetail)
	}

	history, err := db.ListOrderHistory(o.ID)
	if err != nil {
		t.Fatalf("ListOrderHistory: %v", err)
	}
	foundQueued := false
	for _, h := range history {
		if h.Status == "queued" {
			foundQueued = true
		}
	}
	if !foundQueued {
		t.Errorf("order history missing 'queued' entry: %+v", history)
	}
}

func TestOrderService_UpdateStatus_TerminalFailedPersistsDetail(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name)
	if err := svc.UpdateStatus(o.ID, "failed", "resolver had no matching bin"); err != nil {
		t.Fatalf("UpdateStatus(failed): %v", err)
	}
	got, _ := db.GetOrder(o.ID)
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.ErrorDetail != "resolver had no matching bin" {
		t.Errorf("ErrorDetail = %q, want detail persisted on terminal", got.ErrorDetail)
	}
}

func TestOrderService_UpdateVendor(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name)
	if err := svc.UpdateVendor(o.ID, "vendor-42", "RUNNING", "robot-7"); err != nil {
		t.Fatalf("UpdateVendor: %v", err)
	}
	got, _ := db.GetOrder(o.ID)
	if got.VendorOrderID != "vendor-42" {
		t.Errorf("VendorOrderID = %q, want %q", got.VendorOrderID, "vendor-42")
	}
	if got.VendorState != "RUNNING" {
		t.Errorf("VendorState = %q, want %q", got.VendorState, "RUNNING")
	}
	if got.RobotID != "robot-7" {
		t.Errorf("RobotID = %q, want %q", got.RobotID, "robot-7")
	}
}

func TestOrderService_SetPriority_NoVendorSkipsFleet(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	// Failing backend is fine here — it must not be called when VendorOrderID="".
	svc, backend := newOrderSvc(db, true)

	o := makeOrder(t, db, sd.LineNode.Name)

	resolved, err := svc.SetPriority(o.ID, 7)
	if err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	if resolved == nil || resolved.ID != o.ID {
		t.Errorf("resolved order = %+v, want order id %d", resolved, o.ID)
	}

	got, _ := db.GetOrder(o.ID)
	if got.Priority != 7 {
		t.Errorf("Priority = %d, want 7", got.Priority)
	}

	// Backend should have no orders — CreateTransportOrder was never called.
	if len(backend.Orders()) != 0 {
		t.Errorf("backend.Orders() = %+v, want empty", backend.Orders())
	}
}

func TestOrderService_SetPriority_WithVendorCallsFleet(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name)
	// Attach a vendor order id so SetPriority dispatches to the fleet.
	if err := db.UpdateOrderVendor(o.ID, "vendor-abc", "RUNNING", "robot-1"); err != nil {
		t.Fatalf("UpdateOrderVendor: %v", err)
	}

	resolved, err := svc.SetPriority(o.ID, 3)
	if err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolved is nil")
	}

	got, _ := db.GetOrder(o.ID)
	if got.Priority != 3 {
		t.Errorf("Priority = %d, want 3", got.Priority)
	}
}

func TestOrderService_SetPriority_FleetErrorReturnsOrderAndError(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, true) // failing backend

	o := makeOrder(t, db, sd.LineNode.Name)
	if err := db.UpdateOrderVendor(o.ID, "vendor-xyz", "RUNNING", "robot-1"); err != nil {
		t.Fatalf("UpdateOrderVendor: %v", err)
	}
	origPriority := int(0) // freshly-created orders default to 0

	resolved, err := svc.SetPriority(o.ID, 9)
	if err == nil {
		t.Fatal("expected fleet error to propagate")
	}
	// Contract: order is returned even on fleet error so the caller can log.
	if resolved == nil || resolved.ID != o.ID {
		t.Errorf("resolved = %+v, want order id %d (returned even on fleet error)", resolved, o.ID)
	}

	// DB priority must not have been updated (fleet failed first).
	got, _ := db.GetOrder(o.ID)
	if got.Priority != origPriority {
		t.Errorf("Priority = %d, want %d (unchanged after fleet failure)", got.Priority, origPriority)
	}
}

func TestOrderService_SetPriority_OrderNotFound(t *testing.T) {
	db := testDB(t)
	svc, _ := newOrderSvc(db, false)

	resolved, err := svc.SetPriority(99999, 5)
	if err == nil {
		t.Fatal("expected order-not-found error")
	}
	if resolved != nil {
		t.Errorf("resolved = %+v, want nil on lookup failure", resolved)
	}
}

func TestOrderService_ClaimBin_And_UnclaimBin(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	bin := createTestBin(t, db, sd.StorageNode.ID, "OS-CLAIM-1", "", 0)
	o := makeOrder(t, db, sd.LineNode.Name)

	if err := svc.ClaimBin(bin.ID, o.ID); err != nil {
		t.Fatalf("ClaimBin: %v", err)
	}
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy == nil || *got.ClaimedBy != o.ID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, o.ID)
	}

	if err := svc.UnclaimBin(bin.ID); err != nil {
		t.Fatalf("UnclaimBin: %v", err)
	}
	got, _ = db.GetBin(bin.ID)
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil after UnclaimBin", got.ClaimedBy)
	}
}

func TestOrderService_ClaimBin_FailsIfAlreadyClaimed(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	bin := createTestBin(t, db, sd.StorageNode.ID, "OS-CLAIM-2", "", 0)
	o1 := makeOrder(t, db, sd.LineNode.Name)

	if err := svc.ClaimBin(bin.ID, o1.ID); err != nil {
		t.Fatalf("first ClaimBin: %v", err)
	}

	// Second order tries to claim the same bin — must fail.
	o2 := &store.Order{
		EdgeUUID: "second-claim", StationID: "s", OrderType: "move", Status: "pending",
		Quantity: 1, DeliveryNode: sd.LineNode.Name,
	}
	if err := db.CreateOrder(o2); err != nil {
		t.Fatalf("create o2: %v", err)
	}

	if err := svc.ClaimBin(bin.ID, o2.ID); err == nil {
		t.Fatal("expected second ClaimBin to fail on already-claimed bin")
	}

	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy == nil || *got.ClaimedBy != o1.ID {
		t.Errorf("ClaimedBy = %v, want original claim %d", got.ClaimedBy, o1.ID)
	}
}

// --- PR 3a.3a absorbed query methods -------------------------------------

func TestOrderService_GetOrder_ReturnsCreated(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name)
	got, err := svc.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got == nil || got.ID != o.ID {
		t.Fatalf("GetOrder returned %+v, want id %d", got, o.ID)
	}
	if got.EdgeUUID != o.EdgeUUID {
		t.Errorf("EdgeUUID = %q, want %q", got.EdgeUUID, o.EdgeUUID)
	}
}

func TestOrderService_GetOrder_MissingErrors(t *testing.T) {
	db := testDB(t)
	svc, _ := newOrderSvc(db, false)
	if _, err := svc.GetOrder(99999); err == nil {
		t.Fatal("expected GetOrder on missing id to error")
	}
}

func TestOrderService_GetOrderByUUID_ReturnsCreated(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name)
	got, err := svc.GetOrderByUUID(o.EdgeUUID)
	if err != nil {
		t.Fatalf("GetOrderByUUID: %v", err)
	}
	if got == nil || got.ID != o.ID {
		t.Fatalf("GetOrderByUUID returned %+v, want id %d", got, o.ID)
	}
	if got.EdgeUUID != o.EdgeUUID {
		t.Errorf("EdgeUUID = %q, want %q", got.EdgeUUID, o.EdgeUUID)
	}
}

func TestOrderService_GetOrderByUUID_MissingErrors(t *testing.T) {
	db := testDB(t)
	svc, _ := newOrderSvc(db, false)
	if _, err := svc.GetOrderByUUID("does-not-exist-uuid"); err == nil {
		t.Fatal("expected GetOrderByUUID on missing uuid to error")
	}
}

func TestOrderService_ListActiveOrders_IncludesPending(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name) // default status "pending" = active
	active, err := svc.ListActiveOrders()
	if err != nil {
		t.Fatalf("ListActiveOrders: %v", err)
	}
	found := false
	for _, a := range active {
		if a.ID == o.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListActiveOrders did not include pending order id %d (got %d)", o.ID, len(active))
	}
}

func TestOrderService_ListOrders_FiltersByStatus(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	pending := makeOrder(t, db, sd.LineNode.Name)
	failed := makeOrder(t, db, sd.LineNode.Name)
	if err := db.UpdateOrderStatus(failed.ID, "failed", "test failure"); err != nil {
		t.Fatalf("UpdateOrderStatus: %v", err)
	}

	// status="" => all orders, both included
	all, err := svc.ListOrders("", 100)
	if err != nil {
		t.Fatalf("ListOrders all: %v", err)
	}
	foundPending, foundFailed := false, false
	for _, o := range all {
		if o.ID == pending.ID {
			foundPending = true
		}
		if o.ID == failed.ID {
			foundFailed = true
		}
	}
	if !foundPending || !foundFailed {
		t.Errorf("ListOrders(\"\") missing orders: pending=%v failed=%v", foundPending, foundFailed)
	}

	// status="failed" => only failed
	onlyFailed, err := svc.ListOrders("failed", 100)
	if err != nil {
		t.Fatalf("ListOrders failed: %v", err)
	}
	for _, o := range onlyFailed {
		if o.Status != "failed" {
			t.Errorf("ListOrders(failed) included order id %d with status %q", o.ID, o.Status)
		}
		if o.ID == pending.ID {
			t.Errorf("ListOrders(failed) leaked pending order id %d", pending.ID)
		}
	}
}

func TestOrderService_ListOrderHistory_ReturnsTransitions(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	o := makeOrder(t, db, sd.LineNode.Name)
	if err := db.UpdateOrderStatus(o.ID, "queued", "ready"); err != nil {
		t.Fatalf("UpdateOrderStatus queued: %v", err)
	}
	if err := db.UpdateOrderStatus(o.ID, "dispatched", "assigned"); err != nil {
		t.Fatalf("UpdateOrderStatus dispatched: %v", err)
	}

	history, err := svc.ListOrderHistory(o.ID)
	if err != nil {
		t.Fatalf("ListOrderHistory: %v", err)
	}
	if len(history) < 2 {
		t.Fatalf("ListOrderHistory len = %d, want >= 2 (got %+v)", len(history), history)
	}
	sawQueued, sawDispatched := false, false
	for _, h := range history {
		if h.Status == "queued" {
			sawQueued = true
		}
		if h.Status == "dispatched" {
			sawDispatched = true
		}
	}
	if !sawQueued || !sawDispatched {
		t.Errorf("history missing transitions: queued=%v dispatched=%v", sawQueued, sawDispatched)
	}
}

func TestOrderService_ListChildOrders_ReturnsSequencedChildren(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	parent := &store.Order{
		EdgeUUID:     "svc-parent-" + t.Name(),
		StationID:    "test-station",
		OrderType:    "compound",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: sd.LineNode.Name,
	}
	if err := db.CreateOrder(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	for i := 1; i <= 3; i++ {
		child := &store.Order{
			EdgeUUID:      fmt.Sprintf("svc-child-%s-%d", t.Name(), i),
			StationID:     "test-station",
			OrderType:     "move",
			Status:        "pending",
			Quantity:      1,
			DeliveryNode:  sd.LineNode.Name,
			ParentOrderID: &parent.ID,
			Sequence:      i,
		}
		if err := db.CreateOrder(child); err != nil {
			t.Fatalf("create child %d: %v", i, err)
		}
	}

	children, err := svc.ListChildOrders(parent.ID)
	if err != nil {
		t.Fatalf("ListChildOrders: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("len(children) = %d, want 3 (got %+v)", len(children), children)
	}
	// ListChildOrders orders by Sequence per store precedent.
	for i, c := range children {
		wantSeq := i + 1
		if c.Sequence != wantSeq {
			t.Errorf("children[%d].Sequence = %d, want %d", i, c.Sequence, wantSeq)
		}
		if c.ParentOrderID == nil || *c.ParentOrderID != parent.ID {
			t.Errorf("children[%d].ParentOrderID = %v, want %d", i, c.ParentOrderID, parent.ID)
		}
	}
}

// --- PR 3a.3b absorbed query methods -------------------------------------

func TestOrderService_ListOrdersByStation_FiltersByStation(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	mine := &store.Order{
		EdgeUUID:     "svc-by-station-mine",
		StationID:    "station-alpha",
		OrderType:    "move",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: sd.LineNode.Name,
	}
	if err := db.CreateOrder(mine); err != nil {
		t.Fatalf("create mine: %v", err)
	}
	other := &store.Order{
		EdgeUUID:     "svc-by-station-other",
		StationID:    "station-beta",
		OrderType:    "move",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: sd.LineNode.Name,
	}
	if err := db.CreateOrder(other); err != nil {
		t.Fatalf("create other: %v", err)
	}

	got, err := svc.ListOrdersByStation("station-alpha", 50)
	if err != nil {
		t.Fatalf("ListOrdersByStation: %v", err)
	}
	foundMine := false
	for _, o := range got {
		if o.StationID != "station-alpha" {
			t.Errorf("leaked station %q into station-alpha results (order %d)", o.StationID, o.ID)
		}
		if o.ID == mine.ID {
			foundMine = true
		}
		if o.ID == other.ID {
			t.Errorf("ListOrdersByStation leaked station-beta order id %d", other.ID)
		}
	}
	if !foundMine {
		t.Errorf("ListOrdersByStation missing station-alpha order id %d (got %d rows)", mine.ID, len(got))
	}
}

func TestOrderService_ListOrdersByStation_AppliesLimit(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc, _ := newOrderSvc(db, false)

	for i := 0; i < 5; i++ {
		o := &store.Order{
			EdgeUUID:     fmt.Sprintf("svc-limit-%d", i),
			StationID:    "station-limit",
			OrderType:    "move",
			Status:       "pending",
			Quantity:     1,
			DeliveryNode: sd.LineNode.Name,
		}
		if err := db.CreateOrder(o); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	got, err := svc.ListOrdersByStation("station-limit", 3)
	if err != nil {
		t.Fatalf("ListOrdersByStation: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (limit cap)", len(got))
	}
	// ListByStation orders by id DESC, so results must be strictly decreasing
	// and come from the tail of inserts.
	for i := 1; i < len(got); i++ {
		if got[i-1].ID <= got[i].ID {
			t.Errorf("results not in id DESC: got[%d].ID=%d <= got[%d].ID=%d", i-1, got[i-1].ID, i, got[i].ID)
		}
	}
}

func TestOrderService_ListOrdersByStation_UnknownStationEmpty(t *testing.T) {
	db := testDB(t)
	svc, _ := newOrderSvc(db, false)
	got, err := svc.ListOrdersByStation("no-such-station", 50)
	if err != nil {
		t.Fatalf("ListOrdersByStation on unknown station should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 for unknown station (got %+v)", len(got), got)
	}
}
