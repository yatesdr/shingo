//go:build docker

package store

import "testing"

// laneFixture builds a NGRP > LANE > 3 slots hierarchy and returns the lane ID
// plus the three slot IDs (front→back).
func laneFixture(t *testing.T, db *DB, prefix string) (laneID int64, slot1ID, slot2ID, slot3ID int64) {
	t.Helper()

	grpType := &NodeType{Code: prefix + "-NGRP", Name: "Node Group", IsSynthetic: true}
	db.CreateNodeType(grpType)
	laneType := &NodeType{Code: prefix + "-LANE", Name: "Lane", IsSynthetic: true}
	db.CreateNodeType(laneType)

	grp := &Node{Name: prefix + "-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &grpType.ID}
	db.CreateNode(grp)

	lane := &Node{Name: prefix + "-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &grp.ID}
	db.CreateNode(lane)

	d1, d2, d3 := 1, 2, 3
	s1 := &Node{Name: prefix + "-SLOT-01", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	db.CreateNode(s1)
	s2 := &Node{Name: prefix + "-SLOT-02", Enabled: true, ParentID: &lane.ID, Depth: &d2}
	db.CreateNode(s2)
	s3 := &Node{Name: prefix + "-SLOT-03", Enabled: true, ParentID: &lane.ID, Depth: &d3}
	db.CreateNode(s3)

	return lane.ID, s1.ID, s2.ID, s3.ID
}

func TestFindOldestBuriedBin(t *testing.T) {
	db := testDB(t)

	laneID, slot1ID, slot2ID, slot3ID := laneFixture(t, db, "OLD")

	bt := &BinType{Code: "OLD-BT", Description: "tote"}
	db.CreateBinType(bt)

	// Two buried bins at depth 2 and 3, both blocked by a shallower bin at depth 1.
	// The shallower bin doesn't need to match the payload — just needs to exist.

	// Blocker at depth 1 (different payload so it doesn't match the query).
	blocker := &Bin{BinTypeID: bt.ID, Label: "OLD-BLOCK", NodeID: &slot1ID, Status: "available"}
	db.CreateBin(blocker)
	db.SetBinManifest(blocker.ID, `{"items":[]}`, "OTHER", 10)
	db.ConfirmBinManifest(blocker.ID, "2024-01-01 00:00:00")

	// Buried bins at depth 2 (newer) and 3 (older) — FindOldestBuriedBin picks the oldest.
	buriedNewer := &Bin{BinTypeID: bt.ID, Label: "OLD-BUR-NEW", NodeID: &slot2ID, Status: "available"}
	db.CreateBin(buriedNewer)
	db.SetBinManifest(buriedNewer.ID, `{"items":[]}`, "PAY-BUR", 10)
	db.ConfirmBinManifest(buriedNewer.ID, "2024-12-01 00:00:00")

	buriedOlder := &Bin{BinTypeID: bt.ID, Label: "OLD-BUR-OLD", NodeID: &slot3ID, Status: "available"}
	db.CreateBin(buriedOlder)
	db.SetBinManifest(buriedOlder.ID, `{"items":[]}`, "PAY-BUR", 10)
	db.ConfirmBinManifest(buriedOlder.ID, "2024-02-01 00:00:00")

	gotBin, gotSlot, err := db.FindOldestBuriedBin(laneID, "PAY-BUR")
	if err != nil {
		t.Fatalf("FindOldestBuriedBin: %v", err)
	}
	if gotBin.ID != buriedOlder.ID {
		t.Errorf("oldest buried bin = %d (%s), want %d (%s)",
			gotBin.ID, gotBin.Label, buriedOlder.ID, buriedOlder.Label)
	}
	if gotSlot.ID != slot3ID {
		t.Errorf("oldest buried slot = %d, want %d", gotSlot.ID, slot3ID)
	}

	// Counts covering the lane.
	n, err := db.CountBinsInLane(laneID)
	if err != nil {
		t.Fatalf("CountBinsInLane: %v", err)
	}
	if n != 3 {
		t.Errorf("CountBinsInLane = %d, want 3", n)
	}
}

func TestListLaneSlotsEmpty(t *testing.T) {
	db := testDB(t)
	// Non-existent lane id — list should come back empty, no error.
	slots, err := db.ListLaneSlots(99999)
	if err != nil {
		t.Fatalf("ListLaneSlots: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("unknown lane slots len = %d, want 0", len(slots))
	}

	// CountBinsInLane on unknown lane = 0
	c, err := db.CountBinsInLane(99999)
	if err != nil {
		t.Fatalf("CountBinsInLane: %v", err)
	}
	if c != 0 {
		t.Errorf("unknown lane count = %d, want 0", c)
	}
}

func TestFindSourceBinInLane_NoMatch(t *testing.T) {
	db := testDB(t)
	laneID, _, _, _ := laneFixture(t, db, "NOM")

	// No bins at all — should error (no accessible bin).
	if _, err := db.FindSourceBinInLane(laneID, "PAY-NONE"); err == nil {
		t.Error("empty lane should error")
	}

	// FindStoreSlotInLane on an empty lane — deepest is slot3.
	storeSlot, err := db.FindStoreSlotInLane(laneID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane: %v", err)
	}
	if storeSlot == nil {
		t.Fatal("store slot should not be nil")
	}
	// Should be at depth 3 (deepest empty — back-to-front packing).
	if storeSlot.Depth == nil || *storeSlot.Depth != 3 {
		t.Errorf("store slot depth = %v, want 3 (deepest empty)", storeSlot.Depth)
	}
}

func TestLaneSlotAccessibility(t *testing.T) {
	db := testDB(t)
	laneID, slot1ID, slot2ID, slot3ID := laneFixture(t, db, "ACC")

	bt := &BinType{Code: "ACC-BT"}
	db.CreateBinType(bt)

	// Initially all slots are accessible (no bins blocking).
	for _, id := range []int64{slot1ID, slot2ID, slot3ID} {
		acc, err := db.IsSlotAccessible(id)
		if err != nil {
			t.Fatalf("IsSlotAccessible(%d): %v", id, err)
		}
		if !acc {
			t.Errorf("slot %d should be accessible with empty lane", id)
		}
	}

	// Depths readable.
	d1, _ := db.GetSlotDepth(slot1ID)
	d3, _ := db.GetSlotDepth(slot3ID)
	if d1 != 1 || d3 != 3 {
		t.Errorf("depths = (%d,%d), want (1,3)", d1, d3)
	}

	// Put a bin at slot1 — slot3 should now be inaccessible.
	b := &Bin{BinTypeID: bt.ID, Label: "ACC-B1", NodeID: &slot1ID, Status: "available"}
	db.CreateBin(b)

	acc3, err := db.IsSlotAccessible(slot3ID)
	if err != nil {
		t.Fatalf("IsSlotAccessible(slot3): %v", err)
	}
	if acc3 {
		t.Error("slot3 should be blocked by bin at slot1")
	}

	// slot1 itself is still accessible (nothing at shallower depth).
	acc1, _ := db.IsSlotAccessible(slot1ID)
	if !acc1 {
		t.Error("slot1 should remain accessible")
	}

	// ListLaneSlots returns 3 slots in depth-ascending order.
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("ListLaneSlots: %v", err)
	}
	if len(slots) != 3 {
		t.Fatalf("slots len = %d, want 3", len(slots))
	}
	if slots[0].ID != slot1ID || slots[2].ID != slot3ID {
		t.Errorf("slot order wrong: first=%d last=%d, want first=%d last=%d",
			slots[0].ID, slots[2].ID, slot1ID, slot3ID)
	}
}

func TestFindBuriedBin_NoBuried(t *testing.T) {
	db := testDB(t)
	laneID, slot1ID, _, _ := laneFixture(t, db, "NOB")

	bt := &BinType{Code: "NOB-BT"}
	db.CreateBinType(bt)

	// Only a front bin — nothing is buried.
	b := &Bin{BinTypeID: bt.ID, Label: "NOB-B1", NodeID: &slot1ID, Status: "available"}
	db.CreateBin(b)
	db.SetBinManifest(b.ID, `{"items":[]}`, "PAY-A", 10)
	db.ConfirmBinManifest(b.ID, "")

	if _, _, err := db.FindBuriedBin(laneID, "PAY-A"); err == nil {
		t.Error("no buried bin should produce error")
	}

	// But a source lookup should succeed and return it.
	src, err := db.FindSourceBinInLane(laneID, "PAY-A")
	if err != nil {
		t.Fatalf("FindSourceBinInLane: %v", err)
	}
	if src.ID != b.ID {
		t.Errorf("source bin = %d, want %d", src.ID, b.ID)
	}
}
