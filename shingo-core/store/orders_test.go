package store

import "testing"

func TestOrderCRUD(t *testing.T) {
	db := testDB(t)

	o := &Order{
		EdgeUUID:     "uuid-1",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "pending",
		Quantity:     1.0,
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if o.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get
	got, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EdgeUUID != "uuid-1" {
		t.Errorf("EdgeUUID = %q, want %q", got.EdgeUUID, "uuid-1")
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want %q", got.Status, "pending")
	}

	// GetByUUID
	got2, err := db.GetOrderByUUID("uuid-1")
	if err != nil {
		t.Fatalf("getByUUID: %v", err)
	}
	if got2.ID != o.ID {
		t.Errorf("getByUUID ID = %d, want %d", got2.ID, o.ID)
	}

	// UpdateStatus (also creates history)
	db.UpdateOrderStatus(o.ID, "dispatched", "sent to RDS")
	got3, _ := db.GetOrder(o.ID)
	if got3.Status != "dispatched" {
		t.Errorf("Status after update = %q, want %q", got3.Status, "dispatched")
	}

	// Check history
	history, _ := db.ListOrderHistory(o.ID)
	if len(history) != 1 {
		t.Fatalf("history len = %d, want 1", len(history))
	}
	if history[0].Status != "dispatched" {
		t.Errorf("history status = %q, want %q", history[0].Status, "dispatched")
	}

	// UpdateVendor
	db.UpdateOrderVendor(o.ID, "rds-123", "RUNNING", "AMB-01")
	got4, _ := db.GetOrder(o.ID)
	if got4.VendorOrderID != "rds-123" {
		t.Errorf("VendorOrderID = %q, want %q", got4.VendorOrderID, "rds-123")
	}
	if got4.RobotID != "AMB-01" {
		t.Errorf("RobotID = %q, want %q", got4.RobotID, "AMB-01")
	}

	// GetByVendorID
	got5, err := db.GetOrderByVendorID("rds-123")
	if err != nil {
		t.Fatalf("getByVendorID: %v", err)
	}
	if got5.ID != o.ID {
		t.Errorf("getByVendorID ID = %d, want %d", got5.ID, o.ID)
	}

	// Complete
	db.CompleteOrder(o.ID)
	got6, _ := db.GetOrder(o.ID)
	if got6.Status != "confirmed" {
		t.Errorf("Status after complete = %q, want %q", got6.Status, "confirmed")
	}
	if got6.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestListOrders(t *testing.T) {
	db := testDB(t)

	db.CreateOrder(&Order{EdgeUUID: "u1", Status: "pending"})
	db.CreateOrder(&Order{EdgeUUID: "u2", Status: "confirmed"})
	db.CreateOrder(&Order{EdgeUUID: "u3", Status: "pending"})

	// All
	all, _ := db.ListOrders("", 10)
	if len(all) != 3 {
		t.Errorf("all len = %d, want 3", len(all))
	}

	// Filtered
	pending, _ := db.ListOrders("pending", 10)
	if len(pending) != 2 {
		t.Errorf("pending len = %d, want 2", len(pending))
	}

	// Active
	active, _ := db.ListActiveOrders()
	if len(active) != 2 {
		t.Errorf("active len = %d, want 2", len(active))
	}
}

func TestListDispatchedVendorOrderIDs(t *testing.T) {
	db := testDB(t)

	o1 := &Order{EdgeUUID: "u1", Status: "dispatched"}
	o2 := &Order{EdgeUUID: "u2", Status: "in_transit"}
	o3 := &Order{EdgeUUID: "u3", Status: "confirmed"}
	db.CreateOrder(o1)
	db.CreateOrder(o2)
	db.CreateOrder(o3)
	db.UpdateOrderVendor(o1.ID, "rds-1", "CREATED", "")
	db.UpdateOrderVendor(o2.ID, "rds-2", "RUNNING", "")
	db.UpdateOrderVendor(o3.ID, "rds-3", "FINISHED", "")

	ids, err := db.ListDispatchedVendorOrderIDs()
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("len = %d, want 2", len(ids))
	}
}

func TestOrderCompoundFields(t *testing.T) {
	db := testDB(t)

	// Create parent order
	parent := &Order{
		EdgeUUID:  "parent-uuid",
		StationID: "line-1",
		OrderType: "compound",
		Status:    "pending",
	}
	if err := db.CreateOrder(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Create child orders with sequence
	child1 := &Order{
		EdgeUUID:      "child-uuid-1",
		StationID:     "line-1",
		OrderType:     "retrieve",
		Status:        "pending",
		ParentOrderID: &parent.ID,
		Sequence:      1,
	}
	child2 := &Order{
		EdgeUUID:      "child-uuid-2",
		StationID:     "line-1",
		OrderType:     "store",
		Status:        "pending",
		ParentOrderID: &parent.ID,
		Sequence:      2,
	}
	child3 := &Order{
		EdgeUUID:      "child-uuid-3",
		StationID:     "line-1",
		OrderType:     "move",
		Status:        "pending",
		ParentOrderID: &parent.ID,
		Sequence:      3,
	}
	db.CreateOrder(child1)
	db.CreateOrder(child2)
	db.CreateOrder(child3)

	// ListChildOrders (should be in sequence order)
	children, err := db.ListChildOrders(parent.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("children len = %d, want 3", len(children))
	}
	if children[0].Sequence != 1 {
		t.Errorf("children[0].Sequence = %d, want 1", children[0].Sequence)
	}
	if children[1].Sequence != 2 {
		t.Errorf("children[1].Sequence = %d, want 2", children[1].Sequence)
	}
	if children[2].Sequence != 3 {
		t.Errorf("children[2].Sequence = %d, want 3", children[2].Sequence)
	}

	// GetNextChildOrder (should return first pending child)
	next, err := db.GetNextChildOrder(parent.ID)
	if err != nil {
		t.Fatalf("get next child: %v", err)
	}
	if next.ID != child1.ID {
		t.Errorf("next child ID = %d, want %d", next.ID, child1.ID)
	}

	// Complete the first child, next should be child2
	db.UpdateOrderStatus(child1.ID, "confirmed", "done")
	next2, err := db.GetNextChildOrder(parent.ID)
	if err != nil {
		t.Fatalf("get next child after completing first: %v", err)
	}
	if next2.ID != child2.ID {
		t.Errorf("next child ID after complete = %d, want %d", next2.ID, child2.ID)
	}
}
