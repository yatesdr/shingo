//go:build docker

package service

import (
	"sort"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/scene"
)

// makeNode creates a fresh node and returns it.
func makeNode(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	return n
}

func TestNodeService_ApplyAssignments_SpecificStationsAndBinTypes(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	bt2 := &bins.BinType{Code: "BT2", Description: "second"}
	if err := db.CreateBinType(bt2); err != nil {
		t.Fatalf("create second bin type: %v", err)
	}

	a := NodeAssignments{
		StationMode: "specific",
		Stations:    []string{"station-a", "station-b"},
		BinTypeMode: "specific",
		BinTypeIDs:  []int64{sd.BinType.ID, bt2.ID},
	}

	if err := svc.ApplyAssignments(sd.StorageNode.ID, a); err != nil {
		t.Fatalf("ApplyAssignments: %v", err)
	}

	if got := db.GetNodeProperty(sd.StorageNode.ID, "station_mode"); got != "specific" {
		t.Errorf("station_mode = %q, want %q", got, "specific")
	}
	if got := db.GetNodeProperty(sd.StorageNode.ID, "bin_type_mode"); got != "specific" {
		t.Errorf("bin_type_mode = %q, want %q", got, "specific")
	}

	stations, err := db.ListStationsForNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("ListStationsForNode: %v", err)
	}
	sort.Strings(stations)
	want := []string{"station-a", "station-b"}
	if !equalStrings(stations, want) {
		t.Errorf("stations = %v, want %v", stations, want)
	}

	binTypes, err := db.ListBinTypesForNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("ListBinTypesForNode: %v", err)
	}
	if len(binTypes) != 2 {
		t.Errorf("len(binTypes) = %d, want 2", len(binTypes))
	}
	gotIDs := make(map[int64]bool, len(binTypes))
	for _, b := range binTypes {
		gotIDs[b.ID] = true
	}
	if !gotIDs[sd.BinType.ID] || !gotIDs[bt2.ID] {
		t.Errorf("binType IDs = %v, want both %d and %d", gotIDs, sd.BinType.ID, bt2.ID)
	}
}

func TestNodeService_ApplyAssignments_NonSpecificClearsList(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	// Pre-populate with assignments.
	if err := svc.ApplyAssignments(sd.StorageNode.ID, NodeAssignments{
		StationMode: "specific",
		Stations:    []string{"line-1"},
		BinTypeMode: "specific",
		BinTypeIDs:  []int64{sd.BinType.ID},
	}); err != nil {
		t.Fatalf("seed ApplyAssignments: %v", err)
	}

	// Switch to "all" mode — payload-side lists must be cleared even though
	// we still pass non-empty lists, because the mode is no longer "specific".
	a := NodeAssignments{
		StationMode: "all",
		Stations:    []string{"ignored"},
		BinTypeMode: "all",
		BinTypeIDs:  []int64{sd.BinType.ID},
	}
	if err := svc.ApplyAssignments(sd.StorageNode.ID, a); err != nil {
		t.Fatalf("ApplyAssignments (all): %v", err)
	}

	if mode := db.GetNodeProperty(sd.StorageNode.ID, "station_mode"); mode != "all" {
		t.Errorf("station_mode = %q, want %q", mode, "all")
	}
	if mode := db.GetNodeProperty(sd.StorageNode.ID, "bin_type_mode"); mode != "all" {
		t.Errorf("bin_type_mode = %q, want %q", mode, "all")
	}

	stations, _ := db.ListStationsForNode(sd.StorageNode.ID)
	if len(stations) != 0 {
		t.Errorf("stations = %v, want empty when mode != specific", stations)
	}
	binTypes, _ := db.ListBinTypesForNode(sd.StorageNode.ID)
	if len(binTypes) != 0 {
		t.Errorf("binTypes = %v, want empty when mode != specific", binTypes)
	}
}

