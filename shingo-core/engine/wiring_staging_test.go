//go:build docker

package engine

import (
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// --- Characterization tests for resolveNodeStaging (wiring.go:569-582) ---
//
// resolveNodeStaging determines if bins arriving at a node should be "staged"
// (lineside) or "available" (storage slot under a LANE). The key distinction:
// - Storage slot (parent is LANE type) → staged=false, expiresAt=nil
// - Lineside node (anything else) → staged=true, expiresAt from config/TTL
//
// These tests call resolveNodeStaging directly on a real Engine to characterize
// the branching behavior with real DB state.

// TC-RS-1: Normal lineside node (no parent) → staged=true.
func TestResolveNodeStaging_LinesideNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// LINE1-IN from standard data has no parent — it's a lineside node.
	lineNode, _ := db.GetNode(sd.LineNode.ID)

	staged, _ := eng.resolveNodeStaging(lineNode)
	if !staged {
		t.Error("lineside node should resolve to staged=true")
	}
}

// TC-RS-2: Storage slot under a LANE parent → staged=false.
func TestResolveNodeStaging_StorageSlotUnderLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// LANE node type is seeded by migrations.
	laneType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE node type: %v", err)
	}

	laneNode := &nodes.Node{Name: "LANE-A", Enabled: true, NodeTypeID: &laneType.ID}
	if err := db.CreateNode(laneNode); err != nil {
		t.Fatalf("create lane node: %v", err)
	}

	// Create a storage slot under the LANE.
	slotNode := &nodes.Node{Name: "SLOT-A1", Enabled: true, ParentID: &laneNode.ID}
	if err := db.CreateNode(slotNode); err != nil {
		t.Fatalf("create slot node: %v", err)
	}

	// Re-fetch to populate joined fields (NodeTypeCode on parent).
	slotNode, _ = db.GetNode(slotNode.ID)

	staged, expiresAt := eng.resolveNodeStaging(slotNode)
	if staged {
		t.Error("storage slot under LANE should resolve to staged=false")
	}
	if expiresAt != nil {
		t.Error("storage slot should have nil expiresAt (no staging)")
	}
}

// TC-RS-3: Node with non-LANE parent → staged=true (treated as lineside).
func TestResolveNodeStaging_NonLaneParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create a non-LANE parent (e.g., "AREA" type or no type).
	parentNode := &nodes.Node{Name: "AREA-B", Enabled: true}
	if err := db.CreateNode(parentNode); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	childNode := &nodes.Node{Name: "CHILD-B1", Enabled: true, ParentID: &parentNode.ID}
	if err := db.CreateNode(childNode); err != nil {
		t.Fatalf("create child: %v", err)
	}
	childNode, _ = db.GetNode(childNode.ID)

	staged, _ := eng.resolveNodeStaging(childNode)
	if !staged {
		t.Error("child of non-LANE parent should resolve to staged=true")
	}
}

// TC-RS-4: Node with no parent ID → staged=true (lineside default).
func TestResolveNodeStaging_NoParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	orphanNode := &nodes.Node{Name: "ORPHAN-1", Enabled: true}
	if err := db.CreateNode(orphanNode); err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	orphanNode, _ = db.GetNode(orphanNode.ID)

	staged, _ := eng.resolveNodeStaging(orphanNode)
	if !staged {
		t.Error("orphan node (no parent) should resolve to staged=true")
	}
}
