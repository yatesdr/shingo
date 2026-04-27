//go:build docker

package dispatch

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

func createTestBinAtNode(t *testing.T, db *store.DB, payloadCode string, nodeID int64, label string) *bins.Bin {
	return testdb.CreateBinAtNode(t, db, payloadCode, nodeID, label)
}

func setupNodeGroup(t *testing.T, db *store.DB) (grp *nodes.Node, lanes []*nodes.Node, slots [][]*nodes.Node, bp *payloads.Payload) {
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

	// Create payload template
	bp = &payloads.Payload{Code: "WGA"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	// Create NGRP node
	grp = &nodes.Node{Name: "GRP-1", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP node: %v", err)
	}
	grp, _ = db.GetNode(grp.ID)

	// Create 2 lanes
	lanes = make([]*nodes.Node, 2)
	slots = make([][]*nodes.Node, 2)
	for i := 0; i < 2; i++ {
		lane := &nodes.Node{
			Name: fmt.Sprintf("GRP-1-L%d", i+1), IsSynthetic: true,
			NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true,
		}
		if err := db.CreateNode(lane); err != nil {
			t.Fatalf("create lane %d: %v", i, err)
		}
		lane, _ = db.GetNode(lane.ID)
		lanes[i] = lane

		// 3 slots per lane
		slots[i] = make([]*nodes.Node, 3)
		for d := 1; d <= 3; d++ {
			depth := d
			slot := &nodes.Node{
				Name:     fmt.Sprintf("GRP-1-L%d-S%d", i+1, d),
				ParentID: &lane.ID, Enabled: true, Depth: &depth,
			}
			if err := db.CreateNode(slot); err != nil {
				t.Fatalf("create slot L%d-S%d: %v", i+1, d, err)
			}
			slots[i][d-1] = slot
		}
	}
	return
}

func TestGroupResolveRetrieve_AccessibleFIFO(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Place bin at lane 0, slot depth 1 (front/accessible) — older
	older := createTestBinAtNode(t, db, bp.Code, slots[0][0].ID, "BIN-FIFO-OLD")

	// Small delay to ensure different timestamps
	time.Sleep(10 * time.Millisecond)

	// Place bin at lane 1, slot depth 1 (front/accessible) — newer
	createTestBinAtNode(t, db, bp.Code, slots[1][0].ID, "BIN-FIFO-NEW")

	result, err := gr.ResolveRetrieve(grp, bp.Code)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	if result.Bin == nil {
		t.Fatal("expected bin in result")
	}
	if result.Bin.ID != older.ID {
		t.Errorf("bin ID = %d, want %d (FIFO should pick older)", result.Bin.ID, older.ID)
	}
}

func TestGroupResolveRetrieve_BuriedFails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Create a different payload template for the blocker
	blockerBP := &payloads.Payload{Code: "BLK"}
	if err := db.CreatePayload(blockerBP); err != nil {
		t.Fatalf("create blocker payload: %v", err)
	}

	// Place blocker at lane 0, slot depth 1 (front — blocks access)
	createTestBinAtNode(t, db, blockerBP.Code, slots[0][0].ID, "BIN-BLK")

	// Place target at lane 0, slot depth 3 (back — buried)
	buried := createTestBinAtNode(t, db, bp.Code, slots[0][2].ID, "BIN-BURIED")

	_, err := gr.ResolveRetrieve(grp, bp.Code)
	if err == nil {
		t.Fatal("expected error for buried bin, got nil")
	}

	var buriedErr *BuriedError
	if !errors.As(err, &buriedErr) {
		t.Fatalf("expected *BuriedError, got %T: %v", err, err)
	}
	if buriedErr.Bin.ID != buried.ID {
		t.Errorf("buried bin ID = %d, want %d", buriedErr.Bin.ID, buried.ID)
	}
}