func TestNodeService_ApplyAssignments_InheritMode(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "NODE-INHERIT")

	a := NodeAssignments{
		StationMode: "inherit",
		BinTypeMode: "inherit",
	}
	if err := svc.ApplyAssignments(n.ID, a); err != nil {
		t.Fatalf("ApplyAssignments: %v", err)
	}
	if mode := db.GetNodeProperty(n.ID, "station_mode"); mode != "inherit" {
		t.Errorf("station_mode = %q, want %q", mode, "inherit")
	}
	if mode := db.GetNodeProperty(n.ID, "bin_type_mode"); mode != "inherit" {
		t.Errorf("bin_type_mode = %q, want %q", mode, "inherit")
	}

	stations, _ := db.ListStationsForNode(n.ID)
	if len(stations) != 0 {
		t.Errorf("stations = %v, want empty for inherit mode", stations)
	}
	binTypes, _ := db.ListBinTypesForNode(n.ID)
	if len(binTypes) != 0 {
		t.Errorf("binTypes = %v, want empty for inherit mode", binTypes)
	}
}

func TestNodeService_ApplyAssignments_EmptyModesAreWritten(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "NODE-EMPTY")

	// Empty mode value still writes the property (mirrors the comment in
	// the source: "The mode is always written").
	if err := svc.ApplyAssignments(n.ID, NodeAssignments{}); err != nil {
		t.Fatalf("ApplyAssignments: %v", err)
	}
	props, err := db.ListNodeProperties(n.ID)
	if err != nil {
		t.Fatalf("ListNodeProperties: %v", err)
	}
	gotKeys := map[string]string{}
	for _, p := range props {
		gotKeys[p.Key] = p.Value
	}
	if v, ok := gotKeys["station_mode"]; !ok || v != "" {
		t.Errorf("station_mode prop = (%q, present=%v), want present with empty value", v, ok)
	}
	if v, ok := gotKeys["bin_type_mode"]; !ok || v != "" {
		t.Errorf("bin_type_mode prop = (%q, present=%v), want present with empty value", v, ok)
	}
}

