package store

import "testing"

func TestNodeCRUD(t *testing.T) {
	db := testDB(t)

	n := &Node{Name: "STORAGE-A1", VendorLocation: "Loc-01", NodeType: "storage", Zone: "A", Capacity: 10, Enabled: true}
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
	if got.VendorLocation != "Loc-01" {
		t.Errorf("VendorLocation = %q, want %q", got.VendorLocation, "Loc-01")
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
	db.CreateNode(&Node{Name: "LINE1-IN", VendorLocation: "Loc-02", NodeType: "line_side", Enabled: true})
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

func TestLaneQueries(t *testing.T) {
	db := testDB(t)

	// Create node types
	supType := &NodeType{Code: "SMKT", Name: "Supermarket", IsSynthetic: true}
	db.CreateNodeType(supType)

	lanType := &NodeType{Code: "LANE", Name: "Lane", IsSynthetic: true}
	db.CreateNodeType(lanType)

	stgType := &NodeType{Code: "STG", Name: "Storage", IsSynthetic: false}
	db.CreateNodeType(stgType)

	// Create SMKT node
	supNode := &Node{
		Name:           "SUP-01",
		VendorLocation: "Loc-SUP",
		NodeType:       "storage",
		Enabled:        true,
		NodeTypeID:     &supType.ID,
	}
	db.CreateNode(supNode)

	// Create LANE node as child of SMKT
	lanNode := &Node{
		Name:           "LAN-01",
		VendorLocation: "Loc-LAN",
		NodeType:       "storage",
		Enabled:        true,
		NodeTypeID:     &lanType.ID,
		ParentID:       &supNode.ID,
	}
	db.CreateNode(lanNode)

	// Create 3 slot nodes as children of LANE
	slot1 := &Node{Name: "SLOT-01", VendorLocation: "Loc-S1", NodeType: "storage", Capacity: 1, Enabled: true, ParentID: &lanNode.ID}
	db.CreateNode(slot1)
	db.SetNodeProperty(slot1.ID, "depth", "1")

	slot2 := &Node{Name: "SLOT-02", VendorLocation: "Loc-S2", NodeType: "storage", Capacity: 1, Enabled: true, ParentID: &lanNode.ID}
	db.CreateNode(slot2)
	db.SetNodeProperty(slot2.ID, "depth", "2")

	slot3 := &Node{Name: "SLOT-03", VendorLocation: "Loc-S3", NodeType: "storage", Capacity: 1, Enabled: true, ParentID: &lanNode.ID}
	db.CreateNode(slot3)
	db.SetNodeProperty(slot3.ID, "depth", "3")

	// Create a payload style
	ps := &PayloadStyle{Name: "Lane-Tote", Code: "LANE-TOTE", FormFactor: "tote", UOPCapacity: 50}
	db.CreatePayloadStyle(ps)

	// Place instances at slots at depth 1 and depth 3
	instFront := &PayloadInstance{StyleID: ps.ID, NodeID: &slot1.ID, TagID: "LT-001", Status: "available", UOPRemaining: 50}
	db.CreateInstance(instFront)

	instBack := &PayloadInstance{StyleID: ps.ID, NodeID: &slot3.ID, TagID: "LT-003", Status: "available", UOPRemaining: 50}
	db.CreateInstance(instBack)

	// ListLaneSlots: should return slots ordered by depth ascending
	slots, err := db.ListLaneSlots(lanNode.ID)
	if err != nil {
		t.Fatalf("ListLaneSlots: %v", err)
	}
	if len(slots) != 3 {
		t.Fatalf("slots len = %d, want 3", len(slots))
	}
	if slots[0].Name != "SLOT-01" {
		t.Errorf("slots[0].Name = %q, want %q", slots[0].Name, "SLOT-01")
	}
	if slots[1].Name != "SLOT-02" {
		t.Errorf("slots[1].Name = %q, want %q", slots[1].Name, "SLOT-02")
	}
	if slots[2].Name != "SLOT-03" {
		t.Errorf("slots[2].Name = %q, want %q", slots[2].Name, "SLOT-03")
	}

	// GetSlotDepth
	depth1, err := db.GetSlotDepth(slot1.ID)
	if err != nil {
		t.Fatalf("GetSlotDepth slot1: %v", err)
	}
	if depth1 != 1 {
		t.Errorf("slot1 depth = %d, want 1", depth1)
	}
	depth3, err := db.GetSlotDepth(slot3.ID)
	if err != nil {
		t.Fatalf("GetSlotDepth slot3: %v", err)
	}
	if depth3 != 3 {
		t.Errorf("slot3 depth = %d, want 3", depth3)
	}

	// IsSlotAccessible: slot at depth 1 is accessible (nothing in front)
	acc1, err := db.IsSlotAccessible(slot1.ID)
	if err != nil {
		t.Fatalf("IsSlotAccessible slot1: %v", err)
	}
	if !acc1 {
		t.Error("slot1 should be accessible")
	}

	// IsSlotAccessible: slot at depth 3 is NOT accessible (slot at depth 1 is occupied)
	acc3, err := db.IsSlotAccessible(slot3.ID)
	if err != nil {
		t.Fatalf("IsSlotAccessible slot3: %v", err)
	}
	if acc3 {
		t.Error("slot3 should NOT be accessible (blocked by slot1)")
	}

	// FindSourceInstanceInLane: should return the instance at depth 1 (front)
	srcInst, err := db.FindSourceInstanceInLane(lanNode.ID, "Lane-Tote")
	if err != nil {
		t.Fatalf("FindSourceInstanceInLane: %v", err)
	}
	if srcInst.ID != instFront.ID {
		t.Errorf("source instance ID = %d, want %d (front)", srcInst.ID, instFront.ID)
	}

	// FindStoreSlotInLane: should return slot at depth 2 (deepest empty)
	storeSlot, err := db.FindStoreSlotInLane(lanNode.ID, ps.ID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane: %v", err)
	}
	if storeSlot.ID != slot2.ID {
		t.Errorf("store slot ID = %d, want %d (depth 2)", storeSlot.ID, slot2.ID)
	}

	// CountInstancesInLane: should be 2
	laneCount, err := db.CountInstancesInLane(lanNode.ID)
	if err != nil {
		t.Fatalf("CountInstancesInLane: %v", err)
	}
	if laneCount != 2 {
		t.Errorf("lane count = %d, want 2", laneCount)
	}

	// FindBuriedInstance: should return the instance at depth 3 (blocked by depth 1)
	buriedInst, buriedSlot, err := db.FindBuriedInstance(lanNode.ID, "Lane-Tote")
	if err != nil {
		t.Fatalf("FindBuriedInstance: %v", err)
	}
	if buriedInst.ID != instBack.ID {
		t.Errorf("buried instance ID = %d, want %d", buriedInst.ID, instBack.ID)
	}
	if buriedSlot.ID != slot3.ID {
		t.Errorf("buried slot ID = %d, want %d", buriedSlot.ID, slot3.ID)
	}
}