func TestGroupResolveStore_BackToFront(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	result, err := gr.ResolveStore(grp, bp.Code, nil)
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
	t.Parallel()
	db := testDB(t)
	grp, lanes, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Place a bin at lane 0, slot depth 3 (deepest)
	createTestBinAtNode(t, db, bp.Code, slots[0][2].ID, "BIN-CONSOL")

	result, err := gr.ResolveStore(grp, bp.Code, nil)
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
	t.Parallel()
	db := testDB(t)
	grp, lanes, slots, bp := setupNodeGroup(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Fill all 3 slots of lane 0
	for i := 0; i < 3; i++ {
		createTestBinAtNode(t, db, bp.Code, slots[0][i].ID, fmt.Sprintf("BIN-FULL-%d", i))
	}

	result, err := gr.ResolveStore(grp, "", nil)
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
	t.Parallel()
	db := testDB(t)
	grp, lanes, slots, bp := setupNodeGroup(t, db)

	laneLock := NewLaneLock()
	gr := &GroupResolver{DB: db, LaneLock: laneLock}

	// Place bin at lane 0, slot depth 1
	createTestBinAtNode(t, db, bp.Code, slots[0][0].ID, "BIN-LOCKED")

	// Lock lane 0
	laneLock.TryLock(lanes[0].ID, 999)

	// Should fail since lane 0 is locked and lane 1 has no bins
	_, err := gr.ResolveRetrieve(grp, bp.Code)
	if err == nil {
		t.Fatal("expected error when lane is locked and no other bins available, got nil")
	}

	// Verify it's not a BuriedError — it should be a "no bin" error
	var buriedErr *BuriedError
	if errors.As(err, &buriedErr) {
		t.Error("should not be a BuriedError; lane 0 should have been skipped entirely")
	}
}

func TestNodeGroupResolveRetrieve_DirectChildren(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	bp := &payloads.Payload{Code: "PDC"}
	db.CreatePayload(bp)

	// Create group with direct physical children (no lanes)
	grp := &nodes.Node{Name: "GRP-DC", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	child1 := &nodes.Node{Name: "DC-01", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child1)
	child2 := &nodes.Node{Name: "DC-02", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child2)

	// Place bin at child2
	b := createTestBinAtNode(t, db, bp.Code, child2.ID, "BIN-DC")

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveRetrieve(grp, bp.Code)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	if result.Bin.ID != b.ID {
		t.Errorf("bin ID = %d, want %d", result.Bin.ID, b.ID)
	}
	if result.Node.ID != child2.ID {
		t.Errorf("node ID = %d, want %d", result.Node.ID, child2.ID)
	}
}

func TestNodeGroupResolveRetrieve_Mixed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	// Add a direct physical child to the group
	directChild := &nodes.Node{Name: "GRP-1-DC1", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(directChild)

	// Place older bin at direct child
	older := createTestBinAtNode(t, db, bp.Code, directChild.ID, "BIN-MIX-OLD")

	time.Sleep(10 * time.Millisecond)

	// Place newer bin at lane 0, slot 0
	createTestBinAtNode(t, db, bp.Code, slots[0][0].ID, "BIN-MIX-NEW")

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveRetrieve(grp, bp.Code)
	if err != nil {
		t.Fatalf("ResolveRetrieve: %v", err)
	}
	// Should pick the older bin from the direct child
	if result.Bin.ID != older.ID {
		t.Errorf("bin ID = %d, want %d (FIFO should pick older from direct child)", result.Bin.ID, older.ID)
	}
}

func TestNodeGroupResolveStore_DirectChildren(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	bp := &payloads.Payload{Code: "PDS"}
	db.CreatePayload(bp)

	grp := &nodes.Node{Name: "GRP-DS", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	child1 := &nodes.Node{Name: "DS-01", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child1)
	child2 := &nodes.Node{Name: "DS-02", ParentID: &grp.ID, Enabled: true}
	db.CreateNode(child2)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}
	result, err := gr.ResolveStore(grp, bp.Code, nil)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}
	// Should pick one of the direct children
	if result.Node.ID != child1.ID && result.Node.ID != child2.ID {
		t.Errorf("expected direct child, got node %s (ID %d)", result.Node.Name, result.Node.ID)
	}
}

func TestGroupResolveStore_BinTypeRestriction(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup(t, db)

	// Create two bin types
	btSmall := &bins.BinType{Code: "SMALL"}
	if err := db.CreateBinType(btSmall); err != nil {
		t.Fatalf("create bin type SMALL: %v", err)
	}
	btLarge := &bins.BinType{Code: "LARGE"}
	if err := db.CreateBinType(btLarge); err != nil {
		t.Fatalf("create bin type LARGE: %v", err)
	}

	// Restrict lane 0 to SMALL only
	lanes, _ := db.ListChildNodes(grp.ID)
	var lane0 *nodes.Node
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
	result, err := gr.ResolveStore(grp, bp.Code, &btLarge.ID)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}

	// Verify the slot is NOT in lane 0
	if result.Node.ParentID != nil && *result.Node.ParentID == lane0.ID {
		t.Errorf("expected slot NOT in lane 0 (restricted to SMALL), got node %s in lane 0", result.Node.Name)
	}

	// Try to store a SMALL bin — should use lane 0
	result, err = gr.ResolveStore(grp, bp.Code, &btSmall.ID)
	if err != nil {
		t.Fatalf("ResolveStore: %v", err)
	}

	// The result can be in any lane since SMALL is allowed in lane 0
	_ = result
	_ = slots
}

