package store

import (
	"os"
	"path/filepath"
	"testing"

	"warpath/config"
)

// testDB creates a temporary SQLite database for testing.
func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Open(&config.DatabaseConfig{
		Driver: "sqlite",
		SQLite: config.SQLiteConfig{Path: dbPath},
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		os.Remove(dbPath)
	})
	return db
}

// --- Node tests ---

func TestNodeCRUD(t *testing.T) {
	db := testDB(t)

	n := &Node{Name: "STORAGE-A1", RDSLocation: "Loc-01", NodeType: "storage", Zone: "A", Capacity: 10, Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	got, err := db.GetNode(n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "STORAGE-A1" {
		t.Errorf("Name = %q, want %q", got.Name, "STORAGE-A1")
	}
	if got.RDSLocation != "Loc-01" {
		t.Errorf("RDSLocation = %q, want %q", got.RDSLocation, "Loc-01")
	}
	if got.Capacity != 10 {
		t.Errorf("Capacity = %d, want 10", got.Capacity)
	}
	if !got.Enabled {
		t.Error("Enabled should be true")
	}

	// Update
	got.Capacity = 20
	got.Zone = "B"
	if err := db.UpdateNode(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetNode(n.ID)
	if got2.Capacity != 20 {
		t.Errorf("Capacity after update = %d, want 20", got2.Capacity)
	}
	if got2.Zone != "B" {
		t.Errorf("Zone after update = %q, want %q", got2.Zone, "B")
	}

	// GetByName
	got3, err := db.GetNodeByName("STORAGE-A1")
	if err != nil {
		t.Fatalf("getByName: %v", err)
	}
	if got3.ID != n.ID {
		t.Errorf("getByName ID = %d, want %d", got3.ID, n.ID)
	}

	// List
	db.CreateNode(&Node{Name: "LINE1-IN", RDSLocation: "Loc-02", NodeType: "line_side", Enabled: true})
	nodes, err := db.ListNodes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("len = %d, want 2", len(nodes))
	}

	// ListByType
	storageNodes, _ := db.ListNodesByType("storage")
	if len(storageNodes) != 1 {
		t.Errorf("storage count = %d, want 1", len(storageNodes))
	}

	// Delete
	if err := db.DeleteNode(n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = db.GetNode(n.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

// --- Material tests ---

func TestMaterialCRUD(t *testing.T) {
	db := testDB(t)

	m := &Material{Code: "PART-A", Description: "Steel bracket", Unit: "ea"}
	if err := db.CreateMaterial(m); err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	got, err := db.GetMaterial(m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Code != "PART-A" {
		t.Errorf("Code = %q, want %q", got.Code, "PART-A")
	}
	if got.Description != "Steel bracket" {
		t.Errorf("Description = %q, want %q", got.Description, "Steel bracket")
	}

	// GetByCode
	got2, err := db.GetMaterialByCode("PART-A")
	if err != nil {
		t.Fatalf("getByCode: %v", err)
	}
	if got2.ID != m.ID {
		t.Errorf("getByCode ID = %d, want %d", got2.ID, m.ID)
	}

	// Update
	got.Description = "Updated"
	if err := db.UpdateMaterial(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got3, _ := db.GetMaterial(m.ID)
	if got3.Description != "Updated" {
		t.Errorf("Description after update = %q, want %q", got3.Description, "Updated")
	}

	// List
	db.CreateMaterial(&Material{Code: "PART-B", Unit: "kg"})
	materials, _ := db.ListMaterials()
	if len(materials) != 2 {
		t.Errorf("len = %d, want 2", len(materials))
	}

	// Delete
	db.DeleteMaterial(m.ID)
	_, err = db.GetMaterial(m.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

// --- Inventory tests ---

func TestInventoryCRUD(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Capacity: 10, Enabled: true}
	db.CreateNode(node)
	mat := &Material{Code: "PART-A", Unit: "ea"}
	db.CreateMaterial(mat)

	// Add inventory
	invID, err := db.AddInventory(node.ID, mat.ID, 5.0, false, nil, "test item")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if invID == 0 {
		t.Fatal("invID should be assigned")
	}

	// Get
	item, err := db.GetInventoryItem(invID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if item.Quantity != 5.0 {
		t.Errorf("Quantity = %f, want 5.0", item.Quantity)
	}
	if item.MaterialCode != "PART-A" {
		t.Errorf("MaterialCode = %q, want %q", item.MaterialCode, "PART-A")
	}
	if item.Notes != "test item" {
		t.Errorf("Notes = %q, want %q", item.Notes, "test item")
	}

	// List
	items, _ := db.ListNodeInventory(node.ID)
	if len(items) != 1 {
		t.Errorf("list len = %d, want 1", len(items))
	}

	// Update quantity
	db.UpdateInventoryQuantity(invID, 3.0)
	item2, _ := db.GetInventoryItem(invID)
	if item2.Quantity != 3.0 {
		t.Errorf("Quantity after update = %f, want 3.0", item2.Quantity)
	}

	// Count
	count, _ := db.CountNodeInventory(node.ID)
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	// Remove
	db.RemoveInventory(invID)
	_, err = db.GetInventoryItem(invID)
	if err == nil {
		t.Error("expected error after remove")
	}
}

func TestInventoryClaimUnclaim(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Capacity: 10, Enabled: true}
	db.CreateNode(node)
	mat := &Material{Code: "PART-A", Unit: "ea"}
	db.CreateMaterial(mat)

	// Create an order to claim against
	order := &Order{WardropUUID: "uuid-1", OrderType: "retrieve", Status: "pending", MaterialCode: "PART-A"}
	db.CreateOrder(order)

	invID, _ := db.AddInventory(node.ID, mat.ID, 1.0, false, nil, "")

	// Claim
	if err := db.ClaimInventory(invID, order.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	item, _ := db.GetInventoryItem(invID)
	if item.ClaimedBy == nil || *item.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d", item.ClaimedBy, order.ID)
	}

	// Unclaim
	if err := db.UnclaimInventory(invID); err != nil {
		t.Fatalf("unclaim: %v", err)
	}
	item2, _ := db.GetInventoryItem(invID)
	if item2.ClaimedBy != nil {
		t.Errorf("ClaimedBy after unclaim = %v, want nil", item2.ClaimedBy)
	}
}

func TestFindSourceFIFO(t *testing.T) {
	db := testDB(t)

	// Create storage nodes
	s1 := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Capacity: 10, Enabled: true}
	s2 := &Node{Name: "S2", RDSLocation: "Loc-02", NodeType: "storage", Capacity: 10, Enabled: true}
	db.CreateNode(s1)
	db.CreateNode(s2)

	mat := &Material{Code: "PART-A", Unit: "ea"}
	db.CreateMaterial(mat)

	// Add older full pallet at S1
	id1, _ := db.AddInventory(s1.ID, mat.ID, 10.0, false, nil, "")
	// Add newer partial pallet at S2
	db.AddInventory(s2.ID, mat.ID, 3.0, true, nil, "")

	// FIFO with partial priority: should pick the partial at S2 first
	source, err := db.FindSourceFIFO("PART-A")
	if err != nil {
		t.Fatalf("FindSourceFIFO: %v", err)
	}
	if source.NodeID != s2.ID {
		t.Errorf("source node = %d, want %d (partial priority)", source.NodeID, s2.ID)
	}
	if !source.IsPartial {
		t.Error("source should be partial")
	}

	// Claim the partial, now should find the full at S1
	order := &Order{WardropUUID: "uuid-1", OrderType: "retrieve", Status: "pending", MaterialCode: "PART-A"}
	db.CreateOrder(order)
	db.ClaimInventory(source.ID, order.ID)

	source2, err := db.FindSourceFIFO("PART-A")
	if err != nil {
		t.Fatalf("FindSourceFIFO after claim: %v", err)
	}
	if source2.ID != id1 {
		t.Errorf("source = %d, want %d (full pallet at S1)", source2.ID, id1)
	}

	// Claim that too, now should get no results
	db.ClaimInventory(id1, order.ID)
	_, err = db.FindSourceFIFO("PART-A")
	if err == nil {
		t.Error("expected error when no unclaimed inventory")
	}
}

func TestFindSourceFIFO_DisabledNode(t *testing.T) {
	db := testDB(t)

	s1 := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Capacity: 10, Enabled: false}
	db.CreateNode(s1)
	mat := &Material{Code: "PART-A", Unit: "ea"}
	db.CreateMaterial(mat)
	db.AddInventory(s1.ID, mat.ID, 5.0, false, nil, "")

	_, err := db.FindSourceFIFO("PART-A")
	if err == nil {
		t.Error("expected error when only node is disabled")
	}
}

func TestFindStorageDestination(t *testing.T) {
	db := testDB(t)

	mat := &Material{Code: "PART-A", Unit: "ea"}
	db.CreateMaterial(mat)

	// Two storage nodes, S1 has the material already
	s1 := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Capacity: 5, Enabled: true}
	s2 := &Node{Name: "S2", RDSLocation: "Loc-02", NodeType: "storage", Capacity: 5, Enabled: true}
	db.CreateNode(s1)
	db.CreateNode(s2)

	// Add one item of PART-A at S1 (for consolidation preference)
	db.AddInventory(s1.ID, mat.ID, 1.0, false, nil, "")

	// Should prefer S1 (consolidation)
	dest, err := db.FindStorageDestination(mat.ID)
	if err != nil {
		t.Fatalf("FindStorageDestination: %v", err)
	}
	if dest.ID != s1.ID {
		t.Errorf("dest = %d, want %d (consolidation)", dest.ID, s1.ID)
	}
}

func TestFindStorageDestination_FallbackEmptiest(t *testing.T) {
	db := testDB(t)

	matA := &Material{Code: "PART-A", Unit: "ea"}
	matB := &Material{Code: "PART-B", Unit: "ea"}
	db.CreateMaterial(matA)
	db.CreateMaterial(matB)

	// S1 has 3 items (different material), S2 has 1 item
	s1 := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Capacity: 5, Enabled: true}
	s2 := &Node{Name: "S2", RDSLocation: "Loc-02", NodeType: "storage", Capacity: 5, Enabled: true}
	db.CreateNode(s1)
	db.CreateNode(s2)

	db.AddInventory(s1.ID, matB.ID, 1.0, false, nil, "")
	db.AddInventory(s1.ID, matB.ID, 1.0, false, nil, "")
	db.AddInventory(s1.ID, matB.ID, 1.0, false, nil, "")
	db.AddInventory(s2.ID, matB.ID, 1.0, false, nil, "")

	// No consolidation target for PART-A, fallback to emptiest (S2 with 1 vs S1 with 3)
	dest, err := db.FindStorageDestination(matA.ID)
	if err != nil {
		t.Fatalf("FindStorageDestination fallback: %v", err)
	}
	if dest.ID != s2.ID {
		t.Errorf("dest = %d, want %d (emptiest)", dest.ID, s2.ID)
	}
}

// --- Order tests ---

func TestOrderCRUD(t *testing.T) {
	db := testDB(t)

	matID := int64(0)
	o := &Order{
		WardropUUID:  "uuid-1",
		ClientID:     "line-1",
		FactoryID:    "plant-alpha",
		OrderType:    "retrieve",
		Status:       "pending",
		MaterialCode: "PART-A",
		Quantity:     1.0,
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if o.ID == 0 {
		t.Fatal("ID should be assigned")
	}
	_ = matID // not used in this test

	// Get
	got, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WardropUUID != "uuid-1" {
		t.Errorf("WardropUUID = %q, want %q", got.WardropUUID, "uuid-1")
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

	// UpdateRDS
	db.UpdateOrderRDS(o.ID, "rds-123", "RUNNING", "AMB-01")
	got4, _ := db.GetOrder(o.ID)
	if got4.RDSOrderID != "rds-123" {
		t.Errorf("RDSOrderID = %q, want %q", got4.RDSOrderID, "rds-123")
	}
	if got4.RobotID != "AMB-01" {
		t.Errorf("RobotID = %q, want %q", got4.RobotID, "AMB-01")
	}

	// GetByRDSID
	got5, err := db.GetOrderByRDSID("rds-123")
	if err != nil {
		t.Fatalf("getByRDSID: %v", err)
	}
	if got5.ID != o.ID {
		t.Errorf("getByRDSID ID = %d, want %d", got5.ID, o.ID)
	}

	// Complete
	db.CompleteOrder(o.ID)
	got6, _ := db.GetOrder(o.ID)
	if got6.Status != "completed" {
		t.Errorf("Status after complete = %q, want %q", got6.Status, "completed")
	}
	if got6.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestListOrders(t *testing.T) {
	db := testDB(t)

	db.CreateOrder(&Order{WardropUUID: "u1", Status: "pending"})
	db.CreateOrder(&Order{WardropUUID: "u2", Status: "completed"})
	db.CreateOrder(&Order{WardropUUID: "u3", Status: "pending"})

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

func TestListDispatchedRDSOrderIDs(t *testing.T) {
	db := testDB(t)

	o1 := &Order{WardropUUID: "u1", Status: "dispatched"}
	o2 := &Order{WardropUUID: "u2", Status: "in_transit"}
	o3 := &Order{WardropUUID: "u3", Status: "completed"}
	db.CreateOrder(o1)
	db.CreateOrder(o2)
	db.CreateOrder(o3)
	db.UpdateOrderRDS(o1.ID, "rds-1", "CREATED", "")
	db.UpdateOrderRDS(o2.ID, "rds-2", "RUNNING", "")
	db.UpdateOrderRDS(o3.ID, "rds-3", "FINISHED", "")

	ids, err := db.ListDispatchedRDSOrderIDs()
	if err != nil {
		t.Fatalf("list dispatched: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("len = %d, want 2", len(ids))
	}
}

// --- Outbox tests ---

func TestOutboxCRUD(t *testing.T) {
	db := testDB(t)

	// Enqueue
	if err := db.EnqueueOutbox("topic/line-1", []byte(`{"test":true}`), "ack", "line-1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	db.EnqueueOutbox("topic/line-2", []byte(`{"test":2}`), "update", "line-2")

	// List pending
	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Topic != "topic/line-1" {
		t.Errorf("topic = %q, want %q", msgs[0].Topic, "topic/line-1")
	}
	if msgs[0].MsgType != "ack" {
		t.Errorf("msg_type = %q, want %q", msgs[0].MsgType, "ack")
	}

	// Ack
	db.AckOutbox(msgs[0].ID)
	msgs2, _ := db.ListPendingOutbox(10)
	if len(msgs2) != 1 {
		t.Errorf("pending after ack = %d, want 1", len(msgs2))
	}

	// Increment retries
	db.IncrementOutboxRetries(msgs2[0].ID)
	msgs3, _ := db.ListPendingOutbox(10)
	if msgs3[0].Retries != 1 {
		t.Errorf("retries = %d, want 1", msgs3[0].Retries)
	}
}

// --- Audit tests ---

func TestAuditLog(t *testing.T) {
	db := testDB(t)

	db.AppendAudit("order", 1, "created", "", "new order", "system")
	db.AppendAudit("order", 1, "dispatched", "pending", "dispatched", "system")
	db.AppendAudit("node", 2, "updated", "", "S1", "admin")

	// List all
	entries, err := db.ListAuditLog(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("len = %d, want 3", len(entries))
	}
	// Most recent first
	if entries[0].Action != "updated" {
		t.Errorf("first entry action = %q, want %q", entries[0].Action, "updated")
	}

	// List by entity
	orderEntries, _ := db.ListEntityAudit("order", 1)
	if len(orderEntries) != 2 {
		t.Errorf("order entries = %d, want 2", len(orderEntries))
	}
}

// --- Correction tests ---

func TestCorrectionCRUD(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Enabled: true}
	db.CreateNode(node)

	c := &Correction{
		CorrectionType: "add",
		NodeID:         node.ID,
		Quantity:       5.0,
		Reason:         "physical count mismatch",
		Actor:          "admin",
	}
	if err := db.CreateCorrection(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	corrections, err := db.ListCorrections(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(corrections) != 1 {
		t.Fatalf("len = %d, want 1", len(corrections))
	}
	if corrections[0].CorrectionType != "add" {
		t.Errorf("type = %q, want %q", corrections[0].CorrectionType, "add")
	}
	if corrections[0].Reason != "physical count mismatch" {
		t.Errorf("reason = %q, want %q", corrections[0].Reason, "physical count mismatch")
	}
}

// --- Move inventory ---

func TestMoveInventory(t *testing.T) {
	db := testDB(t)

	s1 := &Node{Name: "S1", RDSLocation: "Loc-01", NodeType: "storage", Capacity: 10, Enabled: true}
	s2 := &Node{Name: "S2", RDSLocation: "Loc-02", NodeType: "storage", Capacity: 10, Enabled: true}
	db.CreateNode(s1)
	db.CreateNode(s2)
	mat := &Material{Code: "PART-A", Unit: "ea"}
	db.CreateMaterial(mat)

	invID, _ := db.AddInventory(s1.ID, mat.ID, 5.0, false, nil, "")

	// Move from S1 to S2
	if err := db.MoveInventory(invID, s2.ID); err != nil {
		t.Fatalf("move: %v", err)
	}

	item, _ := db.GetInventoryItem(invID)
	if item.NodeID != s2.ID {
		t.Errorf("NodeID = %d, want %d", item.NodeID, s2.ID)
	}

	// S1 should be empty
	items1, _ := db.ListNodeInventory(s1.ID)
	if len(items1) != 0 {
		t.Errorf("S1 inventory = %d, want 0", len(items1))
	}

	// S2 should have the item
	items2, _ := db.ListNodeInventory(s2.ID)
	if len(items2) != 1 {
		t.Errorf("S2 inventory = %d, want 1", len(items2))
	}
}

// --- Dialect tests ---

func TestRebind(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"SELECT * FROM t WHERE a=? AND b=?", "SELECT * FROM t WHERE a=$1 AND b=$2"},
		{"INSERT INTO t (a) VALUES (?)", "INSERT INTO t (a) VALUES ($1)"},
		{"SELECT 1", "SELECT 1"},
	}
	for _, tt := range tests {
		got := Rebind(tt.input)
		if got != tt.want {
			t.Errorf("Rebind(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
