package dispatch

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"shingocore/store"
)

func setupNodeGroup(t *testing.T, db *store.DB) (grp *store.Node, lanes []*store.Node, slots [][]*store.Node, style *store.PayloadStyle) {
	t.Helper()
	// Get node type IDs
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE node type: %v", err)
	}

	// Create payload style
	style = &store.PayloadStyle{Name: "WIDGET-A", Code: "WGA", FormFactor: "tote", DefaultManifestJSON: "{}"}
	if err := db.CreatePayloadStyle(style); err != nil {
		t.Fatalf("create payload style: %v", err)
	}

	// Create NGRP node
	grp = &store.Node{Name: "GRP-1", NodeType: "storage", IsSynthetic: true, NodeTypeID: &grpType.ID, Capacity: 0, Enabled: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP node: %v", err)
	}
	grp, _ = db.GetNode(grp.ID)

	// Create 2 lanes
	lanes = make([]*store.Node, 2)
	slots = make([][]*store.Node, 2)
	for i := 0; i < 2; i++ {
		lane := &store.Node{
			Name: fmt.Sprintf("GRP-1-L%d", i+1), NodeType: "storage", IsSynthetic: true,
			NodeTypeID: &lanType.ID, ParentID: &grp.ID, Capacity: 0, Enabled: true,
		}
		if err := db.CreateNode(lane); err != nil {
			t.Fatalf("create lane %d: %v", i, err)
		}
		lane, _ = db.GetNode(lane.ID)
		lanes[i] = lane

		// 3 slots per lane
		slots[i] = make([]*store.Node, 3)
		for d := 1; d <= 3; d++ {
			slot := &store.Node{
				Name: fmt.Sprintf("GRP-1-L%d-S%d", i+1, d), NodeType: "storage",
				ParentID: &lane.ID, Capacity: 1, Enabled: true,
			}
			if err := db.CreateNode(slot); err != nil {
				t.Fatalf("create slot L%d-S%d: %v", i+1, d, err)
			}
			if err := db.SetNodeProperty(slot.ID, "depth", fmt.Sprintf("%d", d)); err != nil {
				t.Fatalf("set depth L%d-S%d: %v", i+1, d, err)
			}
			slots[i][d-1] = slot
		}
	}
	return
}

func TestGroupResolveRetrieve_AccessibleFIFO(t *testing.T) {
	db := testDB(t)
	grp, _, slots, style := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Place instance of WIDGET-A at lane 0, slot depth 1 (front/accessible) — older
	older := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0][0].ID, Status: "available"}
	if err := db.CreateInstance(older); err != nil {
		t.Fatalf("create older instance: %v", err)
	}

	// Small delay to ensure different timestamps
	time.Sleep(10 * time.Millisecond)

	// Place instance of WIDGET-A at lane 1, slot depth 1 (front/accessible) — newer
	newer := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[1][0].ID, Status: "available"}
	if err := db.CreateInstance(newer); err != nil {
		t.Fatalf("create newer instance: %v", err)
	}

	result, err := gr.ResolveRetrieve(grp, &style.ID)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	if result.Instance == nil {
		t.Fatal("expected instance in result")
	}
	if result.Instance.ID != older.ID {
		t.Errorf("instance ID = %d, want %d (FIFO should pick older)", result.Instance.ID, older.ID)
	}
}

func TestGroupResolveRetrieve_BuriedFails(t *testing.T) {
	db := testDB(t)
	grp, _, slots, style := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Create a different style for the blocker
	blocker := &store.PayloadStyle{Name: "BLOCKER-B", Code: "BLK", FormFactor: "tote", DefaultManifestJSON: "{}"}
	if err := db.CreatePayloadStyle(blocker); err != nil {
		t.Fatalf("create blocker style: %v", err)
	}

	// Place blocker at lane 0, slot depth 1 (front — blocks access)
	blockerInst := &store.PayloadInstance{StyleID: blocker.ID, NodeID: &slots[0][0].ID, Status: "available"}
	if err := db.CreateInstance(blockerInst); err != nil {
		t.Fatalf("create blocker instance: %v", err)
	}

	// Place target WIDGET-A at lane 0, slot depth 3 (back — buried)
	buried := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0][2].ID, Status: "available"}
	if err := db.CreateInstance(buried); err != nil {
		t.Fatalf("create buried instance: %v", err)
	}

	_, err := gr.ResolveRetrieve(grp, &style.ID)
	if err == nil {
		t.Fatal("expected error for buried instance, got nil")
	}

	var buriedErr *BuriedError
	if !errors.As(err, &buriedErr) {
		t.Fatalf("expected *BuriedError, got %T: %v", err, err)
	}
	if buriedErr.Instance.ID != buried.ID {
		t.Errorf("buried instance ID = %d, want %d", buriedErr.Instance.ID, buried.ID)
	}
}

func TestGroupResolveStore_BackToFront(t *testing.T) {
	db := testDB(t)
	grp, _, slots, style := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	result, err := gr.ResolveStore(grp, &style.ID)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}

	// Should return the deepest slot (depth 3) of a lane
	isDeepest := result.Node.ID == slots[0][2].ID || result.Node.ID == slots[1][2].ID
	if !isDeepest {
		t.Errorf("expected deepest slot (depth 3), got node %s (ID %d)", result.Node.Name, result.Node.ID)
	}
}