// setBinLoadedAt sets the loaded_at timestamp on a bin via direct SQL.
func setBinLoadedAt(t *testing.T, db *store.DB, binID int64, loadedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`UPDATE bins SET loaded_at=$1 WHERE id=$2`, loadedAt, binID)
	if err != nil {
		t.Fatalf("set loaded_at for bin %d: %v", binID, err)
	}
}

// setupNodeGroup3Lane creates an NGRP with 3 lanes of 3 slots each.
func setupNodeGroup3Lane(t *testing.T, db *store.DB) (grp *nodes.Node, lanes []*nodes.Node, slots [][]*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE node type: %v", err)
	}

	bp = &payloads.Payload{Code: "WGA"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	grp = &nodes.Node{Name: "GRP-3L", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP node: %v", err)
	}
	grp, _ = db.GetNode(grp.ID)

	lanes = make([]*nodes.Node, 3)
	slots = make([][]*nodes.Node, 3)
	for i := 0; i < 3; i++ {
		lane := &nodes.Node{
			Name: fmt.Sprintf("GRP-3L-L%d", i+1), IsSynthetic: true,
			NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true,
		}
		if err := db.CreateNode(lane); err != nil {
			t.Fatalf("create lane %d: %v", i, err)
		}
		lane, _ = db.GetNode(lane.ID)
		lanes[i] = lane

		slots[i] = make([]*nodes.Node, 3)
		for d := 1; d <= 3; d++ {
			depth := d
			slot := &nodes.Node{
				Name:     fmt.Sprintf("GRP-3L-L%d-S%d", i+1, d),
				ParentID: &lane.ID, Enabled: true, Depth: &depth,
			}
			if err := db.CreateNode(slot); err != nil {
				t.Fatalf("create slot L%d-S%d: %v", i+1, d, err)
			}
			slots[i][d-1] = slot
		}
	}
	return
}