func TestNodeService_ApplyAssignments_SwitchFromSpecificToInheritClearsList(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	// First seed with specific assignments.
	if err := svc.ApplyAssignments(sd.LineNode.ID, NodeAssignments{
		StationMode: "specific",
		Stations:    []string{"s1", "s2"},
		BinTypeMode: "specific",
		BinTypeIDs:  []int64{sd.BinType.ID},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stations, _ := db.ListStationsForNode(sd.LineNode.ID)
	if len(stations) != 2 {
		t.Fatalf("seed verify: expected 2 stations, got %d", len(stations))
	}

	// Now switch to inherit — the previous selections must be wiped.
	if err := svc.ApplyAssignments(sd.LineNode.ID, NodeAssignments{
		StationMode: "inherit",
		BinTypeMode: "inherit",
	}); err != nil {
		t.Fatalf("re-apply inherit: %v", err)
	}
	stations, _ = db.ListStationsForNode(sd.LineNode.ID)
	if len(stations) != 0 {
		t.Errorf("stations = %v, want empty after switching to inherit", stations)
	}
	binTypes, _ := db.ListBinTypesForNode(sd.LineNode.ID)
	if len(binTypes) != 0 {
		t.Errorf("binTypes = %v, want empty after switching to inherit", binTypes)
	}
}

func TestNodeService_ApplyAssignments_SpecificWithEmptyLists(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "NODE-SPEC-EMPTY")

	// "specific" with nil lists: SetNodeStations(nil) and SetNodeBinTypes(nil)
	// are still called (the explicit-list branch), but the resulting list is empty.
	a := NodeAssignments{
		StationMode: "specific",
		BinTypeMode: "specific",
	}
	if err := svc.ApplyAssignments(n.ID, a); err != nil {
		t.Fatalf("ApplyAssignments: %v", err)
	}
	if got := db.GetNodeProperty(n.ID, "station_mode"); got != "specific" {
		t.Errorf("station_mode = %q, want %q", got, "specific")
	}
	stations, _ := db.ListStationsForNode(n.ID)
	if len(stations) != 0 {
		t.Errorf("stations = %v, want empty", stations)
	}
	binTypes, _ := db.ListBinTypesForNode(n.ID)
	if len(binTypes) != 0 {
		t.Errorf("binTypes = %v, want empty", binTypes)
	}
}

func TestNodeService_ApplyAssignments_ReplacesExistingStations(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "NODE-REPLACE")

	if err := svc.ApplyAssignments(n.ID, NodeAssignments{
		StationMode: "specific",
		Stations:    []string{"s1", "s2", "s3"},
		BinTypeMode: "all",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Replace with a smaller set of different stations.
	if err := svc.ApplyAssignments(n.ID, NodeAssignments{
		StationMode: "specific",
		Stations:    []string{"s9"},
		BinTypeMode: "all",
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	stations, _ := db.ListStationsForNode(n.ID)
	sort.Strings(stations)
	if !equalStrings(stations, []string{"s9"}) {
		t.Errorf("stations = %v, want [s9]", stations)
	}
}

func TestNodeService_CreateNodeGroup(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	id, err := svc.CreateNodeGroup("NGRP-CREATE")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	if id == 0 {
		t.Fatal("CreateNodeGroup returned id=0")
	}
	got, err := db.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode(%d): %v", id, err)
	}
	if got.Name != "NGRP-CREATE" {
		t.Errorf("name = %q, want %q", got.Name, "NGRP-CREATE")
	}
	if got.NodeTypeCode != "NGRP" {
		t.Errorf("NodeTypeCode = %q, want %q", got.NodeTypeCode, "NGRP")
	}
}

func TestNodeService_AddLane(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, err := svc.CreateNodeGroup("NGRP-ADDLANE")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	laneID, err := svc.AddLane(grpID, "NGRP-ADDLANE-L1")
	if err != nil {
		t.Fatalf("AddLane: %v", err)
	}
	if laneID == 0 {
		t.Fatal("AddLane returned id=0")
	}
	lane, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("GetNode(%d): %v", laneID, err)
	}
	if lane.NodeTypeCode != "LANE" {
		t.Errorf("lane NodeTypeCode = %q, want %q", lane.NodeTypeCode, "LANE")
	}
	if lane.ParentID == nil || *lane.ParentID != grpID {
		t.Errorf("lane ParentID = %v, want %d", lane.ParentID, grpID)
	}
}

func TestNodeService_DeleteNodeGroup(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, err := svc.CreateNodeGroup("NGRP-DELETE")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	if _, err := svc.AddLane(grpID, "NGRP-DELETE-L1"); err != nil {
		t.Fatalf("AddLane: %v", err)
	}

	if err := svc.DeleteNodeGroup(grpID); err != nil {
		t.Fatalf("DeleteNodeGroup: %v", err)
	}
	if _, err := db.GetNode(grpID); err == nil {
		t.Errorf("group still exists after DeleteNodeGroup")
	}
}

func TestNodeService_GetGroupLayout(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, err := svc.CreateNodeGroup("NGRP-LAYOUT")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	laneID, err := svc.AddLane(grpID, "NGRP-LAYOUT-L1")
	if err != nil {
		t.Fatalf("AddLane: %v", err)
	}
	// Create two slots and attach to the lane via reparent.
	var slotIDs []int64
	for i := 1; i <= 2; i++ {
		slot := &nodes.Node{Name: "NGRP-LAYOUT-S" + string(rune('0'+i)), Enabled: true}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", i, err)
		}
		if err := db.ReparentNode(slot.ID, &laneID, i); err != nil {
			t.Fatalf("reparent slot %d: %v", i, err)
		}
		slotIDs = append(slotIDs, slot.ID)
	}

	layout, err := svc.GetGroupLayout(grpID)
	if err != nil {
		t.Fatalf("GetGroupLayout: %v", err)
	}
	if layout == nil {
		t.Fatal("GetGroupLayout returned nil layout")
	}
	if len(layout.Lanes) != 1 {
		t.Fatalf("len(layout.Lanes) = %d, want 1", len(layout.Lanes))
	}
	if len(layout.Lanes[0].Slots) != 2 {
		t.Errorf("len(Lanes[0].Slots) = %d, want 2", len(layout.Lanes[0].Slots))
	}
	if layout.Stats.Total != 2 {
		t.Errorf("Stats.Total = %d, want 2", layout.Stats.Total)
	}
}

func TestNodeService_ListLaneSlots(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, err := svc.CreateNodeGroup("NGRP-LISTSLOTS")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	laneID, err := svc.AddLane(grpID, "NGRP-LISTSLOTS-L1")
	if err != nil {
		t.Fatalf("AddLane: %v", err)
	}
	for i := 1; i <= 3; i++ {
		slot := &nodes.Node{Name: "SLOT-LIST-" + string(rune('0'+i)), Enabled: true}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", i, err)
		}
		if err := db.ReparentNode(slot.ID, &laneID, i); err != nil {
			t.Fatalf("reparent slot %d: %v", i, err)
		}
	}

	got, err := svc.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("ListLaneSlots: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len(got) = %d, want 3", len(got))
	}
}

