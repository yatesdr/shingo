//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
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

// Normal lineside node (no parent) → staged=true.
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

// Storage slot under a LANE parent → staged=false.
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
	testutil.MustNoErr(t, db.CreateNode(laneNode), "create lane node")

	// Create a storage slot under the LANE.
	slotNode := &nodes.Node{Name: "SLOT-A1", Enabled: true, ParentID: &laneNode.ID}
	testutil.MustNoErr(t, db.CreateNode(slotNode), "create slot node")

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

// Node with non-LANE parent → staged=true (treated as lineside).
func TestResolveNodeStaging_NonLaneParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create a non-LANE parent (e.g., "AREA" type or no type).
	parentNode := &nodes.Node{Name: "AREA-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(parentNode), "create parent")

	childNode := &nodes.Node{Name: "CHILD-B1", Enabled: true, ParentID: &parentNode.ID}
	testutil.MustNoErr(t, db.CreateNode(childNode), "create child")
	childNode, _ = db.GetNode(childNode.ID)

	staged, _ := eng.resolveNodeStaging(childNode)
	if !staged {
		t.Error("child of non-LANE parent should resolve to staged=true")
	}
}

// NGRP root itself → staged=false (storage container).
// Regression for the dead-string bug: the v6 SMKT→NGRP rename (commit
// 3e3fb4a) left isStorageSlot's `NodeTypeCode == "NODE_GROUP"` check
// matching nothing. Concrete symptom: a loader's L2 to an NGRP outbound
// destination delivered the bin as `staged`, which the group resolver's
// availability gate rejects — downstream retrieves from the same NGRP
// then couldn't see the just-stored bin.
func TestResolveNodeStaging_NGRPRoot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v", err)
	}
	grpNode := &nodes.Node{Name: "SMKT-AREA-A", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grpNode), "create NGRP")
	grpNode, _ = db.GetNode(grpNode.ID)

	staged, _ := eng.resolveNodeStaging(grpNode)
	if staged {
		t.Error("NGRP root should resolve to staged=false (storage container)")
	}
}

// Direct child of an NGRP → staged=false. The loader L2 path
// resolves its NGRP delivery to a concrete child via the resolver at
// order creation; that child must be recognized as storage so the bin
// lands `available`.
func TestResolveNodeStaging_NGRPDirectChild(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v", err)
	}
	grpNode := &nodes.Node{Name: "SMKT-AREA-C", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grpNode), "create NGRP")

	slotNode := &nodes.Node{Name: "SMKT-AREA-C-S1", Enabled: true, ParentID: &grpNode.ID}
	testutil.MustNoErr(t, db.CreateNode(slotNode), "create direct-child slot")
	slotNode, _ = db.GetNode(slotNode.ID)

	staged, expiresAt := eng.resolveNodeStaging(slotNode)
	if staged {
		t.Error("direct child of NGRP should resolve to staged=false (storage slot)")
	}
	if expiresAt != nil {
		t.Error("NGRP direct child should have nil expiresAt (no staging)")
	}
}

// Node with no parent ID → staged=true (lineside default).
func TestResolveNodeStaging_NoParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	orphanNode := &nodes.Node{Name: "ORPHAN-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(orphanNode), "create orphan")
	orphanNode, _ = db.GetNode(orphanNode.ID)

	staged, _ := eng.resolveNodeStaging(orphanNode)
	if !staged {
		t.Error("orphan node (no parent) should resolve to staged=true")
	}
}