// TC-40a: FIFO mode — buried bin older than accessible triggers reshuffle.
//
// Layout:
//   Lane 1: depth 1 = BIN-NEW  (WGA, T+2s, accessible)
//   Lane 2: depth 1 = BIN-MID  (WGA, T+1s, accessible)
//   Lane 3: depth 1 = BLK-1    (BLK, blocker)
//           depth 2 = BLK-2    (BLK, blocker)
//           depth 3 = BIN-OLD  (WGA, T,     buried ← oldest)
//
// Strict FIFO must return BuriedError for BIN-OLD, not the accessible BIN-MID.
func TestTC40a_FIFOBuriedOlderThanAccessible(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lanes, slots, bp := setupNodeGroup3Lane(t, db)

	_ = lanes // used implicitly via slots

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	// Create blocker payload
	blkPayload := &payloads.Payload{Code: "BLK"}
	if err := db.CreatePayload(blkPayload); err != nil {
		t.Fatalf("create blocker payload: %v", err)
	}

	baseTime := time.Now().Add(-1 * time.Hour)

	// BIN-OLD: buried at lane 3, depth 3 — oldest (T)
	binOld := createTestBinAtNode(t, db, bp.Code, slots[2][2].ID, "BIN-OLD")
	setBinLoadedAt(t, db, binOld.ID, baseTime)

	// BLK-1: blocker at lane 3, depth 1
	createTestBinAtNode(t, db, blkPayload.Code, slots[2][0].ID, "BLK-1")

	// BLK-2: blocker at lane 3, depth 2
	createTestBinAtNode(t, db, blkPayload.Code, slots[2][1].ID, "BLK-2")

	// BIN-MID: accessible at lane 2, depth 1 — middle age (T+1s)
	binMid := createTestBinAtNode(t, db, bp.Code, slots[1][0].ID, "BIN-MID")
	setBinLoadedAt(t, db, binMid.ID, baseTime.Add(1*time.Second))

	// BIN-NEW: accessible at lane 1, depth 1 — newest (T+2s)
	binNew := createTestBinAtNode(t, db, bp.Code, slots[0][0].ID, "BIN-NEW")
	setBinLoadedAt(t, db, binNew.ID, baseTime.Add(2*time.Second))

	_, err := gr.ResolveRetrieve(grp, bp.Code)
	if err == nil {
		t.Fatal("expected BuriedError for oldest buried bin, got nil")
	}

	var buriedErr *BuriedError
	if !errors.As(err, &buriedErr) {
		t.Fatalf("expected *BuriedError, got %T: %v", err, err)
	}
	if buriedErr.Bin.ID != binOld.ID {
		t.Errorf("buried bin ID = %d, want %d (BIN-OLD is the globally oldest)", buriedErr.Bin.ID, binOld.ID)
	}
}

// TC-40a regression guard: when buried bin is newer than accessible, return accessible (no reshuffle).
func TestTC40a_FIFOAccessibleOlderThanBuried(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup3Lane(t, db)

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	blkPayload := &payloads.Payload{Code: "BLK"}
	if err := db.CreatePayload(blkPayload); err != nil {
		t.Fatalf("create blocker payload: %v", err)
	}

	baseTime := time.Now().Add(-1 * time.Hour)

	// BIN-OLD-ACC: accessible at lane 1, depth 1 — oldest (T)
	binOldAcc := createTestBinAtNode(t, db, bp.Code, slots[0][0].ID, "BIN-OLD-ACC")
	setBinLoadedAt(t, db, binOldAcc.ID, baseTime)

	// BIN-NEW-BUR: buried at lane 3, depth 3 — newer (T+5s)
	binNewBur := createTestBinAtNode(t, db, bp.Code, slots[2][2].ID, "BIN-NEW-BUR")
	setBinLoadedAt(t, db, binNewBur.ID, baseTime.Add(5*time.Second))

	// Blocker in lane 3
	createTestBinAtNode(t, db, blkPayload.Code, slots[2][0].ID, "BLK-1")

	result, err := gr.ResolveRetrieve(grp, bp.Code)
	if err != nil {
		t.Fatalf("expected accessible result, got error: %v", err)
	}
	if result.Bin.ID != binOldAcc.ID {
		t.Errorf("bin ID = %d, want %d (accessible bin is older, no reshuffle needed)", result.Bin.ID, binOldAcc.ID)
	}
}