func TestNodeService_ReorderLaneSlots(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, err := svc.CreateNodeGroup("NGRP-REORDER")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	laneID, err := svc.AddLane(grpID, "NGRP-REORDER-L1")
	if err != nil {
		t.Fatalf("AddLane: %v", err)
	}
	var slotIDs []int64
	for i := 1; i <= 3; i++ {
		slot := &nodes.Node{Name: "SLOT-REORDER-" + string(rune('0'+i)), Enabled: true}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", i, err)
		}
		if err := db.ReparentNode(slot.ID, &laneID, i); err != nil {
			t.Fatalf("reparent slot %d: %v", i, err)
		}
		slotIDs = append(slotIDs, slot.ID)
	}

	// Reverse the order.
	reversed := []int64{slotIDs[2], slotIDs[1], slotIDs[0]}
	if err := svc.ReorderLaneSlots(laneID, reversed); err != nil {
		t.Fatalf("ReorderLaneSlots: %v", err)
	}

	got, err := svc.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("ListLaneSlots: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for i, want := range reversed {
		if got[i].ID != want {
			t.Errorf("slot[%d].ID = %d, want %d", i, got[i].ID, want)
		}
	}
}

func TestNodeService_SetNodePayloads(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	// Create a second payload to verify replacement semantics.
	p2 := &payloads.Payload{Code: "PART-B", Description: "Second payload"}
	if err := db.CreatePayload(p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}

	if err := svc.SetNodePayloads(sd.StorageNode.ID, []int64{sd.Payload.ID, p2.ID}); err != nil {
		t.Fatalf("SetNodePayloads: %v", err)
	}
	nodes, err := db.ListNodesForPayload(sd.Payload.ID)
	if err != nil {
		t.Fatalf("ListNodesForPayload: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.ID == sd.StorageNode.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("storage node not in payload assignment list")
	}

	// Clearing with nil should wipe.
	if err := svc.SetNodePayloads(sd.StorageNode.ID, nil); err != nil {
		t.Fatalf("SetNodePayloads(nil): %v", err)
	}
	nodes, _ = db.ListNodesForPayload(sd.Payload.ID)
	for _, n := range nodes {
		if n.ID == sd.StorageNode.ID {
			t.Errorf("payload assignment not cleared")
		}
	}
}

func TestNodeService_SetNodeStations(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "NODE-SETSTATIONS")

	if err := svc.SetNodeStations(n.ID, []string{"s1", "s2"}); err != nil {
		t.Fatalf("SetNodeStations: %v", err)
	}
	got, err := db.ListStationsForNode(n.ID)
	if err != nil {
		t.Fatalf("ListStationsForNode: %v", err)
	}
	sort.Strings(got)
	if !equalStrings(got, []string{"s1", "s2"}) {
		t.Errorf("stations = %v, want [s1 s2]", got)
	}

	// Replace with a different set.
	if err := svc.SetNodeStations(n.ID, []string{"s9"}); err != nil {
		t.Fatalf("SetNodeStations (replace): %v", err)
	}
	got, _ = db.ListStationsForNode(n.ID)
	if !equalStrings(got, []string{"s9"}) {
		t.Errorf("stations after replace = %v, want [s9]", got)
	}

	// Clear with nil.
	if err := svc.SetNodeStations(n.ID, nil); err != nil {
		t.Fatalf("SetNodeStations(nil): %v", err)
	}
	got, _ = db.ListStationsForNode(n.ID)
	if len(got) != 0 {
		t.Errorf("stations after clear = %v, want empty", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── PR 3a.1b additions: tests for methods absorbed from engine_db_methods.go ──

func TestNodeService_CreateNode_AssignsIDAndReadback(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	n := &nodes.Node{Name: "CREATE-NODE-1", Zone: "zone-a", Enabled: true}
	if err := svc.CreateNode(n); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if n.ID == 0 {
		t.Fatal("CreateNode did not populate ID")
	}

	got, err := db.GetNode(n.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.Name != "CREATE-NODE-1" {
		t.Errorf("Name = %q, want %q", got.Name, "CREATE-NODE-1")
	}
	if got.Zone != "zone-a" {
		t.Errorf("Zone = %q, want %q", got.Zone, "zone-a")
	}
	if !got.Enabled {
		t.Errorf("Enabled = %v, want true", got.Enabled)
	}
}

func TestNodeService_UpdateNode_PersistsFieldChanges(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "UPDATE-NODE-1")

	n.Zone = "zone-b"
	n.Enabled = false
	if err := svc.UpdateNode(n); err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}

	got, err := db.GetNode(n.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.Zone != "zone-b" {
		t.Errorf("Zone = %q, want %q", got.Zone, "zone-b")
	}
	if got.Enabled {
		t.Errorf("Enabled = %v, want false", got.Enabled)
	}
}

func TestNodeService_DeleteNode_RemovesRow(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "DELETE-NODE-1")

	if err := svc.DeleteNode(n.ID); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if _, err := db.GetNode(n.ID); err == nil {
		t.Errorf("GetNode succeeded after DeleteNode — node still present")
	}
}

func TestNodeService_GetNode_RoundTrip(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "GETNODE-1")

	got, err := svc.GetNode(n.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.ID != n.ID || got.Name != n.Name {
		t.Errorf("got {ID:%d Name:%q}, want {ID:%d Name:%q}",
			got.ID, got.Name, n.ID, n.Name)
	}
}

func TestNodeService_ListNodes_SeesInsertedNode(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "LISTNODES-ONE")

	nodes, err := svc.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	found := false
	for _, x := range nodes {
		if x.ID == n.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inserted node %d not in ListNodes() result (len=%d)", n.ID, len(nodes))
	}
}

func TestNodeService_ListChildNodes_ReturnsOnlyChildren(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	parent := makeNode(t, db, "CHILD-PARENT-1")

	child := &nodes.Node{Name: "CHILD-1", Enabled: true, ParentID: &parent.ID}
	if err := db.CreateNode(child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	// Unrelated sibling under no parent.
	_ = makeNode(t, db, "CHILD-UNRELATED")

	got, err := svc.ListChildNodes(parent.ID)
	if err != nil {
		t.Fatalf("ListChildNodes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(children) = %d, want 1", len(got))
	}
	if got[0].ID != child.ID {
		t.Errorf("child.ID = %d, want %d", got[0].ID, child.ID)
	}
}

func TestNodeService_ListNodeStates_IncludesSeededNode(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "STATE-1")

	states, err := svc.ListNodeStates()
	if err != nil {
		t.Fatalf("ListNodeStates: %v", err)
	}
	if _, ok := states[n.ID]; !ok {
		t.Errorf("node %d missing from ListNodeStates result (keys=%d)", n.ID, len(states))
	}
}

func TestNodeService_ListBinsByNode_ReturnsAttachedBin(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	b := &bins.Bin{
		Label:     "BIN-BY-NODE-1",
		BinTypeID: sd.BinType.ID,
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	if err := db.CreateBin(b); err != nil {
		t.Fatalf("CreateBin: %v", err)
	}

	got, err := svc.ListBinsByNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("ListBinsByNode: %v", err)
	}
	found := false
	for _, x := range got {
		if x.ID == b.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("bin %d not in ListBinsByNode(%d) result (len=%d)",
			b.ID, sd.StorageNode.ID, len(got))
	}
}

func TestNodeService_ListStationsForNode_Roundtrip(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "LIST-STATIONS-1")

	if err := db.SetNodeStations(n.ID, []string{"st-a", "st-b"}); err != nil {
		t.Fatalf("SetNodeStations: %v", err)
	}
	got, err := svc.ListStationsForNode(n.ID)
	if err != nil {
		t.Fatalf("ListStationsForNode: %v", err)
	}
	sort.Strings(got)
	if !equalStrings(got, []string{"st-a", "st-b"}) {
		t.Errorf("stations = %v, want [st-a st-b]", got)
	}
}

