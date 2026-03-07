package store

import "testing"

func TestNodeCRUD(t *testing.T) {
	db := testDB(t)

	n := &Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
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
	if !got.Enabled {
		t.Error("Enabled should be true")
	}

	// Update
	got.Zone = "B"
	if err := db.UpdateNode(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := db.GetNode(n.ID)
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
	db.CreateNode(&Node{Name: "LINE1-IN", Enabled: true})
	nodes, err := db.ListNodes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("len = %d, want 2", len(nodes))
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
	grpType := &NodeType{Code: "NGRP", Name: "Node Group", IsSynthetic: true}
	db.CreateNodeType(grpType)

	lanType := &NodeType{Code: "LANE", Name: "Lane", IsSynthetic: true}
	db.CreateNodeType(lanType)

	// Create NGRP node
	grpNode := &Node{
		Name:        "GRP-01",
		IsSynthetic: true,
		Enabled:     true,
		NodeTypeID:  &grpType.ID,
	}
	db.CreateNode(grpNode)

	// Create LANE node as child of NGRP
	lanNode := &Node{
		Name:        "LAN-01",
		IsSynthetic: true,
		Enabled:     true,
		NodeTypeID:  &lanType.ID,
		ParentID:    &grpNode.ID,
	}
	db.CreateNode(lanNode)

	// Create 3 slot nodes as children of LANE
	slot1 := &Node{Name: "SLOT-01", Enabled: true, ParentID: &lanNode.ID}
	db.CreateNode(slot1)
	db.SetNodeProperty(slot1.ID, "depth", "1")

	slot2 := &Node{Name: "SLOT-02", Enabled: true, ParentID: &lanNode.ID}
	db.CreateNode(slot2)
	db.SetNodeProperty(slot2.ID, "depth", "2")

	slot3 := &Node{Name: "SLOT-03", Enabled: true, ParentID: &lanNode.ID}
	db.CreateNode(slot3)
	db.SetNodeProperty(slot3.ID, "depth", "3")

	// Create bin type, blueprint, bins, and payloads
	bt := &BinType{Code: "TOTE", Description: "Tote"}
	db.CreateBinType(bt)

	bp := &Blueprint{Code: "LANE-TOTE", UOPCapacity: 50}
	db.CreateBlueprint(bp)

	binFront := &Bin{BinTypeID: bt.ID, Label: "LT-001", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(binFront)
	binBack := &Bin{BinTypeID: bt.ID, Label: "LT-003", NodeID: &slot3.ID, Status: "available"}
	db.CreateBin(binBack)

	pFront := &Payload{BlueprintID: bp.ID, BinID: &binFront.ID, ManifestConfirmed: true, UOPRemaining: 50}
	db.CreatePayload(pFront)
	pBack := &Payload{BlueprintID: bp.ID, BinID: &binBack.ID, ManifestConfirmed: true, UOPRemaining: 50}
	db.CreatePayload(pBack)

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

	// FindSourcePayloadInLane: should return the payload at depth 1 (front)
	srcPayload, err := db.FindSourcePayloadInLane(lanNode.ID, "LANE-TOTE")
	if err != nil {
		t.Fatalf("FindSourcePayloadInLane: %v", err)
	}
	if srcPayload.ID != pFront.ID {
		t.Errorf("source payload ID = %d, want %d (front)", srcPayload.ID, pFront.ID)
	}

	// FindStoreSlotInLane: should return slot at depth 2 (deepest empty)
	storeSlot, err := db.FindStoreSlotInLane(lanNode.ID, bp.ID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane: %v", err)
	}
	if storeSlot.ID != slot2.ID {
		t.Errorf("store slot ID = %d, want %d (depth 2)", storeSlot.ID, slot2.ID)
	}

	// CountBinsInLane: should be 2
	laneCount, err := db.CountBinsInLane(lanNode.ID)
	if err != nil {
		t.Fatalf("CountBinsInLane: %v", err)
	}
	if laneCount != 2 {
		t.Errorf("lane count = %d, want 2", laneCount)
	}

	// FindBuriedPayload: should return the payload at depth 3 (blocked by depth 1)
	buriedPayload, buriedSlot, err := db.FindBuriedPayload(lanNode.ID, "LANE-TOTE")
	if err != nil {
		t.Fatalf("FindBuriedPayload: %v", err)
	}
	if buriedPayload.ID != pBack.ID {
		t.Errorf("buried payload ID = %d, want %d", buriedPayload.ID, pBack.ID)
	}
	if buriedSlot.ID != slot3.ID {
		t.Errorf("buried slot ID = %d, want %d", buriedSlot.ID, slot3.ID)
	}
}