// TC-40b: COST mode — oldest accessible returned, older buried bin ignored.
//
// Same layout as TC-40a but with retrieve_algorithm=COST.
// Should return BIN-MID (oldest accessible), NOT trigger BuriedError for BIN-OLD.
func TestTC40b_COSTIgnoresBuriedWhenAccessible(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup3Lane(t, db)

	// Set group to COST mode
	if err := db.SetNodeProperty(grp.ID, "retrieve_algorithm", "COST"); err != nil {
		t.Fatalf("set retrieve_algorithm: %v", err)
	}

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	blkPayload := &payloads.Payload{Code: "BLK"}
	if err := db.CreatePayload(blkPayload); err != nil {
		t.Fatalf("create blocker payload: %v", err)
	}

	baseTime := time.Now().Add(-1 * time.Hour)

	// BIN-OLD: buried at lane 3, depth 3 — oldest (T)
	binOld := createTestBinAtNode(t, db, bp.Code, slots[2][2].ID, "BIN-OLD")
	setBinLoadedAt(t, db, binOld.ID, baseTime)

	// Blockers in lane 3
	createTestBinAtNode(t, db, blkPayload.Code, slots[2][0].ID, "BLK-1")
	createTestBinAtNode(t, db, blkPayload.Code, slots[2][1].ID, "BLK-2")

	// BIN-MID: accessible at lane 2, depth 1 — (T+1s)
	binMid := createTestBinAtNode(t, db, bp.Code, slots[1][0].ID, "BIN-MID")
	setBinLoadedAt(t, db, binMid.ID, baseTime.Add(1*time.Second))

	// BIN-NEW: accessible at lane 1, depth 1 — (T+2s)
	binNew := createTestBinAtNode(t, db, bp.Code, slots[0][0].ID, "BIN-NEW")
	setBinLoadedAt(t, db, binNew.ID, baseTime.Add(2*time.Second))

	result, err := gr.ResolveRetrieve(grp, bp.Code)
	if err != nil {
		t.Fatalf("COST mode should return accessible bin, got error: %v", err)
	}
	if result.Bin.ID != binMid.ID {
		t.Errorf("bin ID = %d, want %d (COST returns oldest accessible = BIN-MID)", result.Bin.ID, binMid.ID)
	}
	_ = binOld
	_ = binNew
}

// TC-40b edge: COST mode falls back to buried when no accessible bins exist.
func TestTC40b_COSTFallsToBuriedWhenNoAccessible(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, bp := setupNodeGroup3Lane(t, db)

	if err := db.SetNodeProperty(grp.ID, "retrieve_algorithm", "COST"); err != nil {
		t.Fatalf("set retrieve_algorithm: %v", err)
	}

	gr := &GroupResolver{DB: db, LaneLock: NewLaneLock()}

	blkPayload := &payloads.Payload{Code: "BLK"}
	if err := db.CreatePayload(blkPayload); err != nil {
		t.Fatalf("create blocker payload: %v", err)
	}

	// Only buried bins, no accessible
	buried := createTestBinAtNode(t, db, bp.Code, slots[0][2].ID, "BIN-BURIED")
	createTestBinAtNode(t, db, blkPayload.Code, slots[0][0].ID, "BLK-FRONT")

	_, err := gr.ResolveRetrieve(grp, bp.Code)
	if err == nil {
		t.Fatal("expected BuriedError when no accessible bins exist in COST mode")
	}

	var buriedErr *BuriedError
	if !errors.As(err, &buriedErr) {
		t.Fatalf("expected *BuriedError, got %T: %v", err, err)
	}
	if buriedErr.Bin.ID != buried.ID {
		t.Errorf("buried bin ID = %d, want %d", buriedErr.Bin.ID, buried.ID)
	}
}