func TestNodeService_ListBinTypesForNode_Roundtrip(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	if err := db.SetNodeBinTypes(sd.StorageNode.ID, []int64{sd.BinType.ID}); err != nil {
		t.Fatalf("SetNodeBinTypes: %v", err)
	}
	got, err := svc.ListBinTypesForNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("ListBinTypesForNode: %v", err)
	}
	if len(got) != 1 || got[0].ID != sd.BinType.ID {
		t.Errorf("bin types = %+v, want [id=%d]", got, sd.BinType.ID)
	}
}

func TestNodeService_ListNodeProperties_ReturnsAllSet(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "LIST-PROPS-1")

	if err := db.SetNodeProperty(n.ID, "role", "source"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if err := db.SetNodeProperty(n.ID, "capacity", "5"); err != nil {
		t.Fatalf("set capacity: %v", err)
	}

	props, err := svc.ListNodeProperties(n.ID)
	if err != nil {
		t.Fatalf("ListNodeProperties: %v", err)
	}
	byKey := map[string]string{}
	for _, p := range props {
		byKey[p.Key] = p.Value
	}
	if byKey["role"] != "source" {
		t.Errorf("role = %q, want %q", byKey["role"], "source")
	}
	if byKey["capacity"] != "5" {
		t.Errorf("capacity = %q, want %q", byKey["capacity"], "5")
	}
}