func TestGroupResolveStore_Consolidation(t *testing.T) {
	db := testDB(t)
	grp, lanes, slots, style := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Place a WIDGET-A instance at lane 0, slot depth 3 (deepest)
	existing := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0][2].ID, Status: "available"}
	if err := db.CreateInstance(existing); err != nil {
		t.Fatalf("create existing instance: %v", err)
	}

	result, err := gr.ResolveStore(grp, &style.ID)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}

	// Should pick a slot in lane 0 (consolidation preference)
	if result.Node.ParentID == nil || *result.Node.ParentID != lanes[0].ID {
		t.Errorf("expected slot in lane 0 (ID %d) for consolidation, got parent_id=%v node=%s",
			lanes[0].ID, result.Node.ParentID, result.Node.Name)
	}
}

func TestGroupResolveStore_FullLane(t *testing.T) {
	db := testDB(t)
	grp, lanes, slots, style := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Fill all 3 slots of lane 0
	for i := 0; i < 3; i++ {
		inst := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0][i].ID, Status: "available"}
		if err := db.CreateInstance(inst); err != nil {
			t.Fatalf("create instance slot %d: %v", i, err)
		}
	}

	result, err := gr.ResolveStore(grp, nil)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}

	// Should pick a slot in lane 1 since lane 0 is full
	if result.Node.ParentID == nil || *result.Node.ParentID != lanes[1].ID {
		t.Errorf("expected slot in lane 1 (ID %d), got parent_id=%v node=%s",
			lanes[1].ID, result.Node.ParentID, result.Node.Name)
	}
}

func TestGroupResolveRetrieve_LockedLaneSkipped(t *testing.T) {
	db := testDB(t)
	grp, lanes, slots, style := setupNodeGroup(t, db)

	laneLock := NewLaneLock()
	gr := &GroupResolver{DB: db, LaneLock: laneLock}

	// Place instance at lane 0, slot depth 1
	inst := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0][0].ID, Status: "available"}
	if err := db.CreateInstance(inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	// Lock lane 0
	laneLock.TryLock(lanes[0].ID, 999)

	// Should fail since lane 0 is locked and lane 1 has no instances
	_, err := gr.ResolveRetrieve(grp, &style.ID)
	if err == nil {
		t.Fatal("expected error when lane is locked and no other instances available, got nil")
	}

	// Verify it's not a BuriedError — it should be a "no instance" error
	var buriedErr *BuriedError
	if errors.As(err, &buriedErr) {
		t.Error("should not be a BuriedError; lane 0 should have been skipped entirely")
	}
}

func TestNodeGroupResolveRetrieve_DirectChildren(t *testing.T) {
	db := testDB(t)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	style := &store.PayloadStyle{Name: "PART-DC", Code: "PDC", FormFactor: "tote", DefaultManifestJSON: "{}"}
	db.CreatePayloadStyle(style)

	// Create group with direct physical children (no lanes)
	grp := &store.Node{Name: "GRP-DC", NodeType: "storage", IsSynthetic: true, NodeTypeID: &grpType.ID, Capacity: 0, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	child1 := &store.Node{Name: "DC-01", NodeType: "storage", ParentID: &grp.ID, Capacity: 1, Enabled: true}
	db.CreateNode(child1)
	child2 := &store.Node{Name: "DC-02", NodeType: "storage", ParentID: &grp.ID, Capacity: 1, Enabled: true}
	db.CreateNode(child2)

	// Place instance at child2
	inst := &store.PayloadInstance{StyleID: style.ID, NodeID: &child2.ID, Status: "available"}
	db.CreateInstance(inst)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveRetrieve(grp, &style.ID)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	if result.Instance.ID != inst.ID {
		t.Errorf("instance ID = %d, want %d", result.Instance.ID, inst.ID)
	}
	if result.Node.ID != child2.ID {
		t.Errorf("node ID = %d, want %d", result.Node.ID, child2.ID)
	}
}

func TestNodeGroupResolveRetrieve_Mixed(t *testing.T) {
	db := testDB(t)
	grp, _, slots, style := setupNodeGroup(t, db)

	// Add a direct physical child to the group
	directChild := &store.Node{Name: "GRP-1-DC1", NodeType: "storage", ParentID: &grp.ID, Capacity: 1, Enabled: true}
	db.CreateNode(directChild)

	// Place older instance at direct child
	older := &store.PayloadInstance{StyleID: style.ID, NodeID: &directChild.ID, Status: "available"}
	db.CreateInstance(older)

	time.Sleep(10 * time.Millisecond)

	// Place newer instance at lane 0, slot 0
	newer := &store.PayloadInstance{StyleID: style.ID, NodeID: &slots[0][0].ID, Status: "available"}
	db.CreateInstance(newer)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveRetrieve(grp, &style.ID)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	// Should pick the older instance from the direct child
	if result.Instance.ID != older.ID {
		t.Errorf("instance ID = %d, want %d (FIFO should pick older from direct child)", result.Instance.ID, older.ID)
	}
}

func TestNodeGroupResolveStore_DirectChildren(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	style := &store.PayloadStyle{Name: "PART-DS", Code: "PDS", FormFactor: "tote", DefaultManifestJSON: "{}"}
	db.CreatePayloadStyle(style)

	grp := &store.Node{Name: "GRP-DS", NodeType: "storage", IsSynthetic: true, NodeTypeID: &grpType.ID, Capacity: 0, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	child1 := &store.Node{Name: "DS-01", NodeType: "storage", ParentID: &grp.ID, Capacity: 2, Enabled: true}
	db.CreateNode(child1)
	child2 := &store.Node{Name: "DS-02", NodeType: "storage", ParentID: &grp.ID, Capacity: 2, Enabled: true}
	db.CreateNode(child2)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveStore(grp, &style.ID)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}
	// Should pick one of the direct children
	if result.Node.ID != child1.ID && result.Node.ID != child2.ID {
		t.Errorf("expected direct child, got node %s (ID %d)", result.Node.Name, result.Node.ID)
	}
}
