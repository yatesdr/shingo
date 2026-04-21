//go:build docker

package nodes_test

import (
	"shingocore/store/nodes"
	"database/sql"
	"testing"

	"shingocore/domain"
	"shingocore/internal/testdb"
)

// countNodePayloads peeks directly at the node_payloads junction table, since
// ListPayloadsForNode lives in the outer store/ package (returns *Payload).
func countNodePayloads(t *testing.T, db *sql.DB, nodeID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_payloads WHERE node_id=$1`, nodeID).Scan(&n); err != nil {
		t.Fatalf("count node_payloads: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// nodes.Node CRUD (nodes.go)
// ---------------------------------------------------------------------------

func TestNodeCRUD(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}
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
	if err := nodes.Update(sdb, got); err != nil {
		t.Fatalf("nodes.Update: %v", err)
	}
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
	if err := nodes.Delete(sdb, n.ID); err != nil {
		t.Fatalf("nodes.Delete: %v", err)
	}
	if _, err := nodes.Get(sdb, n.ID); err == nil {
		t.Errorf("nodes.Get after nodes.Delete: expected error, got nil")
	}
}

func TestNodeGetByName(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "UNIQ-NAME", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "PARENT", Enabled: true}
	if err := nodes.Create(sdb, parent); err != nil {
		t.Fatalf("nodes.Create parent: %v", err)
	}
	child := &nodes.Node{Name: "CHILD", Enabled: true, ParentID: &parent.ID}
	if err := nodes.Create(sdb, child); err != nil {
		t.Fatalf("nodes.Create child: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	root := &nodes.Node{Name: "ROOT", Enabled: true}
	if err := nodes.Create(sdb, root); err != nil {
		t.Fatalf("nodes.Create root: %v", err)
	}
	mid := &nodes.Node{Name: "MID", Enabled: true, ParentID: &root.ID}
	if err := nodes.Create(sdb, mid); err != nil {
		t.Fatalf("nodes.Create mid: %v", err)
	}
	leaf := &nodes.Node{Name: "LEAF", Enabled: true, ParentID: &mid.ID}
	if err := nodes.Create(sdb, leaf); err != nil {
		t.Fatalf("nodes.Create leaf: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	// Fresh DB: list should be empty.
	empty, err := nodes.List(sdb)
	if err != nil {
		t.Fatalf("nodes.List empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("nodes.List empty len = %d, want 0", len(empty))
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
	if len(got) != 3 {
		t.Fatalf("nodes.List len = %d, want 3", len(got))
	}
	// Ordered by name ascending.
	if got[0].Name != "ALPHA" || got[1].Name != "BRAVO" || got[2].Name != "CHARLIE" {
		t.Errorf("nodes.List order = [%q, %q, %q], want [ALPHA, BRAVO, CHARLIE]",
			got[0].Name, got[1].Name, got[2].Name)
	}
}

func TestNodeListChildren(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "PARENT", Enabled: true}
	if err := nodes.Create(sdb, parent); err != nil {
		t.Fatalf("nodes.Create parent: %v", err)
	}
	unrelated := &nodes.Node{Name: "UNRELATED", Enabled: true}
	if err := nodes.Create(sdb, unrelated); err != nil {
		t.Fatalf("nodes.Create unrelated: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "P", Enabled: true}
	if err := nodes.Create(sdb, parent); err != nil {
		t.Fatalf("nodes.Create parent: %v", err)
	}
	child := &nodes.Node{Name: "C", Enabled: true}
	if err := nodes.Create(sdb, child); err != nil {
		t.Fatalf("nodes.Create child: %v", err)
	}

	if err := nodes.SetParent(sdb, child.ID, parent.ID); err != nil {
		t.Fatalf("nodes.SetParent: %v", err)
	}
	got, err := nodes.Get(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.Get after nodes.SetParent: %v", err)
	}
	if got.ParentID == nil || *got.ParentID != parent.ID {
		t.Errorf("ParentID after nodes.SetParent = %v, want %d", got.ParentID, parent.ID)
	}

	if err := nodes.ClearParent(sdb, child.ID); err != nil {
		t.Fatalf("nodes.ClearParent: %v", err)
	}
	got2, err := nodes.Get(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.Get after nodes.ClearParent: %v", err)
	}
	if got2.ParentID != nil {
		t.Errorf("ParentID after nodes.ClearParent = %v, want nil", got2.ParentID)
	}
}

func TestNodeReparent(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "L", Enabled: true}
	if err := nodes.Create(sdb, lane); err != nil {
		t.Fatalf("nodes.Create lane: %v", err)
	}
	slot := &nodes.Node{Name: "S", Enabled: true}
	if err := nodes.Create(sdb, slot); err != nil {
		t.Fatalf("nodes.Create slot: %v", err)
	}
	// Seed role property — orphaning should clear it.
	if err := nodes.SetProperty(sdb, slot.ID, "role", "store"); err != nil {
		t.Fatalf("nodes.SetProperty role: %v", err)
	}

	// Adopt with a position -> depth set.
	if err := nodes.Reparent(sdb, slot.ID, &lane.ID, 3); err != nil {
		t.Fatalf("nodes.Reparent adopt: %v", err)
	}
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
	if err := nodes.Reparent(sdb, slot.ID, nil, 0); err != nil {
		t.Fatalf("nodes.Reparent orphan: %v", err)
	}
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
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "LANE-X", Enabled: true}
	if err := nodes.Create(sdb, lane); err != nil {
		t.Fatalf("nodes.Create lane: %v", err)
	}
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
	if err := nodes.ReorderLaneSlots(sdb, lane.ID, []int64{s3.ID, s2.ID, s1.ID}); err != nil {
		t.Fatalf("nodes.ReorderLaneSlots: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	nt := &nodes.NodeType{Code: "TSTOR", Name: "Storage", Description: "storage slot", IsSynthetic: false}
	if err := nodes.CreateType(sdb, nt); err != nil {
		t.Fatalf("nodes.CreateType: %v", err)
	}
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
	if err := nodes.UpdateType(sdb, got); err != nil {
		t.Fatalf("nodes.UpdateType: %v", err)
	}
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
	if err := nodes.CreateType(sdb, second); err != nil {
		t.Fatalf("nodes.CreateType second: %v", err)
	}
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
	if err := nodes.DeleteType(sdb, second.ID); err != nil {
		t.Fatalf("nodes.DeleteType: %v", err)
	}
	if _, err := nodes.GetType(sdb, second.ID); err == nil {
		t.Errorf("nodes.GetType after nodes.DeleteType: expected error, got nil")
	}
}

func TestNodeTypeGetByCodeMissing(t *testing.T) {
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
	db := testdb.Open(t)
	sdb := db.DB

	// Non-existent group ID — LANE type is pre-seeded by migration.
	if _, err := nodes.AddLane(sdb, 99999, "LAN"); err == nil {
		t.Errorf("nodes.AddLane with missing group: expected error, got nil")
	}
}

func TestDeleteGroupHierarchy(t *testing.T) {
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
	if err := nodes.Create(sdb, physical); err != nil {
		t.Fatalf("nodes.Create physical: %v", err)
	}
	// Give it a "role" property so we can verify nodes.DeleteGroup clears it.
	if err := nodes.SetProperty(sdb, physical.ID, "role", "store"); err != nil {
		t.Fatalf("nodes.SetProperty role: %v", err)
	}
	// And an unrelated property that must NOT be cleared.
	if err := nodes.SetProperty(sdb, physical.ID, "notes", "keep-me"); err != nil {
		t.Fatalf("nodes.SetProperty notes: %v", err)
	}

	if err := nodes.DeleteGroup(sdb, grpID); err != nil {
		t.Fatalf("nodes.DeleteGroup: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	laneParent := &nodes.Node{Name: "LAN", Enabled: true}
	if err := nodes.Create(sdb, laneParent); err != nil {
		t.Fatalf("nodes.Create lane: %v", err)
	}
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
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "NO-DEPTH", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}
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
	db := testdb.Open(t)
	sdb := db.DB

	root := &nodes.Node{Name: "ROOT", Enabled: true}
	if err := nodes.Create(sdb, root); err != nil {
		t.Fatalf("nodes.Create root: %v", err)
	}

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
	if err := nodes.Create(sdb, child); err != nil {
		t.Fatalf("nodes.Create child: %v", err)
	}
	ok2, err := nodes.IsSlotAccessible(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.IsSlotAccessible child: %v", err)
	}
	if !ok2 {
		t.Errorf("nodes.IsSlotAccessible(child-no-depth) = false, want true")
	}
}

func TestFindStoreSlotInLaneEmpty(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "LN", Enabled: true}
	if err := nodes.Create(sdb, lane); err != nil {
		t.Fatalf("nodes.Create lane: %v", err)
	}

	// No slots -> expect error ("no empty slot").
	if _, err := nodes.FindStoreSlotInLane(sdb, lane.ID); err == nil {
		t.Errorf("nodes.FindStoreSlotInLane empty lane: expected error, got nil")
	}
}

func TestCountBinsInLaneNoBins(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	lane := &nodes.Node{Name: "LN", Enabled: true}
	if err := nodes.Create(sdb, lane); err != nil {
		t.Fatalf("nodes.Create lane: %v", err)
	}
	d := 1
	slot := &nodes.Node{Name: "SL", Enabled: true, ParentID: &lane.ID, Depth: &d}
	if err := nodes.Create(sdb, slot); err != nil {
		t.Fatalf("nodes.Create slot: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N1", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}

	// Unset key returns empty string.
	if v := nodes.GetProperty(sdb, n.ID, "role"); v != "" {
		t.Errorf("nodes.GetProperty unset = %q, want \"\"", v)
	}

	// Set, then read back.
	if err := nodes.SetProperty(sdb, n.ID, "role", "store"); err != nil {
		t.Fatalf("nodes.SetProperty: %v", err)
	}
	if v := nodes.GetProperty(sdb, n.ID, "role"); v != "store" {
		t.Errorf("nodes.GetProperty after set = %q, want %q", v, "store")
	}

	// Upsert: same key re-set updates the value.
	if err := nodes.SetProperty(sdb, n.ID, "role", "retrieve"); err != nil {
		t.Fatalf("nodes.SetProperty upsert: %v", err)
	}
	if v := nodes.GetProperty(sdb, n.ID, "role"); v != "retrieve" {
		t.Errorf("nodes.GetProperty after upsert = %q, want %q", v, "retrieve")
	}

	// Add a second property.
	if err := nodes.SetProperty(sdb, n.ID, "capacity", "42"); err != nil {
		t.Fatalf("nodes.SetProperty capacity: %v", err)
	}

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
	if err := nodes.DeleteProperty(sdb, n.ID, "role"); err != nil {
		t.Fatalf("nodes.DeleteProperty: %v", err)
	}
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
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "EMPTY", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}
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
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N-STAT", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}

	if err := nodes.AssignStation(sdb, n.ID, "line-1"); err != nil {
		t.Fatalf("nodes.AssignStation line-1: %v", err)
	}
	// ON CONFLICT DO NOTHING: assigning same station again is a no-op (no error).
	if err := nodes.AssignStation(sdb, n.ID, "line-1"); err != nil {
		t.Fatalf("nodes.AssignStation line-1 dup: %v", err)
	}
	if err := nodes.AssignStation(sdb, n.ID, "line-2"); err != nil {
		t.Fatalf("nodes.AssignStation line-2: %v", err)
	}

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

	if err := nodes.UnassignStation(sdb, n.ID, "line-1"); err != nil {
		t.Fatalf("nodes.UnassignStation: %v", err)
	}
	list2, err := nodes.ListStationsForNode(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListStationsForNode after unassign: %v", err)
	}
	if len(list2) != 1 || list2[0] != "line-2" {
		t.Errorf("nodes.ListStationsForNode after unassign = %v, want [line-2]", list2)
	}
}

func TestSetStationsReplacesAll(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N-SET", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}

	if err := nodes.AssignStation(sdb, n.ID, "alpha"); err != nil {
		t.Fatalf("nodes.AssignStation alpha: %v", err)
	}
	if err := nodes.AssignStation(sdb, n.ID, "beta"); err != nil {
		t.Fatalf("nodes.AssignStation beta: %v", err)
	}

	// nodes.SetStations replaces the full set.
	if err := nodes.SetStations(sdb, n.ID, []string{"gamma", "delta"}); err != nil {
		t.Fatalf("nodes.SetStations: %v", err)
	}
	list, err := nodes.ListStationsForNode(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListStationsForNode: %v", err)
	}
	if len(list) != 2 || list[0] != "delta" || list[1] != "gamma" {
		t.Errorf("nodes.ListStationsForNode = %v, want [delta gamma]", list)
	}

	// nodes.SetStations with empty slice clears everything.
	if err := nodes.SetStations(sdb, n.ID, nil); err != nil {
		t.Fatalf("nodes.SetStations nil: %v", err)
	}
	list2, err := nodes.ListStationsForNode(sdb, n.ID)
	if err != nil {
		t.Fatalf("nodes.ListStationsForNode empty: %v", err)
	}
	if len(list2) != 0 {
		t.Errorf("nodes.ListStationsForNode after nodes.SetStations(nil) = %v, want []", list2)
	}
}

func TestListNodesForStation(t *testing.T) {
	db := testdb.Open(t)
	sdb := db.DB

	// Look up seeded NGRP type (ListNodesForStation queries by code='NGRP').
	ngrp, err := nodes.GetTypeByCode(sdb, "NGRP")
	if err != nil {
		t.Fatalf("lookup NGRP type: %v", err)
	}

	// Directly assigned top-level node.
	direct := &nodes.Node{Name: "DIRECT", Enabled: true}
	if err := nodes.Create(sdb, direct); err != nil {
		t.Fatalf("nodes.Create direct: %v", err)
	}
	if err := nodes.AssignStation(sdb, direct.ID, "line-1"); err != nil {
		t.Fatalf("nodes.AssignStation direct: %v", err)
	}

	// Group + child under it: station assigned to the child; group should
	// surface too because of the NGRP parent rollup clause.
	grp := &nodes.Node{Name: "GRP", Enabled: true, IsSynthetic: true, NodeTypeID: &ngrp.ID}
	if err := nodes.Create(sdb, grp); err != nil {
		t.Fatalf("nodes.Create grp: %v", err)
	}
	kid := &nodes.Node{Name: "KID", Enabled: true, ParentID: &grp.ID}
	if err := nodes.Create(sdb, kid); err != nil {
		t.Fatalf("nodes.Create kid: %v", err)
	}
	if err := nodes.AssignStation(sdb, kid.ID, "line-1"); err != nil {
		t.Fatalf("nodes.AssignStation kid: %v", err)
	}

	// Unrelated node — must not appear.
	if err := nodes.Create(sdb, &nodes.Node{Name: "UNREL", Enabled: true}); err != nil {
		t.Fatalf("nodes.Create unrel: %v", err)
	}

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
	db := testdb.Open(t)
	sdb := db.DB

	parent := &nodes.Node{Name: "P", Enabled: true}
	if err := nodes.Create(sdb, parent); err != nil {
		t.Fatalf("nodes.Create parent: %v", err)
	}
	if err := nodes.AssignStation(sdb, parent.ID, "parent-line"); err != nil {
		t.Fatalf("nodes.AssignStation parent: %v", err)
	}

	child := &nodes.Node{Name: "C", Enabled: true, ParentID: &parent.ID}
	if err := nodes.Create(sdb, child); err != nil {
		t.Fatalf("nodes.Create child: %v", err)
	}

	// Mode "all" -> returns nil.
	if err := nodes.SetProperty(sdb, child.ID, "station_mode", "all"); err != nil {
		t.Fatalf("nodes.SetProperty all: %v", err)
	}
	got, err := nodes.GetEffectiveStations(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.GetEffectiveStations all: %v", err)
	}
	if got != nil {
		t.Errorf("nodes.GetEffectiveStations(all) = %v, want nil", got)
	}

	// Mode "none" -> empty non-nil slice.
	if err := nodes.SetProperty(sdb, child.ID, "station_mode", "none"); err != nil {
		t.Fatalf("nodes.SetProperty none: %v", err)
	}
	got, err = nodes.GetEffectiveStations(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.GetEffectiveStations none: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("nodes.GetEffectiveStations(none) = %v, want empty non-nil slice", got)
	}

	// Mode "specific" -> direct assignments only (seed a child assignment).
	if err := nodes.SetProperty(sdb, child.ID, "station_mode", "specific"); err != nil {
		t.Fatalf("nodes.SetProperty specific: %v", err)
	}
	if err := nodes.AssignStation(sdb, child.ID, "child-line"); err != nil {
		t.Fatalf("nodes.AssignStation child: %v", err)
	}
	got, err = nodes.GetEffectiveStations(sdb, child.ID)
	if err != nil {
		t.Fatalf("nodes.GetEffectiveStations specific: %v", err)
	}
	if len(got) != 1 || got[0] != "child-line" {
		t.Errorf("nodes.GetEffectiveStations(specific) = %v, want [child-line]", got)
	}

	// Mode inherit: delete the child's own assignment, then inherit walks
	// up to parent-line.
	if err := nodes.UnassignStation(sdb, child.ID, "child-line"); err != nil {
		t.Fatalf("nodes.UnassignStation child: %v", err)
	}
	if err := nodes.DeleteProperty(sdb, child.ID, "station_mode"); err != nil {
		t.Fatalf("nodes.DeleteProperty station_mode: %v", err)
	}
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
	db := testdb.Open(t)
	sdb := db.DB

	n := &nodes.Node{Name: "N-PL", Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("nodes.Create: %v", err)
	}

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
	if err := nodes.AssignPayload(sdb, n.ID, pl1ID); err != nil {
		t.Fatalf("nodes.AssignPayload pl1: %v", err)
	}
	// Duplicate assign — ON CONFLICT DO NOTHING, no error.
	if err := nodes.AssignPayload(sdb, n.ID, pl1ID); err != nil {
		t.Fatalf("nodes.AssignPayload pl1 dup: %v", err)
	}
	if err := nodes.AssignPayload(sdb, n.ID, pl2ID); err != nil {
		t.Fatalf("nodes.AssignPayload pl2: %v", err)
	}

	if got := countNodePayloads(t, sdb, n.ID); got != 2 {
		t.Errorf("node_payloads count after assigns = %d, want 2", got)
	}

	// nodes.UnassignPayload.
	if err := nodes.UnassignPayload(sdb, n.ID, pl1ID); err != nil {
		t.Fatalf("nodes.UnassignPayload: %v", err)
	}
	if got := countNodePayloads(t, sdb, n.ID); got != 1 {
		t.Errorf("node_payloads count after unassign = %d, want 1", got)
	}

	// nodes.SetPayloads replaces the set entirely.
	if err := nodes.SetPayloads(sdb, n.ID, []int64{pl1ID, pl2ID}); err != nil {
		t.Fatalf("nodes.SetPayloads: %v", err)
	}
	if got := countNodePayloads(t, sdb, n.ID); got != 2 {
		t.Errorf("node_payloads count after nodes.SetPayloads = %d, want 2", got)
	}

	// nodes.SetPayloads nil clears.
	if err := nodes.SetPayloads(sdb, n.ID, nil); err != nil {
		t.Fatalf("nodes.SetPayloads nil: %v", err)
	}
	if got := countNodePayloads(t, sdb, n.ID); got != 0 {
		t.Errorf("node_payloads count after nodes.SetPayloads(nil) = %d, want 0", got)
	}
}
