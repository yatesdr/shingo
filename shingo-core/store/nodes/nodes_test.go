//go:build docker

package nodes_test

import (
	"database/sql"
	"shingocore/store/nodes"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
)

// countNodePayloads peeks directly at the node_payloads junction table, since
// ListPayloadsForNode lives in the outer store/ package (returns *Payload).
func countNodePayloads(t *testing.T, db *sql.DB, nodeID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM node_payloads WHERE node_id=$1`, nodeID).Scan(&n), "count node_payloads")
	return n
}

// ---------------------------------------------------------------------------
// nodes.Node CRUD (nodes.go)
// ---------------------------------------------------------------------------

func TestNodeCRUD(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")
	if n.ID == 0 {
		t.Fatalf("nodes.Create: expected ID to be assigned")
	}

	got, err := nodes.Get(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.Get: %v", err)
	}
	if got.Name != "STORAGE-A1" {
		t.Errorf("nodes.Get Name = %q, want %q", got.Name, "STORAGE-A1")
	}
	if got.Zone != "A" {
		t.Errorf("nodes.Get Zone = %q, want %q", got.Zone, "A")
	}
	if !got.Enabled {
		t.Errorf("nodes.Get Enabled = false, want true")
	}

	// nodes.Update: mutate several columns, read back.
	got.Zone = "B"
	got.Enabled = false
	got.Name = "STORAGE-A1-RENAMED"
	testutil.MustNoErr(t, nodes.Update(sdb, got), "nodes.Update")
	got2, err := nodes.Get(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.Get after update: %v", err)
	}
	if got2.Zone != "B" {
		t.Errorf("Zone after update = %q, want %q", got2.Zone, "B")
	}
	if got2.Enabled {
		t.Errorf("Enabled after update = true, want false")
	}
	if got2.Name != "STORAGE-A1-RENAMED" {
		t.Errorf("Name after update = %q, want %q", got2.Name, "STORAGE-A1-RENAMED")
	}

	// nodes.Delete then nodes.Get should error.
	testutil.MustNoErr(t, nodes.Delete(sdb, n.ID), "nodes.Delete")
	if _, err := nodes.Get(sdb, n.ID); err == nil {
		t.Errorf("nodes.Get after nodes.Delete: expected error, got nil")
	}
}

func TestNodeGetByName(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "UNIQ-NAME", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")

	got, err := nodes.GetByName(sdb, "UNIQ-NAME")
	if err != nil {
		t.Fatalf("nodes.GetByName: %v", err)
	}
	if got.ID != n.ID {
		t.Errorf("nodes.GetByName ID = %d, want %d", got.ID, n.ID)
	}

	if _, err := nodes.GetByName(sdb, "DOES-NOT-EXIST"); err == nil {
		t.Errorf("nodes.GetByName missing: expected error, got nil")
	}
}

func TestNodeGetByDotName(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "PARENT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, parent), "nodes.Create parent")
	child := &nodes.Node{Name: "CHILD", Enabled: true, ParentID: &parent.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, child), "nodes.Create child")

	// Dot notation: PARENT.CHILD should resolve to the child parented under PARENT.
	got, err := nodes.GetByDotName(sdb, "PARENT.CHILD")
	if err != nil {
		t.Fatalf("nodes.GetByDotName: %v", err)
	}
	if got.ID != child.ID {
		t.Errorf("nodes.GetByDotName PARENT.CHILD ID = %d, want %d", got.ID, child.ID)
	}

	// Plain name (no dot) falls back to nodes.GetByName.
	got2, err := nodes.GetByDotName(sdb, "PARENT")
	if err != nil {
		t.Fatalf("nodes.GetByDotName PARENT: %v", err)
	}
	if got2.ID != parent.ID {
		t.Errorf("nodes.GetByDotName PARENT ID = %d, want %d", got2.ID, parent.ID)
	}
}

func TestNodeGetRoot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	root := &nodes.Node{Name: "ROOT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, root), "nodes.Create root")
	mid := &nodes.Node{Name: "MID", Enabled: true, ParentID: &root.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, mid), "nodes.Create mid")
	leaf := &nodes.Node{Name: "LEAF", Enabled: true, ParentID: &mid.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, leaf), "nodes.Create leaf")

	got, err := nodes.GetRoot(sdb, leaf.ID)
	if err != nil {
		t.Fatalf("nodes.GetRoot from leaf: %v", err)
	}
	if got.ID != root.ID {
		t.Errorf("nodes.GetRoot(leaf) ID = %d, want %d", got.ID, root.ID)
	}

	// Root of a root is itself.
	got2, err := nodes.GetRoot(sdb, root.ID)
	if err != nil {
		t.Fatalf("nodes.GetRoot from root: %v", err)
	}
	if got2.ID != root.ID {
		t.Errorf("nodes.GetRoot(root) ID = %d, want %d", got2.ID, root.ID)
	}
}

func TestNodeList(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// Fresh DB: only the synthetic _TRANSIT node from migration v15
	// (bin-transit-state Phase 1) should be present.
	empty, err := nodes.List(sdb)
	if err != nil {
		t.Fatalf("nodes.List empty: %v", err)
	}
	if len(empty) != 1 || empty[0].Name != "_TRANSIT" {
		t.Errorf("nodes.List empty = %+v, want only [_TRANSIT]", empty)
	}

	names := []string{"CHARLIE", "ALPHA", "BRAVO"}
	for _, name := range names {
		if err := nodes.Create(sdb, &nodes.Node{Name: name, Enabled: true}); err != nil {
			t.Fatalf("nodes.Create %q: %v", name, err)
		}
	}

	got, err := nodes.List(sdb)
	if err != nil {
		t.Fatalf("nodes.List: %v", err)
	}
	// 3 created + the _TRANSIT migration row.
	if len(got) != 4 {
		t.Fatalf("nodes.List len = %d, want 4 (3 created + _TRANSIT)", len(got))
	}
	// Ordered by name ascending. Underscore sorts after letters in
	// PostgreSQL's default collation, so _TRANSIT comes last.
	if got[0].Name != "ALPHA" || got[1].Name != "BRAVO" || got[2].Name != "CHARLIE" || got[3].Name != "_TRANSIT" {
		t.Errorf("nodes.List order = [%q, %q, %q, %q], want [ALPHA, BRAVO, CHARLIE, _TRANSIT]",
			got[0].Name, got[1].Name, got[2].Name, got[3].Name)
	}
}

func TestNodeListChildren(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "PARENT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, parent), "nodes.Create parent")
	unrelated := &nodes.Node{Name: "UNRELATED", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, unrelated), "nodes.Create unrelated")

	childNames := []string{"ZETA", "ALPHA", "MU"}
	for _, name := range childNames {
		if err := nodes.Create(sdb, &nodes.Node{Name: name, Enabled: true, ParentID: &parent.ID}); err != nil {
			t.Fatalf("nodes.Create child %q: %v", name, err)
		}
	}

	children, err := nodes.ListChildren(sdb, parent.ID)
	if err != nil {
		t.Fatalf("nodes.ListChildren: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("nodes.ListChildren len = %d, want 3", len(children))
	}
	if children[0].Name != "ALPHA" || children[1].Name != "MU" || children[2].Name != "ZETA" {
		t.Errorf("nodes.ListChildren order = [%q, %q, %q], want [ALPHA, MU, ZETA]",
			children[0].Name, children[1].Name, children[2].Name)
	}

	// No children.
	noKids, err := nodes.ListChildren(sdb, unrelated.ID)
	if err != nil {
		t.Fatalf("nodes.ListChildren unrelated: %v", err)
	}
	if len(noKids) != 0 {
		t.Errorf("nodes.ListChildren for childless node len = %d, want 0", len(noKids))
	}
}

func TestNodeSetAndClearParent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "P", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, parent), "nodes.Create parent")
	child := &nodes.Node{Name: "C", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, child), "nodes.Create child")

	testutil.MustNoErr(t, nodes.SetParent(sdb, child.ID, parent.ID), "nodes.SetParent")
	got, err := nodes.Get(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.Get after nodes.SetParent: %v", err)
	}
	if got.ParentID == nil || *got.ParentID != parent.ID {
		t.Errorf("ParentID after nodes.SetParent = %v, want %d", got.ParentID, parent.ID)
	}

	testutil.MustNoErr(t, nodes.ClearParent(sdb, child.ID), "nodes.ClearParent")
	got2, err := nodes.Get(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.Get after nodes.ClearParent: %v", err)
	}
	if got2.ParentID != nil {
		t.Errorf("ParentID after nodes.ClearParent = %v, want nil", got2.ParentID)
	}
}

func TestNodeReparent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "L", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, lane), "nodes.Create lane")
	slot := &nodes.Node{Name: "S", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "nodes.Create slot")
	// Seed role property — orphaning should clear it.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, slot.ID, "role", "store"), "nodes.SetProperty role")

	// Adopt with a position -> depth set.
	testutil.MustNoErr(t, nodes.Reparent(sdb, slot.ID, &lane.ID, 3), "nodes.Reparent adopt")
	got, err := nodes.Get(sdb, slot.ID)
	if err != nil {
		t.Fatalf("nodes.Get after nodes.Reparent adopt: %v", err)
	}
	if got.ParentID == nil || *got.ParentID != lane.ID {
		t.Errorf("ParentID after adopt = %v, want %d", got.ParentID, lane.ID)
	}
	if got.Depth == nil || *got.Depth != 3 {
		t.Errorf("Depth after adopt = %v, want 3", got.Depth)
	}

	// Orphan: parent cleared, depth cleared, role property cleared.
	testutil.MustNoErr(t, nodes.Reparent(sdb, slot.ID, nil, 0), "nodes.Reparent orphan")
	got2, err := nodes.Get(sdb, slot.ID)
	if err != nil {
		t.Fatalf("nodes.Get after nodes.Reparent orphan: %v", err)
	}
	if got2.ParentID != nil {
		t.Errorf("ParentID after orphan = %v, want nil", got2.ParentID)
	}
	if got2.Depth != nil {
		t.Errorf("Depth after orphan = %v, want nil", got2.Depth)
	}
	if role := nodes.GetProperty(sdb, slot.ID, "role"); role != "" {
		t.Errorf("role after orphan = %q, want \"\"", role)
	}
}

func TestReorderLaneSlots(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "LANE-X", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, lane), "nodes.Create lane")
	d1, d2, d3 := 1, 2, 3
	s1 := &nodes.Node{Name: "S1", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	s2 := &nodes.Node{Name: "S2", Enabled: true, ParentID: &lane.ID, Depth: &d2}
	s3 := &nodes.Node{Name: "S3", Enabled: true, ParentID: &lane.ID, Depth: &d3}
	for _, s := range []*nodes.Node{s1, s2, s3} {
		if err := nodes.Create(sdb, s); err != nil {
			t.Fatalf("nodes.Create slot %q: %v", s.Name, err)
		}
	}

	// Reverse the order: s3 -> depth 1, s2 -> depth 2, s1 -> depth 3.
	testutil.MustNoErr(t, nodes.ReorderLaneSlots(sdb, lane.ID, []int64{s3.ID, s2.ID, s1.ID}), "nodes.ReorderLaneSlots")

	cases := map[int64]int{s3.ID: 1, s2.ID: 2, s1.ID: 3}
	for id, want := range cases {
		got, err := nodes.GetSlotDepth(sdb, id)
		if err != nil {
			t.Fatalf("nodes.GetSlotDepth %d: %v", id, err)
		}
		if got != want {
			t.Errorf("slot %d depth = %d, want %d", id, got, want)
		}
	}
}

func TestScanNodesEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// Exercise nodes.ScanNodes directly against an empty-but-valid row set.
	rows, err := sdb.Query(`SELECT ` + nodes.SelectCols + ` ` + nodes.FromClause + ` WHERE 1=0`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	got, err := nodes.ScanNodes(rows)
	if err != nil {
		t.Fatalf("nodes.ScanNodes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nodes.ScanNodes empty len = %d, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// nodes.NodeType CRUD (types.go)
// ---------------------------------------------------------------------------

func TestNodeTypeCRUD(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	nt := &nodes.NodeType{Code: "TSTOR", Name: "Storage", Description: "storage slot", IsSynthetic: false}
	testutil.MustNoErr(t, nodes.CreateType(sdb, nt), "nodes.CreateType")
	if nt.ID == 0 {
		t.Fatalf("nodes.CreateType: expected ID to be assigned")
	}

	got, err := nodes.GetType(sdb, nt.ID)
	if err != nil {
		t.Fatalf("nodes.GetType: %v", err)
	}
	if got.Code != "TSTOR" || got.Name != "Storage" || got.Description != "storage slot" {
		t.Errorf("nodes.GetType = %+v, want code=TSTOR name=Storage description=\"storage slot\"", got)
	}
	if got.IsSynthetic {
		t.Errorf("IsSynthetic = true, want false")
	}

	// nodes.GetTypeByCode.
	got2, err := nodes.GetTypeByCode(sdb, "TSTOR")
	if err != nil {
		t.Fatalf("nodes.GetTypeByCode: %v", err)
	}
	if got2.ID != nt.ID {
		t.Errorf("nodes.GetTypeByCode ID = %d, want %d", got2.ID, nt.ID)
	}

	// nodes.Update.
	got.Name = "Storage Slot"
	got.IsSynthetic = true
	testutil.MustNoErr(t, nodes.UpdateType(sdb, got), "nodes.UpdateType")
	got3, err := nodes.GetType(sdb, nt.ID)
	if err != nil {
		t.Fatalf("nodes.GetType after update: %v", err)
	}
	if got3.Name != "Storage Slot" {
		t.Errorf("Name after update = %q, want %q", got3.Name, "Storage Slot")
	}
	if !got3.IsSynthetic {
		t.Errorf("IsSynthetic after update = false, want true")
	}

	// nodes.List: LANE and NGRP are seeded by migration; TSTOR makes 3 total.
	second := &nodes.NodeType{Code: "TSTOR2", Name: "Extra", IsSynthetic: true}
	testutil.MustNoErr(t, nodes.CreateType(sdb, second), "nodes.CreateType second")
	list, err := nodes.ListTypes(sdb)
	if err != nil {
		t.Fatalf("nodes.ListTypes: %v", err)
	}
	// seeded: LANE, NGRP + created: TSTOR, TSTOR2 = 4
	if len(list) != 4 {
		t.Fatalf("nodes.ListTypes len = %d, want 4", len(list))
	}
	if list[0].Code != "LANE" || list[3].Code != "TSTOR2" {
		t.Errorf("nodes.ListTypes order = [%q, ..., %q], want [LANE, ..., TSTOR2]", list[0].Code, list[3].Code)
	}

	// nodes.Delete.
	testutil.MustNoErr(t, nodes.DeleteType(sdb, second.ID), "nodes.DeleteType")
	if _, err := nodes.GetType(sdb, second.ID); err == nil {
		t.Errorf("nodes.GetType after nodes.DeleteType: expected error, got nil")
	}
}

func TestNodeTypeGetByCodeMissing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	if _, err := nodes.GetTypeByCode(sdb, "NOPE"); err == nil {
		t.Errorf("nodes.GetTypeByCode missing: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Group + Lane layout (group.go, lanes.go)
// ---------------------------------------------------------------------------

func TestCreateGroupAndAddLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// Look up seeded types used by CreateGroup/AddLane.
	ngrp, err := nodes.GetTypeByCode(sdb, "NGRP")
	if err != nil {
		t.Fatalf("lookup NGRP type: %v", err)
	}
	lane, err := nodes.GetTypeByCode(sdb, "LANE")
	if err != nil {
		t.Fatalf("lookup LANE type: %v", err)
	}

	grpID, err := nodes.CreateGroup(sdb, "GRP-A")
	if err != nil {
		t.Fatalf("nodes.CreateGroup: %v", err)
	}
	if grpID == 0 {
		t.Fatalf("nodes.CreateGroup: expected non-zero ID")
	}

	grp, err := nodes.Get(sdb, grpID)
	if err != nil {
		t.Fatalf("nodes.Get group: %v", err)
	}
	if grp.Name != "GRP-A" {
		t.Errorf("group Name = %q, want %q", grp.Name, "GRP-A")
	}
	if !grp.IsSynthetic {
		t.Errorf("group IsSynthetic = false, want true")
	}
	if grp.NodeTypeID == nil || *grp.NodeTypeID != ngrp.ID {
		t.Errorf("group NodeTypeID = %v, want %d", grp.NodeTypeID, ngrp.ID)
	}

	laneID, err := nodes.AddLane(sdb, grpID, "LAN-1")
	if err != nil {
		t.Fatalf("nodes.AddLane: %v", err)
	}
	laneNode, err := nodes.Get(sdb, laneID)
	if err != nil {
		t.Fatalf("nodes.Get lane: %v", err)
	}
	if laneNode.ParentID == nil || *laneNode.ParentID != grpID {
		t.Errorf("lane ParentID = %v, want %d", laneNode.ParentID, grpID)
	}
	if laneNode.NodeTypeID == nil || *laneNode.NodeTypeID != lane.ID {
		t.Errorf("lane NodeTypeID = %v, want %d", laneNode.NodeTypeID, lane.ID)
	}
	if !laneNode.IsSynthetic {
		t.Errorf("lane IsSynthetic = false, want true")
	}

	// nodes.ListChildren: the group's single child should be the lane.
	children, err := nodes.ListChildren(sdb, grpID)
	if err != nil {
		t.Fatalf("nodes.ListChildren group: %v", err)
	}
	if len(children) != 1 || children[0].ID != laneID {
		t.Errorf("group children = %+v, want exactly lane %d", children, laneID)
	}
}

func TestCreateGroupDuplicateName(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	_, err := nodes.CreateGroup(sdb, "GRP-DUP")
	if err != nil {
		t.Fatalf("first CreateGroup: %v", err)
	}
	// Duplicate name should fail due to nodes_name_key unique constraint.
	if _, err := nodes.CreateGroup(sdb, "GRP-DUP"); err == nil {
		t.Error("duplicate group name should have been rejected")
	}
}

func TestAddLaneMissingGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// Non-existent group ID — LANE type is pre-seeded by migration.
	if _, err := nodes.AddLane(sdb, 99999, "LAN"); err == nil {
		t.Errorf("nodes.AddLane with missing group: expected error, got nil")
	}
}

func TestDeleteGroupHierarchy(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// NGRP and LANE types are pre-seeded by migration.

	grpID, err := nodes.CreateGroup(sdb, "GRP-DEL")
	if err != nil {
		t.Fatalf("nodes.CreateGroup: %v", err)
	}
	laneID, err := nodes.AddLane(sdb, grpID, "LAN-DEL")
	if err != nil {
		t.Fatalf("nodes.AddLane: %v", err)
	}

	// Physical slot under the lane: should survive nodes.DeleteGroup but be unparented.
	physical := &nodes.Node{Name: "PHYS-1", Enabled: true, ParentID: &laneID}
	testutil.MustNoErr(t, nodes.Create(sdb, physical), "nodes.Create physical")
	// Give it a "role" property so we can verify nodes.DeleteGroup clears it.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, physical.ID, "role", "store"), "nodes.SetProperty role")
	// And an unrelated property that must NOT be cleared.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, physical.ID, "notes", "keep-me"), "nodes.SetProperty notes")

	testutil.MustNoErr(t, nodes.DeleteGroup(sdb, grpID), "nodes.DeleteGroup")

	// Synthetic group and lane are gone.
	if _, err := nodes.Get(sdb, grpID); err == nil {
		t.Errorf("nodes.Get grp after nodes.DeleteGroup: expected error, got nil")
	}
	if _, err := nodes.Get(sdb, laneID); err == nil {
		t.Errorf("nodes.Get lane after nodes.DeleteGroup: expected error, got nil")
	}

	// Physical slot survived and was unparented.
	gotPhys, err := nodes.Get(sdb, physical.ID)
	if err != nil {
		t.Fatalf("nodes.Get physical after nodes.DeleteGroup: %v", err)
	}
	if gotPhys.ParentID != nil {
		t.Errorf("physical.ParentID after nodes.DeleteGroup = %v, want nil", gotPhys.ParentID)
	}

	if role := nodes.GetProperty(sdb, physical.ID, "role"); role != "" {
		t.Errorf("role after nodes.DeleteGroup = %q, want \"\"", role)
	}
	if notes := nodes.GetProperty(sdb, physical.ID, "notes"); notes != "keep-me" {
		t.Errorf("notes after nodes.DeleteGroup = %q, want %q", notes, "keep-me")
	}
}

func TestListLaneSlotsOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	laneParent := &nodes.Node{Name: "LAN", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, laneParent), "nodes.Create lane")
	d1, d2, d3 := 1, 2, 3
	s2 := &nodes.Node{Name: "S2", Enabled: true, ParentID: &laneParent.ID, Depth: &d2}
	s1 := &nodes.Node{Name: "S1", Enabled: true, ParentID: &laneParent.ID, Depth: &d1}
	s3 := &nodes.Node{Name: "S3", Enabled: true, ParentID: &laneParent.ID, Depth: &d3}
	for _, s := range []*nodes.Node{s2, s1, s3} {
		if err := nodes.Create(sdb, s); err != nil {
			t.Fatalf("nodes.Create %q: %v", s.Name, err)
		}
	}

	slots, err := nodes.ListLaneSlots(sdb, laneParent.ID)
	if err != nil {
		t.Fatalf("nodes.ListLaneSlots: %v", err)
	}
	if len(slots) != 3 {
		t.Fatalf("nodes.ListLaneSlots len = %d, want 3", len(slots))
	}
	if slots[0].ID != s1.ID || slots[1].ID != s2.ID || slots[2].ID != s3.ID {
		t.Errorf("nodes.ListLaneSlots order = [%d, %d, %d], want [%d, %d, %d]",
			slots[0].ID, slots[1].ID, slots[2].ID, s1.ID, s2.ID, s3.ID)
	}
}

func TestGetSlotDepthUnset(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "NO-DEPTH", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")
	got, err := nodes.GetSlotDepth(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.GetSlotDepth: %v", err)
	}
	if got != 0 {
		t.Errorf("nodes.GetSlotDepth with no depth = %d, want 0", got)
	}

	// Missing node: should return error.
	if _, err := nodes.GetSlotDepth(sdb, 999999); err == nil {
		t.Errorf("nodes.GetSlotDepth on missing node: expected error, got nil")
	}
}

func TestIsSlotAccessibleRootNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	root := &nodes.Node{Name: "ROOT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, root), "nodes.Create root")

	// A node with no parent is accessible.
	ok, err := nodes.IsSlotAccessible(sdb, root.ID)
	if err != nil {
		t.Fatalf("nodes.IsSlotAccessible root: %v", err)
	}
	if !ok {
		t.Errorf("nodes.IsSlotAccessible(root-no-parent) = false, want true")
	}

	// Child with no Depth set is accessible by the "no depth = accessible" rule.
	child := &nodes.Node{Name: "CHILD", Enabled: true, ParentID: &root.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, child), "nodes.Create child")
	ok2, err := nodes.IsSlotAccessible(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.IsSlotAccessible child: %v", err)
	}
	if !ok2 {
		t.Errorf("nodes.IsSlotAccessible(child-no-depth) = false, want true")
	}
}

func TestFindStoreSlotInLaneEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "LN", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, lane), "nodes.Create lane")

	// No slots -> expect error ("no empty slot").
	if _, err := nodes.FindStoreSlotInLane(sdb, lane.ID); err == nil {
		t.Errorf("nodes.FindStoreSlotInLane empty lane: expected error, got nil")
	}
}

func TestCountBinsInLaneNoBins(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "LN", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, lane), "nodes.Create lane")
	d := 1
	slot := &nodes.Node{Name: "SL", Enabled: true, ParentID: &lane.ID, Depth: &d}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "nodes.Create slot")

	count, err := nodes.CountBinsInLane(sdb, lane.ID)
	if err != nil {
		t.Fatalf("nodes.CountBinsInLane: %v", err)
	}
	if count != 0 {
		t.Errorf("nodes.CountBinsInLane empty = %d, want 0", count)
	}
}

// ---------------------------------------------------------------------------
// Properties (properties.go)
// ---------------------------------------------------------------------------

func TestNodeProperties_SetGetDelete(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N1", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")

	// Unset key returns empty string.
	if v := nodes.GetProperty(sdb, n.ID, "role"); v != "" {
		t.Errorf("nodes.GetProperty unset = %q, want \"\"", v)
	}

	// Set, then read back.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, n.ID, "role", "store"), "nodes.SetProperty")
	if v := nodes.GetProperty(sdb, n.ID, "role"); v != "store" {
		t.Errorf("nodes.GetProperty after set = %q, want %q", v, "store")
	}

	// Upsert: same key re-set updates the value.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, n.ID, "role", "retrieve"), "nodes.SetProperty upsert")
	if v := nodes.GetProperty(sdb, n.ID, "role"); v != "retrieve" {
		t.Errorf("nodes.GetProperty after upsert = %q, want %q", v, "retrieve")
	}

	// Add a second property.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, n.ID, "capacity", "42"), "nodes.SetProperty capacity")

	// nodes.ListProperties -- ordered by key ASC.
	list, err := nodes.ListProperties(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListProperties: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("nodes.ListProperties len = %d, want 2", len(list))
	}
	if list[0].Key != "capacity" || list[0].Value != "42" {
		t.Errorf("nodes.ListProperties[0] = %+v, want capacity=42", list[0])
	}
	if list[1].Key != "role" || list[1].Value != "retrieve" {
		t.Errorf("nodes.ListProperties[1] = %+v, want role=retrieve", list[1])
	}
	if list[0].NodeID != n.ID {
		t.Errorf("nodes.Property NodeID = %d, want %d", list[0].NodeID, n.ID)
	}

	// nodes.Delete one, list shrinks.
	testutil.MustNoErr(t, nodes.DeleteProperty(sdb, n.ID, "role"), "nodes.DeleteProperty")
	if v := nodes.GetProperty(sdb, n.ID, "role"); v != "" {
		t.Errorf("nodes.GetProperty after delete = %q, want \"\"", v)
	}
	list2, err := nodes.ListProperties(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListProperties after delete: %v", err)
	}
	if len(list2) != 1 || list2[0].Key != "capacity" {
		t.Errorf("nodes.ListProperties after delete = %+v, want [capacity]", list2)
	}

	// nodes.Property type alias points at domain.NodeProperty — sanity check.
	var _ *domain.NodeProperty = list2[0]
}

func TestListPropertiesEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "EMPTY", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")
	list, err := nodes.ListProperties(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListProperties: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("nodes.ListProperties empty len = %d, want 0", len(list))
	}
}

// ---------------------------------------------------------------------------
// Station bindings (stations.go)
// ---------------------------------------------------------------------------

func TestStations_AssignListUnassign(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N-STAT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")

	testutil.MustNoErr(t, nodes.AssignStation(sdb, n.ID, "line-1"), "nodes.AssignStation line-1")
	// ON CONFLICT DO NOTHING: assigning same station again is a no-op (no error).
	testutil.MustNoErr(t, nodes.AssignStation(sdb, n.ID, "line-1"), "nodes.AssignStation line-1 dup")
	testutil.MustNoErr(t, nodes.AssignStation(sdb, n.ID, "line-2"), "nodes.AssignStation line-2")

	list, err := nodes.ListStationsForNode(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListStationsForNode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("nodes.ListStationsForNode len = %d, want 2 (dup ignored)", len(list))
	}
	if list[0] != "line-1" || list[1] != "line-2" {
		t.Errorf("nodes.ListStationsForNode = %v, want [line-1 line-2]", list)
	}

	testutil.MustNoErr(t, nodes.UnassignStation(sdb, n.ID, "line-1"), "nodes.UnassignStation")
	list2, err := nodes.ListStationsForNode(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListStationsForNode after unassign: %v", err)
	}
	if len(list2) != 1 || list2[0] != "line-2" {
		t.Errorf("nodes.ListStationsForNode after unassign = %v, want [line-2]", list2)
	}
}

func TestSetStationsReplacesAll(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N-SET", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")

	testutil.MustNoErr(t, nodes.AssignStation(sdb, n.ID, "alpha"), "nodes.AssignStation alpha")
	testutil.MustNoErr(t, nodes.AssignStation(sdb, n.ID, "beta"), "nodes.AssignStation beta")

	// nodes.SetStations replaces the full set.
	testutil.MustNoErr(t, nodes.SetStations(sdb, n.ID, []string{"gamma", "delta"}), "nodes.SetStations")
	list, err := nodes.ListStationsForNode(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListStationsForNode: %v", err)
	}
	if len(list) != 2 || list[0] != "delta" || list[1] != "gamma" {
		t.Errorf("nodes.ListStationsForNode = %v, want [delta gamma]", list)
	}

	// nodes.SetStations with empty slice clears everything.
	testutil.MustNoErr(t, nodes.SetStations(sdb, n.ID, nil), "nodes.SetStations nil")
	list2, err := nodes.ListStationsForNode(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListStationsForNode empty: %v", err)
	}
	if len(list2) != 0 {
		t.Errorf("nodes.ListStationsForNode after nodes.SetStations(nil) = %v, want []", list2)
	}
}

func TestListNodesForStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// Look up seeded NGRP type (ListNodesForStation queries by code='NGRP').
	ngrp, err := nodes.GetTypeByCode(sdb, "NGRP")
	if err != nil {
		t.Fatalf("lookup NGRP type: %v", err)
	}

	// Directly assigned top-level node.
	direct := &nodes.Node{Name: "DIRECT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, direct), "nodes.Create direct")
	testutil.MustNoErr(t, nodes.AssignStation(sdb, direct.ID, "line-1"), "nodes.AssignStation direct")

	// Group + child under it: station assigned to the child; group should
	// surface too because of the NGRP parent rollup clause.
	grp := &nodes.Node{Name: "GRP", Enabled: true, IsSynthetic: true, NodeTypeID: &ngrp.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, grp), "nodes.Create grp")
	kid := &nodes.Node{Name: "KID", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, kid), "nodes.Create kid")
	testutil.MustNoErr(t, nodes.AssignStation(sdb, kid.ID, "line-1"), "nodes.AssignStation kid")

	// Unrelated node — must not appear.
	testutil.MustNoErr(t, nodes.Create(sdb, &nodes.Node{Name: "UNREL", Enabled: true}), "nodes.Create unrel")

	nodes, err := nodes.ListNodesForStation(sdb, "line-1")
	if err != nil {
		t.Fatalf("nodes.ListNodesForStation: %v", err)
	}
	names := map[string]bool{}
	for _, n := range nodes {
		names[n.Name] = true
	}
	if !names["DIRECT"] {
		t.Errorf("nodes.ListNodesForStation missing DIRECT: got %v", names)
	}
	if !names["GRP"] {
		t.Errorf("nodes.ListNodesForStation missing GRP (NGRP rollup): got %v", names)
	}
	if !names["KID"] {
		t.Errorf("nodes.ListNodesForStation missing KID (child of NGRP): got %v", names)
	}
	if names["UNREL"] {
		t.Errorf("nodes.ListNodesForStation should not include UNREL: got %v", names)
	}
}

func TestGetEffectiveStationsModes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "P", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, parent), "nodes.Create parent")
	testutil.MustNoErr(t, nodes.AssignStation(sdb, parent.ID, "parent-line"), "nodes.AssignStation parent")

	child := &nodes.Node{Name: "C", Enabled: true, ParentID: &parent.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, child), "nodes.Create child")

	// Mode "all" -> returns nil.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, child.ID, "station_mode", "all"), "nodes.SetProperty all")
	got, err := nodes.GetEffectiveStations(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.GetEffectiveStations all: %v", err)
	}
	if got != nil {
		t.Errorf("nodes.GetEffectiveStations(all) = %v, want nil", got)
	}

	// Mode "none" -> empty non-nil slice.
	testutil.MustNoErr(t, nodes.SetProperty(sdb, child.ID, "station_mode", "none"), "nodes.SetProperty none")
	got, err = nodes.GetEffectiveStations(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.GetEffectiveStations none: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("nodes.GetEffectiveStations(none) = %v, want empty non-nil slice", got)
	}

	// Mode "specific" -> direct assignments only (seed a child assignment).
	testutil.MustNoErr(t, nodes.SetProperty(sdb, child.ID, "station_mode", "specific"), "nodes.SetProperty specific")
	testutil.MustNoErr(t, nodes.AssignStation(sdb, child.ID, "child-line"), "nodes.AssignStation child")
	got, err = nodes.GetEffectiveStations(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.GetEffectiveStations specific: %v", err)
	}
	if len(got) != 1 || got[0] != "child-line" {
		t.Errorf("nodes.GetEffectiveStations(specific) = %v, want [child-line]", got)
	}

	// Mode inherit: delete the child's own assignment, then inherit walks
	// up to parent-line.
	testutil.MustNoErr(t, nodes.UnassignStation(sdb, child.ID, "child-line"), "nodes.UnassignStation child")
	testutil.MustNoErr(t, nodes.DeleteProperty(sdb, child.ID, "station_mode"), "nodes.DeleteProperty station_mode")
	got, err = nodes.GetEffectiveStations(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.GetEffectiveStations inherit: %v", err)
	}
	if len(got) != 1 || got[0] != "parent-line" {
		t.Errorf("nodes.GetEffectiveStations(inherit) = %v, want [parent-line]", got)
	}
}

// ---------------------------------------------------------------------------
// Payload bindings (payloads_link.go)
// ---------------------------------------------------------------------------

func TestPayloadAssignments(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N-PL", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, n), "nodes.Create")

	// Seed two payload templates directly — no nodes-package CRUD for payloads.
	var pl1ID, pl2ID int64
	if err := sdb.QueryRow(
		`INSERT INTO payloads (code, description, uop_capacity) VALUES ($1, $2, $3) RETURNING id`,
		"PAY-1", "p1", 10).Scan(&pl1ID); err != nil {
		t.Fatalf("insert payload 1: %v", err)
	}
	if err := sdb.QueryRow(
		`INSERT INTO payloads (code, description, uop_capacity) VALUES ($1, $2, $3) RETURNING id`,
		"PAY-2", "p2", 20).Scan(&pl2ID); err != nil {
		t.Fatalf("insert payload 2: %v", err)
	}

	// nodes.AssignPayload.
	testutil.MustNoErr(t, nodes.AssignPayload(sdb, n.ID, pl1ID), "nodes.AssignPayload pl1")
	// Duplicate assign — ON CONFLICT DO NOTHING, no error.
	testutil.MustNoErr(t, nodes.AssignPayload(sdb, n.ID, pl1ID), "nodes.AssignPayload pl1 dup")
	testutil.MustNoErr(t, nodes.AssignPayload(sdb, n.ID, pl2ID), "nodes.AssignPayload pl2")

	if got := countNodePayloads(t, sdb, n.ID); got != 2 {
		t.Errorf("node_payloads count after assigns = %d, want 2", got)
	}

	// nodes.UnassignPayload.
	testutil.MustNoErr(t, nodes.UnassignPayload(sdb, n.ID, pl1ID), "nodes.UnassignPayload")
	if got := countNodePayloads(t, sdb, n.ID); got != 1 {
		t.Errorf("node_payloads count after unassign = %d, want 1", got)
	}

	// nodes.SetPayloads replaces the set entirely.
	testutil.MustNoErr(t, nodes.SetPayloads(sdb, n.ID, []int64{pl1ID, pl2ID}), "nodes.SetPayloads")
	if got := countNodePayloads(t, sdb, n.ID); got != 2 {
		t.Errorf("node_payloads count after nodes.SetPayloads = %d, want 2", got)
	}

	// nodes.SetPayloads nil clears.
	testutil.MustNoErr(t, nodes.SetPayloads(sdb, n.ID, nil), "nodes.SetPayloads nil")
	if got := countNodePayloads(t, sdb, n.ID); got != 0 {
		t.Errorf("node_payloads count after nodes.SetPayloads(nil) = %d, want 0", got)
	}
}
