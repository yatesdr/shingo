//go:build docker

package store

import "testing"

func TestNodeGroupLifecycle(t *testing.T) {
	db := testDB(t)

	// NGRP and LANE node types are pre-seeded by migration.

	// Create the group
	groupID, err := db.CreateNodeGroup("GRP-1")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	if groupID == 0 {
		t.Fatal("CreateNodeGroup returned 0 id")
	}

	// Read back the node directly to confirm the group row was inserted.
	grpNode, err := db.GetNode(groupID)
	if err != nil {
		t.Fatalf("GetNode(group): %v", err)
	}
	if grpNode.Name != "GRP-1" {
		t.Errorf("group name = %q, want %q", grpNode.Name, "GRP-1")
	}
	if !grpNode.IsSynthetic {
		t.Error("group should be synthetic")
	}
	if grpNode.NodeTypeCode != "NGRP" {
		t.Errorf("group NodeTypeCode = %q, want %q", grpNode.NodeTypeCode, "NGRP")
	}

	// Before adding any lanes, GetGroupLayout should return an empty layout.
	emptyLayout, err := db.GetGroupLayout(groupID)
	if err != nil {
		t.Fatalf("GetGroupLayout empty: %v", err)
	}
	if len(emptyLayout.Lanes) != 0 {
		t.Errorf("empty layout lanes = %d, want 0", len(emptyLayout.Lanes))
	}

	// Add a lane to the group
	laneID, err := db.AddLane(groupID, "LANE-A")
	if err != nil {
		t.Fatalf("AddLane: %v", err)
	}
	if laneID == 0 {
		t.Fatal("AddLane returned 0 id")
	}

	// Verify the lane exists as a child of the group
	laneNode, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("GetNode(lane): %v", err)
	}
	if laneNode.ParentID == nil || *laneNode.ParentID != groupID {
		t.Errorf("lane.ParentID = %v, want %d", laneNode.ParentID, groupID)
	}
	if laneNode.NodeTypeCode != "LANE" {
		t.Errorf("lane.NodeTypeCode = %q, want %q", laneNode.NodeTypeCode, "LANE")
	}

	// GetGroupLayout should now include the lane
	layout, err := db.GetGroupLayout(groupID)
	if err != nil {
		t.Fatalf("GetGroupLayout: %v", err)
	}
	if len(layout.Lanes) != 1 {
		t.Fatalf("layout lanes = %d, want 1", len(layout.Lanes))
	}
	if layout.Lanes[0].ID != laneID {
		t.Errorf("layout lane ID = %d, want %d", layout.Lanes[0].ID, laneID)
	}
	if layout.Lanes[0].Name != "LANE-A" {
		t.Errorf("layout lane Name = %q, want %q", layout.Lanes[0].Name, "LANE-A")
	}

	// Delete the group — both the group and its lane should vanish
	if err := db.DeleteNodeGroup(groupID); err != nil {
		t.Fatalf("DeleteNodeGroup: %v", err)
	}
	if _, err := db.GetNode(groupID); err == nil {
		t.Error("group should be gone after DeleteNodeGroup")
	}
	if _, err := db.GetNode(laneID); err == nil {
		t.Error("lane should be gone after DeleteNodeGroup (synthetic child)")
	}
}