func TestNodeService_GetEffectiveStations_SpecificMode(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "EFF-STATIONS-1")

	if err := db.SetNodeStations(n.ID, []string{"st-x"}); err != nil {
		t.Fatalf("SetNodeStations: %v", err)
	}
	if err := db.SetNodeProperty(n.ID, "station_mode", "specific"); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	got, err := svc.GetEffectiveStations(n.ID)
	if err != nil {
		t.Fatalf("GetEffectiveStations: %v", err)
	}
	if !equalStrings(got, []string{"st-x"}) {
		t.Errorf("effective stations = %v, want [st-x]", got)
	}
}

func TestNodeService_GetEffectiveBinTypes_SpecificMode(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	if err := db.SetNodeBinTypes(sd.StorageNode.ID, []int64{sd.BinType.ID}); err != nil {
		t.Fatalf("SetNodeBinTypes: %v", err)
	}
	if err := db.SetNodeProperty(sd.StorageNode.ID, "bin_type_mode", "specific"); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	got, err := svc.GetEffectiveBinTypes(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("GetEffectiveBinTypes: %v", err)
	}
	if len(got) != 1 || got[0].ID != sd.BinType.ID {
		t.Errorf("effective bin types = %+v, want [id=%d]", got, sd.BinType.ID)
	}
}

func TestNodeService_SetAndGetNodeProperty_Roundtrip(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "PROP-RW-1")

	if err := svc.SetNodeProperty(n.ID, "direction", "forward"); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if got := svc.GetNodeProperty(n.ID, "direction"); got != "forward" {
		t.Errorf("GetNodeProperty = %q, want %q", got, "forward")
	}
	// Missing key returns empty string.
	if got := svc.GetNodeProperty(n.ID, "missing-key"); got != "" {
		t.Errorf("missing key returned %q, want empty string", got)
	}
}

func TestNodeService_DeleteNodeProperty_ClearsValue(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)
	n := makeNode(t, db, "PROP-DEL-1")

	if err := svc.SetNodeProperty(n.ID, "kill-me", "yes"); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if got := svc.GetNodeProperty(n.ID, "kill-me"); got != "yes" {
		t.Fatalf("seed verify: got %q, want %q", got, "yes")
	}

	if err := svc.DeleteNodeProperty(n.ID, "kill-me"); err != nil {
		t.Fatalf("DeleteNodeProperty: %v", err)
	}
	if got := svc.GetNodeProperty(n.ID, "kill-me"); got != "" {
		t.Errorf("after delete, GetNodeProperty = %q, want empty", got)
	}
}

func TestNodeService_SetNodeBinTypes_ReplacesAssignments(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	btB := &bins.BinType{Code: "BT-REPLACE-B", Description: "b"}
	if err := db.CreateBinType(btB); err != nil {
		t.Fatalf("create btB: %v", err)
	}

	// Start with one.
	if err := svc.SetNodeBinTypes(sd.StorageNode.ID, []int64{sd.BinType.ID}); err != nil {
		t.Fatalf("SetNodeBinTypes initial: %v", err)
	}
	// Replace with a different one.
	if err := svc.SetNodeBinTypes(sd.StorageNode.ID, []int64{btB.ID}); err != nil {
		t.Fatalf("SetNodeBinTypes replace: %v", err)
	}

	got, err := db.ListBinTypesForNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("ListBinTypesForNode: %v", err)
	}
	if len(got) != 1 || got[0].ID != btB.ID {
		t.Errorf("after replace, bin types = %+v, want [id=%d]", got, btB.ID)
	}
}

