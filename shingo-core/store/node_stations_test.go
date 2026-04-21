//go:build docker

package store

import "testing"

func TestNodeStation_AssignUnassignList(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "STA-NODE-1", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	if err := db.AssignNodeToStation(node.ID, "line-1"); err != nil {
		t.Fatalf("AssignNodeToStation line-1: %v", err)
	}
	if err := db.AssignNodeToStation(node.ID, "line-2"); err != nil {
		t.Fatalf("AssignNodeToStation line-2: %v", err)
	}

	stations, err := db.ListStationsForNode(node.ID)
	if err != nil {
		t.Fatalf("ListStationsForNode: %v", err)
	}
	if len(stations) != 2 {
		t.Fatalf("stations len = %d, want 2", len(stations))
	}
	// ordered by station_id
	if stations[0] != "line-1" || stations[1] != "line-2" {
		t.Errorf("stations = %+v, want [line-1 line-2]", stations)
	}

	// Double-assign should be idempotent (ON CONFLICT DO NOTHING)
	if err := db.AssignNodeToStation(node.ID, "line-1"); err != nil {
		t.Fatalf("AssignNodeToStation duplicate: %v", err)
	}
	dup, _ := db.ListStationsForNode(node.ID)
	if len(dup) != 2 {
		t.Errorf("stations len after dup-assign = %d, want 2", len(dup))
	}

	// Unassign
	if err := db.UnassignNodeFromStation(node.ID, "line-1"); err != nil {
		t.Fatalf("UnassignNodeFromStation: %v", err)
	}
	after, _ := db.ListStationsForNode(node.ID)
	if len(after) != 1 {
		t.Fatalf("stations after unassign len = %d, want 1", len(after))
	}
	if after[0] != "line-2" {
		t.Errorf("after[0] = %q, want %q", after[0], "line-2")
	}
}

func TestListNodesForStation(t *testing.T) {
	db := testDB(t)

	nodeA := &Node{Name: "STA-LIST-A", Enabled: true}
	db.CreateNode(nodeA)
	nodeB := &Node{Name: "STA-LIST-B", Enabled: true}
	db.CreateNode(nodeB)
	nodeC := &Node{Name: "STA-LIST-C", Enabled: true}
	db.CreateNode(nodeC)

	db.AssignNodeToStation(nodeA.ID, "line-X")
	db.AssignNodeToStation(nodeB.ID, "line-X")
	db.AssignNodeToStation(nodeC.ID, "line-Y")

	gotX, err := db.ListNodesForStation("line-X")
	if err != nil {
		t.Fatalf("ListNodesForStation X: %v", err)
	}
	if len(gotX) != 2 {
		t.Fatalf("line-X nodes len = %d, want 2", len(gotX))
	}
	names := map[string]bool{}
	for _, n := range gotX {
		names[n.Name] = true
	}
	if !names["STA-LIST-A"] || !names["STA-LIST-B"] {
		t.Errorf("line-X names = %+v, want A and B", names)
	}

	gotY, _ := db.ListNodesForStation("line-Y")
	if len(gotY) != 1 {
		t.Fatalf("line-Y nodes len = %d, want 1", len(gotY))
	}
	if gotY[0].Name != "STA-LIST-C" {
		t.Errorf("line-Y[0] name = %q, want STA-LIST-C", gotY[0].Name)
	}
}

func TestSetNodeStations_Replaces(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "STA-SET-1", Enabled: true}
	db.CreateNode(node)

	if err := db.SetNodeStations(node.ID, []string{"line-1", "line-2"}); err != nil {
		t.Fatalf("SetNodeStations [1,2]: %v", err)
	}
	first, _ := db.ListStationsForNode(node.ID)
	if len(first) != 2 {
		t.Fatalf("after [1,2] len = %d, want 2", len(first))
	}

	// Replace with [3]
	if err := db.SetNodeStations(node.ID, []string{"line-3"}); err != nil {
		t.Fatalf("SetNodeStations [3]: %v", err)
	}
	second, _ := db.ListStationsForNode(node.ID)
	if len(second) != 1 {
		t.Fatalf("after [3] len = %d, want 1", len(second))
	}
	if second[0] != "line-3" {
		t.Errorf("after [3] = %q, want line-3", second[0])
	}

	// Clear
	if err := db.SetNodeStations(node.ID, nil); err != nil {
		t.Fatalf("SetNodeStations nil: %v", err)
	}
	empty, _ := db.ListStationsForNode(node.ID)
	if len(empty) != 0 {
		t.Errorf("after clear len = %d, want 0", len(empty))
	}
}

func TestGetEffectiveStations_Modes(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "STA-EFF-1", Enabled: true}
	db.CreateNode(node)

	db.SetNodeStations(node.ID, []string{"line-A"})

	// "specific"
	db.SetNodeProperty(node.ID, "station_mode", "specific")
	spec, err := db.GetEffectiveStations(node.ID)
	if err != nil {
		t.Fatalf("GetEffectiveStations specific: %v", err)
	}
	if len(spec) != 1 || spec[0] != "line-A" {
		t.Errorf("specific = %+v, want [line-A]", spec)
	}

	// "all" -> nil
	db.SetNodeProperty(node.ID, "station_mode", "all")
	allRes, err := db.GetEffectiveStations(node.ID)
	if err != nil {
		t.Fatalf("GetEffectiveStations all: %v", err)
	}
	if allRes != nil {
		t.Errorf("all mode should return nil, got %+v", allRes)
	}

	// "none" -> empty slice (but not nil)
	db.SetNodeProperty(node.ID, "station_mode", "none")
	noneRes, err := db.GetEffectiveStations(node.ID)
	if err != nil {
		t.Fatalf("GetEffectiveStations none: %v", err)
	}
	if len(noneRes) != 0 {
		t.Errorf("none mode should be empty, got %+v", noneRes)
	}

	// "inherit" with direct assignments -> returns its own
	db.SetNodeProperty(node.ID, "station_mode", "inherit")
	inh, err := db.GetEffectiveStations(node.ID)
	if err != nil {
		t.Fatalf("GetEffectiveStations inherit: %v", err)
	}
	if len(inh) != 1 || inh[0] != "line-A" {
		t.Errorf("inherit (self) = %+v, want [line-A]", inh)
	}
}
