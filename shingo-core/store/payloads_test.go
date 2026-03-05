package store

import "testing"

func TestPayloadStyleCRUD(t *testing.T) {
	db := testDB(t)

	ps := &PayloadStyle{
		Name:        "Tote-A",
		Code:        "TOTE-A",
		Description: "Standard tote type A",
		FormFactor:  "tote",
		UOPCapacity: 50,
		WidthMM:     600,
		HeightMM:    400,
		DepthMM:     300,
		WeightKG:    2.5,
	}
	if err := db.CreatePayloadStyle(ps); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ps.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get
	got, err := db.GetPayloadStyle(ps.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Tote-A" {
		t.Errorf("Name = %q, want %q", got.Name, "Tote-A")
	}
	if got.Code != "TOTE-A" {
		t.Errorf("Code = %q, want %q", got.Code, "TOTE-A")
	}
	if got.Description != "Standard tote type A" {
		t.Errorf("Description = %q, want %q", got.Description, "Standard tote type A")
	}
	if got.FormFactor != "tote" {
		t.Errorf("FormFactor = %q, want %q", got.FormFactor, "tote")
	}
	if got.UOPCapacity != 50 {
		t.Errorf("UOPCapacity = %d, want 50", got.UOPCapacity)
	}
	if got.WidthMM != 600 {
		t.Errorf("WidthMM = %f, want 600", got.WidthMM)
	}
	if got.HeightMM != 400 {
		t.Errorf("HeightMM = %f, want 400", got.HeightMM)
	}
	if got.DepthMM != 300 {
		t.Errorf("DepthMM = %f, want 300", got.DepthMM)
	}
	if got.WeightKG != 2.5 {
		t.Errorf("WeightKG = %f, want 2.5", got.WeightKG)
	}

	// GetByName
	byName, err := db.GetPayloadStyleByName("Tote-A")
	if err != nil {
		t.Fatalf("getByName: %v", err)
	}
	if byName.ID != ps.ID {
		t.Errorf("getByName ID = %d, want %d", byName.ID, ps.ID)
	}

	// GetByCode
	byCode, err := db.GetPayloadStyleByCode("TOTE-A")
	if err != nil {
		t.Fatalf("getByCode: %v", err)
	}
	if byCode.ID != ps.ID {
		t.Errorf("getByCode ID = %d, want %d", byCode.ID, ps.ID)
	}

	// Update
	got.Name = "Tote-A-Updated"
	got.UOPCapacity = 75
	got.WeightKG = 3.0
	if err := db.UpdatePayloadStyle(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetPayloadStyle(ps.ID)
	if got2.Name != "Tote-A-Updated" {
		t.Errorf("Name after update = %q, want %q", got2.Name, "Tote-A-Updated")
	}
	if got2.UOPCapacity != 75 {
		t.Errorf("UOPCapacity after update = %d, want 75", got2.UOPCapacity)
	}
	if got2.WeightKG != 3.0 {
		t.Errorf("WeightKG after update = %f, want 3.0", got2.WeightKG)
	}

	// Delete
	if err := db.DeletePayloadStyle(ps.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = db.GetPayloadStyle(ps.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestPayloadInstanceCRUD(t *testing.T) {
	db := testDB(t)

	// Create prerequisites
	ps := &PayloadStyle{Name: "Bin-X", Code: "BIN-X", FormFactor: "bin", UOPCapacity: 200}
	db.CreatePayloadStyle(ps)

	node := &Node{Name: "STORAGE-B1", NodeType: "storage", Enabled: true}
	db.CreateNode(node)

	// Create instance
	inst := &PayloadInstance{
		StyleID:      ps.ID,
		NodeID:       &node.ID,
		TagID:        "TAG-001",
		Status:       "available",
		UOPRemaining: 100,
		Notes:        "test instance",
	}
	if err := db.CreateInstance(inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if inst.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// Get with joined fields
	got, err := db.GetInstance(inst.ID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if got.StyleName != "Bin-X" {
		t.Errorf("StyleName = %q, want %q", got.StyleName, "Bin-X")
	}
	if got.NodeName != "STORAGE-B1" {
		t.Errorf("NodeName = %q, want %q", got.NodeName, "STORAGE-B1")
	}
	if got.TagID != "TAG-001" {
		t.Errorf("TagID = %q, want %q", got.TagID, "TAG-001")
	}
	if got.Status != "available" {
		t.Errorf("Status = %q, want %q", got.Status, "available")
	}
	if got.UOPRemaining != 100 {
		t.Errorf("UOPRemaining = %d, want 100", got.UOPRemaining)
	}

	// Update
	got.UOPRemaining = 80
	got.Notes = "updated notes"
	if err := db.UpdateInstance(got); err != nil {
		t.Fatalf("update instance: %v", err)
	}
	got2, _ := db.GetInstance(inst.ID)
	if got2.UOPRemaining != 80 {
		t.Errorf("UOPRemaining after update = %d, want 80", got2.UOPRemaining)
	}
	if got2.Notes != "updated notes" {
		t.Errorf("Notes after update = %q, want %q", got2.Notes, "updated notes")
	}

	// Create a second instance at same node
	inst2 := &PayloadInstance{StyleID: ps.ID, NodeID: &node.ID, TagID: "TAG-002", Status: "available", UOPRemaining: 50}
	db.CreateInstance(inst2)

	// ListInstances
	all, err := db.ListInstances()
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("list len = %d, want 2", len(all))
	}

	// ListInstancesByNode
	byNode, err := db.ListInstancesByNode(node.ID)
	if err != nil {
		t.Fatalf("list by node: %v", err)
	}
	if len(byNode) != 2 {
		t.Errorf("by node len = %d, want 2", len(byNode))
	}

	// CountInstancesByNode
	count, err := db.CountInstancesByNode(node.ID)
	if err != nil {
		t.Fatalf("count by node: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	// ListInstancesByStatus
	byStatus, err := db.ListInstancesByStatus("available")
	if err != nil {
		t.Fatalf("list by status: %v", err)
	}
	if len(byStatus) != 2 {
		t.Errorf("by status len = %d, want 2", len(byStatus))
	}

	// Delete
	if err := db.DeleteInstance(inst.ID); err != nil {
		t.Fatalf("delete instance: %v", err)
	}
	remaining, _ := db.ListInstances()
	if len(remaining) != 1 {
		t.Errorf("remaining after delete = %d, want 1", len(remaining))
	}
}

func TestPayloadInstanceLifecycle(t *testing.T) {
	db := testDB(t)

	ps := &PayloadStyle{Name: "Crate-Y", Code: "CRATE-Y", FormFactor: "crate", UOPCapacity: 100}
	db.CreatePayloadStyle(ps)

	node1 := &Node{Name: "STORE-1", NodeType: "storage", Enabled: true}
	db.CreateNode(node1)
	node2 := &Node{Name: "LINE-1", NodeType: "line_side", Enabled: true}
	db.CreateNode(node2)

	inst := &PayloadInstance{StyleID: ps.ID, NodeID: &node1.ID, TagID: "TAG-100", Status: "available", UOPRemaining: 100}
	db.CreateInstance(inst)

	// Claim
	orderID := int64(42)
	if err := db.ClaimInstance(inst.ID, orderID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, _ := db.GetInstance(inst.ID)
	if got.ClaimedBy == nil || *got.ClaimedBy != orderID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, orderID)
	}

	// ListInstancesByClaimedOrder
	claimed, err := db.ListInstancesByClaimedOrder(orderID)
	if err != nil {
		t.Fatalf("list by claimed order: %v", err)
	}
	if len(claimed) != 1 {
		t.Errorf("claimed len = %d, want 1", len(claimed))
	}

	// Unclaim
	if err := db.UnclaimInstance(inst.ID); err != nil {
		t.Fatalf("unclaim: %v", err)
	}
	got2, _ := db.GetInstance(inst.ID)
	if got2.ClaimedBy != nil {
		t.Errorf("ClaimedBy after unclaim = %v, want nil", got2.ClaimedBy)
	}

	// Move
	if err := db.MoveInstance(inst.ID, node2.ID); err != nil {
		t.Fatalf("move: %v", err)
	}
	got3, _ := db.GetInstance(inst.ID)
	if got3.NodeID == nil || *got3.NodeID != node2.ID {
		t.Errorf("NodeID after move = %v, want %d", got3.NodeID, node2.ID)
	}

	// FindSourceInstanceFIFO — create two instances, verify FIFO order
	inst2 := &PayloadInstance{StyleID: ps.ID, NodeID: &node1.ID, TagID: "TAG-101", Status: "available", UOPRemaining: 50}
	db.CreateInstance(inst2)
	inst3 := &PayloadInstance{StyleID: ps.ID, NodeID: &node1.ID, TagID: "TAG-102", Status: "available", UOPRemaining: 75}
	db.CreateInstance(inst3)

	fifo, err := db.FindSourceInstanceFIFO("Crate-Y")
	if err != nil {
		t.Fatalf("FindSourceInstanceFIFO: %v", err)
	}
	// inst2 was created first at node1, should be returned (FIFO by delivered_at)
	if fifo.ID != inst2.ID {
		t.Errorf("FIFO instance ID = %d, want %d", fifo.ID, inst2.ID)
	}
}

func TestInstanceEventsCRUD(t *testing.T) {
	db := testDB(t)

	ps := &PayloadStyle{Name: "Box-Z", Code: "BOX-Z", FormFactor: "box", UOPCapacity: 30}
	db.CreatePayloadStyle(ps)

	node := &Node{Name: "STORE-EVT", NodeType: "storage", Enabled: true}
	db.CreateNode(node)

	inst := &PayloadInstance{StyleID: ps.ID, NodeID: &node.ID, TagID: "TAG-200", Status: "available", UOPRemaining: 30}
	db.CreateInstance(inst) // This should auto-log a "created" event

	// Create additional events
	db.CreateInstanceEvent(&InstanceEvent{
		InstanceID: inst.ID,
		EventType:  InstanceEventMoved,
		Detail:     "moved to node 5",
		Actor:      "system",
	})
	db.CreateInstanceEvent(&InstanceEvent{
		InstanceID: inst.ID,
		EventType:  InstanceEventClaimed,
		Detail:     "order_id=10",
		Actor:      "dispatch",
	})

	// List events (reverse chronological, limit)
	events, err := db.ListInstanceEvents(inst.ID, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	// Should have 3: created (auto), moved, claimed
	if len(events) != 3 {
		t.Fatalf("events len = %d, want 3", len(events))
	}
	// Verify all expected event types are present
	typeSet := map[string]bool{}
	for _, e := range events {
		typeSet[e.EventType] = true
	}
	for _, want := range []string{InstanceEventCreated, InstanceEventMoved, InstanceEventClaimed} {
		if !typeSet[want] {
			t.Errorf("missing event type %q in results", want)
		}
	}

	// Verify limit works
	limited, err := db.ListInstanceEvents(inst.ID, 2)
	if err != nil {
		t.Fatalf("list events limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limited events len = %d, want 2", len(limited))
	}
}

func TestPayloadStyleManifestCRUD(t *testing.T) {
	db := testDB(t)

	ps := &PayloadStyle{Name: "Kit-M", Code: "KIT-M", FormFactor: "kit", UOPCapacity: 10}
	db.CreatePayloadStyle(ps)

	// Create 2 manifest items
	item1 := &PayloadStyleManifestItem{StyleID: ps.ID, PartNumber: "PN-001", Quantity: 5, Description: "Bolt M8"}
	if err := db.CreateStyleManifestItem(item1); err != nil {
		t.Fatalf("create item1: %v", err)
	}
	if item1.ID == 0 {
		t.Fatal("item1 ID should be assigned")
	}

	item2 := &PayloadStyleManifestItem{StyleID: ps.ID, PartNumber: "PN-002", Quantity: 10, Description: "Washer M8"}
	if err := db.CreateStyleManifestItem(item2); err != nil {
		t.Fatalf("create item2: %v", err)
	}

	// List (ordered by id)
	items, err := db.ListStyleManifest(ps.ID)
	if err != nil {
		t.Fatalf("list manifest: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("manifest len = %d, want 2", len(items))
	}
	if items[0].PartNumber != "PN-001" {
		t.Errorf("first item part = %q, want %q", items[0].PartNumber, "PN-001")
	}
	if items[1].PartNumber != "PN-002" {
		t.Errorf("second item part = %q, want %q", items[1].PartNumber, "PN-002")
	}

	// Delete one item
	if err := db.DeleteStyleManifestItem(item1.ID); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	remaining, _ := db.ListStyleManifest(ps.ID)
	if len(remaining) != 1 {
		t.Errorf("remaining after delete = %d, want 1", len(remaining))
	}

	// ReplaceStyleManifest
	replacements := []*PayloadStyleManifestItem{
		{PartNumber: "PN-100", Quantity: 2, Description: "Nut M10"},
		{PartNumber: "PN-101", Quantity: 4, Description: "Screw M10"},
		{PartNumber: "PN-102", Quantity: 1, Description: "Bracket"},
	}
	if err := db.ReplaceStyleManifest(ps.ID, replacements); err != nil {
		t.Fatalf("replace manifest: %v", err)
	}
	replaced, _ := db.ListStyleManifest(ps.ID)
	if len(replaced) != 3 {
		t.Fatalf("replaced len = %d, want 3", len(replaced))
	}
	if replaced[0].PartNumber != "PN-100" {
		t.Errorf("replaced[0] part = %q, want %q", replaced[0].PartNumber, "PN-100")
	}
	if replaced[2].PartNumber != "PN-102" {
		t.Errorf("replaced[2] part = %q, want %q", replaced[2].PartNumber, "PN-102")
	}
}