func TestNodeService_ReparentNode_MovesUnderNewParent(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	// Build a group + lane to act as the new parent.
	grpID, err := svc.CreateNodeGroup("NGRP-REPARENT-PR3A1B")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	laneID, err := svc.AddLane(grpID, "NGRP-REPARENT-PR3A1B-L1")
	if err != nil {
		t.Fatalf("AddLane: %v", err)
	}

	slot := &nodes.Node{Name: "REPARENT-SLOT-1", Enabled: true}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}

	if err := svc.ReparentNode(slot.ID, &laneID, 1); err != nil {
		t.Fatalf("ReparentNode: %v", err)
	}
	got, err := db.GetNode(slot.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.ParentID == nil || *got.ParentID != laneID {
		t.Errorf("after reparent, ParentID = %v, want %d", got.ParentID, laneID)
	}
}

// ── PR 3a.5.1 additions: tests for methods absorbed from engine_db_methods.go ──

func TestNodeService_NodeTileStates_IncludesNodeWithBin(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewNodeService(db)

	// Create a bin at the storage node so NodeTileStates has something
	// to report. The tile-state query groups by node_id of bins.
	bin := &bins.Bin{
		Label:     "TILE-STATE-1",
		BinTypeID: sd.BinType.ID,
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("CreateBin: %v", err)
	}

	states, err := svc.NodeTileStates()
	if err != nil {
		t.Fatalf("NodeTileStates: %v", err)
	}
	if _, ok := states[sd.StorageNode.ID]; !ok {
		t.Errorf("storage node %d missing from NodeTileStates (keys=%d)",
			sd.StorageNode.ID, len(states))
	}
}

func TestNodeService_ListScenePoints_RoundTrip(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	sp := &scene.Point{
		AreaName:       "AREA-SVC",
		InstanceName:   "INST-SVC",
		ClassName:      "GeneralLocation",
		PointName:      "pp",
		PropertiesJSON: `{}`,
	}
	if err := db.UpsertScenePoint(sp); err != nil {
		t.Fatalf("UpsertScenePoint: %v", err)
	}

	all, err := svc.ListScenePoints()
	if err != nil {
		t.Fatalf("ListScenePoints: %v", err)
	}
	found := false
	for _, p := range all {
		if p.InstanceName == "INST-SVC" && p.AreaName == "AREA-SVC" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inserted scene point not in ListScenePoints result (len=%d)", len(all))
	}
}

func TestNodeService_ListEdges_ReturnsRegisteredStation(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	if err := db.RegisterEdge("edge-svc-1", "host-svc", "v1", []string{"L1"}); err != nil {
		t.Fatalf("RegisterEdge: %v", err)
	}

	edges, err := svc.ListEdges()
	if err != nil {
		t.Fatalf("ListEdges: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.StationID == "edge-svc-1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("registered station edge-svc-1 not in ListEdges result (len=%d)", len(edges))
	}
}

func TestNodeService_GetSlotDepth_ReflectsSlotOrder(t *testing.T) {
	db := testDB(t)
	svc := NewNodeService(db)

	// Build NGRP > LANE > 3 slots and verify depth matches position.
	grpID, err := svc.CreateNodeGroup("NGRP-DEPTH-PR3A51")
	if err != nil {
		t.Fatalf("CreateNodeGroup: %v", err)
	}
	laneID, err := svc.AddLane(grpID, "NGRP-DEPTH-PR3A51-L1")
	if err != nil {
		t.Fatalf("AddLane: %v", err)
	}
	var slotIDs []int64
	for i := 1; i <= 3; i++ {
		slot := &nodes.Node{Name: "DEPTH-SLOT-" + string(rune('0'+i)), Enabled: true}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", i, err)
		}
		if err := db.ReparentNode(slot.ID, &laneID, i); err != nil {
			t.Fatalf("reparent slot %d: %v", i, err)
		}
		slotIDs = append(slotIDs, slot.ID)
	}

	d1, err := svc.GetSlotDepth(slotIDs[0])
	if err != nil {
		t.Fatalf("GetSlotDepth(slot1): %v", err)
	}
	d3, err := svc.GetSlotDepth(slotIDs[2])
	if err != nil {
		t.Fatalf("GetSlotDepth(slot3): %v", err)
	}
	if d1 != 1 {
		t.Errorf("slot1 depth = %d, want 1", d1)
	}
	if d3 != 3 {
		t.Errorf("slot3 depth = %d, want 3", d3)
	}
}
