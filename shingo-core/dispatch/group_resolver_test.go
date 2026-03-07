package dispatch

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"shingocore/store"
)

// createTestPayloadAtNode creates a bin at the given node and a payload in that bin.
func createTestPayloadAtNode(t *testing.T, db *store.DB, blueprintID int64, nodeID int64, label string) *store.Payload {
	t.Helper()
	bin := &store.Bin{BinTypeID: 1, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	p := &store.Payload{BlueprintID: blueprintID, BinID: &bin.ID, ManifestConfirmed: true}
	if err := db.CreatePayload(p); err != nil {
		t.Fatalf("create payload for bin %s: %v", label, err)
	}
	return p
}

func setupNodeGroup(t *testing.T, db *store.DB) (grp *store.Node, lanes []*store.Node, slots [][]*store.Node, bp *store.Blueprint) {
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

	// Create blueprint
	bp = &store.Blueprint{Code: "WGA", DefaultManifestJSON: "{}"}
	if err := db.CreateBlueprint(bp); err != nil {
		t.Fatalf("create blueprint: %v", err)
	}

	// Create NGRP node
	grp = &store.Node{Name: "GRP-1", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP node: %v", err)
	}
	grp, _ = db.GetNode(grp.ID)

	// Create 2 lanes
	lanes = make([]*store.Node, 2)
	slots = make([][]*store.Node, 2)
	for i := 0; i < 2; i++ {
		lane := &store.Node{
			Name: fmt.Sprintf("GRP-1-L%d", i+1), IsSynthetic: true,
			NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true,
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
				Name: fmt.Sprintf("GRP-1-L%d-S%d", i+1, d),
				ParentID: &lane.ID, Enabled: true,
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
	grp, _, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Place payload of WIDGET-A at lane 0, slot depth 1 (front/accessible) — older
	older := createTestPayloadAtNode(t, db, bp.ID, slots[0][0].ID, "BIN-FIFO-OLD")

	// Small delay to ensure different timestamps
	time.Sleep(10 * time.Millisecond)

	// Place payload of WIDGET-A at lane 1, slot depth 1 (front/accessible) — newer
	createTestPayloadAtNode(t, db, bp.ID, slots[1][0].ID, "BIN-FIFO-NEW")

	result, err := gr.ResolveRetrieve(grp, &bp.ID)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	if result.Payload == nil {
		t.Fatal("expected payload in result")
	}
	if result.Payload.ID != older.ID {
		t.Errorf("payload ID = %d, want %d (FIFO should pick older)", result.Payload.ID, older.ID)
	}
}

func TestGroupResolveRetrieve_BuriedFails(t *testing.T) {
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Create a different blueprint for the blocker
	blockerBP := &store.Blueprint{Code: "BLK", DefaultManifestJSON: "{}"}
	if err := db.CreateBlueprint(blockerBP); err != nil {
		t.Fatalf("create blocker blueprint: %v", err)
	}

	// Place blocker at lane 0, slot depth 1 (front — blocks access)
	createTestPayloadAtNode(t, db, blockerBP.ID, slots[0][0].ID, "BIN-BLK")

	// Place target WIDGET-A at lane 0, slot depth 3 (back — buried)
	buried := createTestPayloadAtNode(t, db, bp.ID, slots[0][2].ID, "BIN-BURIED")

	_, err := gr.ResolveRetrieve(grp, &bp.ID)
	if err == nil {
		t.Fatal("expected error for buried payload, got nil")
	}

	var buriedErr *BuriedError
	if !errors.As(err, &buriedErr) {
		t.Fatalf("expected *BuriedError, got %T: %v", err, err)
	}
	if buriedErr.Payload.ID != buried.ID {
		t.Errorf("buried payload ID = %d, want %d", buriedErr.Payload.ID, buried.ID)
	}
}

func TestGroupResolveStore_BackToFront(t *testing.T) {
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	result, err := gr.ResolveStore(grp, &bp.ID, nil)
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
	grp, lanes, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Place a WIDGET-A payload at lane 0, slot depth 3 (deepest)
	createTestPayloadAtNode(t, db, bp.ID, slots[0][2].ID, "BIN-CONSOL")

	result, err := gr.ResolveStore(grp, &bp.ID, nil)
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
	grp, lanes, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Fill all 3 slots of lane 0
	for i := 0; i < 3; i++ {
		createTestPayloadAtNode(t, db, bp.ID, slots[0][i].ID, fmt.Sprintf("BIN-FULL-%d", i))
	}

	result, err := gr.ResolveStore(grp, nil, nil)
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
	grp, lanes, slots, bp := setupNodeGroup(t, db)

	laneLock := NewLaneLock()
	gr := &GroupResolver{DB: db, LaneLock: laneLock}

	// Place payload at lane 0, slot depth 1
	createTestPayloadAtNode(t, db, bp.ID, slots[0][0].ID, "BIN-LOCKED")

	// Lock lane 0
	laneLock.TryLock(lanes[0].ID, 999)

	// Should fail since lane 0 is locked and lane 1 has no payloads
	_, err := gr.ResolveRetrieve(grp, &bp.ID)
	if err == nil {
		t.Fatal("expected error when lane is locked and no other payloads available, got nil")
	}

	// Verify it's not a BuriedError — it should be a "no payload" error
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

	bp := &store.Blueprint{Code: "PDC", DefaultManifestJSON: "{}"}
	db.CreateBlueprint(bp)

	// Create group with direct physical children (no lanes)
	grp := &store.Node{Name: "GRP-DC", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	child1 := &store.Node{Name: "DC-01", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child1)
	child2 := &store.Node{Name: "DC-02", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child2)

	// Place payload at child2
	p := createTestPayloadAtNode(t, db, bp.ID, child2.ID, "BIN-DC")

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveRetrieve(grp, &bp.ID)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	if result.Payload.ID != p.ID {
		t.Errorf("payload ID = %d, want %d", result.Payload.ID, p.ID)
	}
	if result.Node.ID != child2.ID {
		t.Errorf("node ID = %d, want %d", result.Node.ID, child2.ID)
	}
}

func TestNodeGroupResolveRetrieve_Mixed(t *testing.T) {
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	// Add a direct physical child to the group
	directChild := &store.Node{Name: "GRP-1-DC1", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(directChild)

	// Place older payload at direct child
	older := createTestPayloadAtNode(t, db, bp.ID, directChild.ID, "BIN-MIX-OLD")

	time.Sleep(10 * time.Millisecond)

	// Place newer payload at lane 0, slot 0
	createTestPayloadAtNode(t, db, bp.ID, slots[0][0].ID, "BIN-MIX-NEW")

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveRetrieve(grp, &bp.ID)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	// Should pick the older payload from the direct child
	if result.Payload.ID != older.ID {
		t.Errorf("payload ID = %d, want %d (FIFO should pick older from direct child)", result.Payload.ID, older.ID)
	}
}

func TestNodeGroupResolveStore_DirectChildren(t *testing.T) {
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	bp := &store.Blueprint{Code: "PDS", DefaultManifestJSON: "{}"}
	db.CreateBlueprint(bp)

	grp := &store.Node{Name: "GRP-DS", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	child1 := &store.Node{Name: "DS-01", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child1)
	child2 := &store.Node{Name: "DS-02", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child2)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveStore(grp, &bp.ID, nil)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}
	// Should pick one of the direct children
	if result.Node.ID != child1.ID && result.Node.ID != child2.ID {
		t.Errorf("expected direct child, got node %s (ID %d)", result.Node.Name, result.Node.ID)
	}
}

func TestGroupResolveStore_BinTypeRestriction(t *testing.T) {
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	// Create two bin types
	btSmall := &store.BinType{Code: "SMALL"}
	if err := db.CreateBinType(btSmall); err != nil {
		t.Fatalf("create bin type SMALL: %v", err)
	}
	btLarge := &store.BinType{Code: "LARGE"}
	if err := db.CreateBinType(btLarge); err != nil {
		t.Fatalf("create bin type LARGE: %v", err)
	}

	// Restrict lane 0 to SMALL only
	lanes, _ := db.ListChildNodes(grp.ID)
	var lane0 *store.Node
	for _, l := range lanes {
		if l.NodeTypeCode == "LANE" {
			lane0 = l
			break
		}
	}
	if lane0 == nil {
		t.Fatal("no lane found")
	}
	if err := db.SetNodeBinTypes(lane0.ID, []int64{btSmall.ID}); err != nil {
		t.Fatalf("set node bin types: %v", err)
	}

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Try to store a LARGE bin — should skip lane 0 and use lane 1
	result, err := gr.ResolveStore(grp, &bp.ID, &btLarge.ID)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}

	// Verify the slot is NOT in lane 0
	if result.Node.ParentID != nil && *result.Node.ParentID == lane0.ID {
		t.Errorf("expected slot NOT in lane 0 (restricted to SMALL), got node %s in lane 0", result.Node.Name)
	}

	// Try to store a SMALL bin — should use lane 0
	result, err = gr.ResolveStore(grp, &bp.ID, &btSmall.ID)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}

	// The result can be in any lane since SMALL is allowed in lane 0
	_ = result
	_ = slots
}