// TC-41: Empty cart starvation — FindEmptyCompatibleBin is lane-unaware.
//
// Proves the gap: all accessible empties are consumed, only buried empties remain.
// FindEmptyCompatibleBin still returns a buried empty (it doesn't check lane depth),
// but IsSlotAccessible shows the bin is unreachable. The retrieve_empty path has no
// BuriedError detection and no reshuffle trigger — the robot gets sent to a slot it
// can't physically access.
//
// Layout:
//   Lane 1: depth 1 = FULL-BIN (WGA, blocker)
//           depth 2 = FULL-BIN (WGA, blocker)
//           depth 3 = EMPTY-BIN (no manifest, buried)
//   Lane 2: depth 1 = FULL-BIN (WGA, blocker)
//           depth 3 = EMPTY-BIN (no manifest, buried)
//
// No accessible empties exist anywhere in the NGRP.
func TestTC41_EmptyStarvation_BuriedEmptiesUnreachable(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lanes, slots, bp := setupNodeGroup3Lane(t, db)

	_ = lanes

	// Set up bin type and payload-bin-type link for FindEmptyCompatibleBin
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		bt = &bins.BinType{Code: "DEFAULT", Description: "Default test bin type"}
		if err := db.CreateBinType(bt); err != nil {
			t.Fatalf("create default bin type: %v", err)
		}
	}
	if err := db.SetPayloadBinTypes(bp.ID, []int64{bt.ID}); err != nil {
		t.Fatalf("set payload bin types: %v", err)
	}

	// Fill lane 1 (index 0): full bins at depth 1 and 2, empty bin buried at depth 3
	createTestBinAtNode(t, db, bp.Code, slots[0][0].ID, "FULL-L1-S1")
	createTestBinAtNode(t, db, bp.Code, slots[0][1].ID, "FULL-L1-S2")

	// Empty bin at depth 3 — no manifest, just a bare bin
	emptyL1 := &bins.Bin{BinTypeID: bt.ID, Label: "EMPTY-L1-S3", NodeID: &slots[0][2].ID, Status: "available"}
	if err := db.CreateBin(emptyL1); err != nil {
		t.Fatalf("create empty bin L1: %v", err)
	}

	// Fill lane 2 (index 1): full bin at depth 1, empty bin buried at depth 3
	createTestBinAtNode(t, db, bp.Code, slots[1][0].ID, "FULL-L2-S1")

	emptyL2 := &bins.Bin{BinTypeID: bt.ID, Label: "EMPTY-L2-S3", NodeID: &slots[1][2].ID, Status: "available"}
	if err := db.CreateBin(emptyL2); err != nil {
		t.Fatalf("create empty bin L2: %v", err)
	}

	// GAP PROOF 1: FindEmptyCompatibleBin returns a buried empty (lane-unaware)
	found, err := db.FindEmptyCompatibleBin(bp.Code, "", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatibleBin returned error: %v — if all empties are buried and the query filtered by accessibility, this would be the starvation scenario", err)
	}
	t.Logf("FindEmptyCompatibleBin returned bin %d (%s) — lane-unaware, doesn't check burial", found.ID, found.Label)

	// GAP PROOF 2: The returned bin is NOT accessible (buried behind full bins)
	accessible, err := db.IsSlotAccessible(*found.NodeID)
	if err != nil {
		t.Fatalf("IsSlotAccessible: %v", err)
	}
	if accessible {
		t.Fatal("expected buried empty bin to be inaccessible, but IsSlotAccessible returned true")
	}
	t.Logf("Bin %d is at an inaccessible slot — robot would be dispatched to a location it can't reach", found.ID)

	// GAP PROOF 3: The retrieve_empty planning path (planRetrieveEmpty) does not call
	// the NGRP resolver, so there's no BuriedError detection and no reshuffle trigger.
	// This is a documentation-only assertion — the code path is:
	//   planRetrieveEmpty → FindEmptyCompatibleBin → ClaimBin → dispatch
	// No lane awareness anywhere in that chain.
	t.Log("TC-41 gap confirmed: FindEmptyCompatibleBin returns buried bins, planRetrieveEmpty has no reshuffle path")
}
