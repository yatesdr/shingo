package www

import (
	"sort"
	"strings"
	"testing"

	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/registry"
	"shingocore/store/scene"
)

// stubNodesPageDataStore is a canned in-memory implementation of
// nodesPageDataStore used to unit-test getNodesPageData without a DB or
// the docker build tag. It has no per-node read to offer: the page data is
// built from list reads and the engine's robot snapshot, nothing else.
type stubNodesPageDataStore struct {
	nodes       []*nodes.Node
	counts      map[int64]int
	tileStates  map[int64]bins.NodeTileState
	scenePoints []*scene.Point
	binTypes    []*bins.BinType
	edges       []registry.Edge
	robots      []fleet.RobotStatus
}

func (s *stubNodesPageDataStore) ListNodes() ([]*nodes.Node, error) { return s.nodes, nil }
func (s *stubNodesPageDataStore) CountBinsByAllNodes() (map[int64]int, error) {
	return s.counts, nil
}
func (s *stubNodesPageDataStore) NodeTileStates() (map[int64]bins.NodeTileState, error) {
	return s.tileStates, nil
}
func (s *stubNodesPageDataStore) ListScenePoints() ([]*scene.Point, error) {
	return s.scenePoints, nil
}
func (s *stubNodesPageDataStore) ListBinTypes() ([]*bins.BinType, error) { return s.binTypes, nil }
func (s *stubNodesPageDataStore) ListEdges() ([]registry.Edge, error) {
	return s.edges, nil
}
func (s *stubNodesPageDataStore) CachedRobots() []fleet.RobotStatus { return s.robots }

// TestGetNodesPageData_ComposesOutput drives getNodesPageData with a
// canned store and asserts on the composition: uniqued zones, child
// counts driven by ParentID, and depths read off the node rows.
func TestGetNodesPageData_ComposesOutput(t *testing.T) {
	t.Parallel()
	parentID := int64(1)
	otherParentID := int64(99) // parent that does not exist in the node list
	depthOne := 1
	stub := &stubNodesPageDataStore{
		nodes: []*nodes.Node{
			{ID: 1, Name: "root-a", Zone: "zone-A"},
			{ID: 2, Name: "child-a1", Zone: "zone-A", ParentID: &parentID, Depth: &depthOne},
			{ID: 3, Name: "child-a2", Zone: "zone-B", ParentID: &parentID},
			{ID: 4, Name: "orphan", Zone: "", ParentID: &otherParentID},
			{ID: 5, Name: "solo", Zone: "zone-B"},
		},
		counts:     map[int64]int{1: 2, 3: 7},
		tileStates: map[int64]bins.NodeTileState{2: {}},
		binTypes: []*bins.BinType{
			{ID: 10, Code: "BT-A"},
			{ID: 11, Code: "BT-B"},
		},
		edges: []registry.Edge{{StationID: "edge-a"}},
	}

	pd, err := getNodesPageData(stub)
	if err != nil {
		t.Fatalf("getNodesPageData returned error: %v", err)
	}
	if pd == nil {
		t.Fatal("getNodesPageData returned nil pd")
	}

	// 1. Nodes and BinTypes flow through verbatim.
	if len(pd.Nodes) != 5 {
		t.Errorf("len(pd.Nodes) = %d, want 5", len(pd.Nodes))
	}
	if len(pd.BinTypes) != 2 {
		t.Errorf("len(pd.BinTypes) = %d, want 2", len(pd.BinTypes))
	}

	// 2. Zones are uniqued from the non-empty Zone fields of the canned nodes.
	gotZones := append([]string(nil), pd.Zones...)
	sort.Strings(gotZones)
	wantZones := []string{"zone-A", "zone-B"}
	if len(gotZones) != len(wantZones) {
		t.Fatalf("Zones = %v, want %v", gotZones, wantZones)
	}
	for i := range wantZones {
		if gotZones[i] != wantZones[i] {
			t.Errorf("Zones[%d] = %q, want %q", i, gotZones[i], wantZones[i])
		}
	}

	// 3. ChildCounts is populated for nodes with parents, keyed by parent ID.
	//    Nodes 2 and 3 both have parentID=1 (two children). Node 4 has
	//    parentID=99 (one child under a synthetic parent).
	if got := pd.ChildCounts[parentID]; got != 2 {
		t.Errorf("ChildCounts[%d] = %d, want 2", parentID, got)
	}
	if got := pd.ChildCounts[otherParentID]; got != 1 {
		t.Errorf("ChildCounts[%d] = %d, want 1", otherParentID, got)
	}
	// Nodes without children should not appear as keys.
	if _, ok := pd.ChildCounts[5]; ok {
		t.Errorf("ChildCounts[5] unexpectedly present (node 5 has no children)")
	}

	// 4. Depths comes off the node row for every child: node 2 carries depth 1;
	//    nodes 3 and 4 carry NULL, which reads as 0 (GetSlotDepth's reading of
	//    NULL, and what the template's index of an absent key rendered). It
	//    used to be one GetSlotDepth query per child node for the same column.
	if got, ok := pd.Depths[2]; !ok || got != 1 {
		t.Errorf("Depths[2] = (%d, ok=%v), want (1, true)", got, ok)
	}
	for _, id := range []int64{3, 4} {
		if got, ok := pd.Depths[id]; !ok || got != 0 {
			t.Errorf("Depths[%d] = (%d, ok=%v), want (0, true): a NULL depth reads as 0", id, got, ok)
		}
	}
	if _, ok := pd.Depths[5]; ok {
		t.Errorf("Depths[5] unexpectedly present (node 5 has no parent)")
	}

	// 5. Counts flows through verbatim.
	if pd.Counts[1] != 2 || pd.Counts[3] != 7 {
		t.Errorf("Counts = %v, want {1:2, 3:7}", pd.Counts)
	}

	// 6. TileStates is filled in with zero values for nodes that weren't
	//    present in the canned map.
	for _, n := range stub.nodes {
		if _, ok := pd.TileStates[n.ID]; !ok {
			t.Errorf("TileStates missing entry for node %d", n.ID)
		}
	}
}

// TestGetNodesPageData_KeepsTheStoresOrder pins that the page data hands the
// nodes over in the order the store read them (ListNodes is ORDER BY name).
// The lanes-out-of-order defect (Lane_01, 02, 03, 15, 16, 04 …) is therefore
// not a server ordering: buildHierarchy in nodes-supermarket.js re-collects the
// tiles through Object.keys of an id-keyed map, which is id order.
func TestGetNodesPageData_KeepsTheStoresOrder(t *testing.T) {
	t.Parallel()
	grp := int64(1)
	stub := &stubNodesPageDataStore{
		nodes: []*nodes.Node{
			{ID: 1, Name: "GRP", IsSynthetic: true, NodeTypeCode: "NGRP"},
			{ID: 40, Name: "Lane_01", ParentID: &grp, NodeTypeCode: "LANE"},
			{ID: 41, Name: "Lane_02", ParentID: &grp, NodeTypeCode: "LANE"},
			{ID: 9, Name: "Lane_15", ParentID: &grp, NodeTypeCode: "LANE"},
			{ID: 42, Name: "Lane_16", ParentID: &grp, NodeTypeCode: "LANE"},
		},
	}
	pd, err := getNodesPageData(stub)
	if err != nil {
		t.Fatalf("getNodesPageData: %v", err)
	}
	var got []string
	for _, n := range pd.Nodes {
		got = append(got, n.Name)
	}
	want := []string{"GRP", "Lane_01", "Lane_02", "Lane_15", "Lane_16"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("page node order = %v, want the store's %v", got, want)
	}
}

// TestGetNodesPageData_TransitNode pins whether the synthetic _TRANSIT node
// reaches the page. It is bookkeeping (where a bin sits while it rides a
// robot), not a place on the floor.
func TestGetNodesPageData_TransitNode(t *testing.T) {
	t.Parallel()
	stub := &stubNodesPageDataStore{
		nodes: []*nodes.Node{
			{ID: 1, Name: "SLOT-A"},
			{ID: 2, Name: domain.TransitNodeName, IsSynthetic: true},
			{ID: 3, Name: "SLOT-B"},
		},
	}
	pd, err := getNodesPageData(stub)
	if err != nil {
		t.Fatalf("getNodesPageData: %v", err)
	}
	present := false
	for _, n := range pd.Nodes {
		if n.Name == domain.TransitNodeName {
			present = true
		}
	}
	if present || len(pd.Nodes) != 2 {
		t.Errorf("_TRANSIT present=%v, %d nodes; want absent, 2 nodes", present, len(pd.Nodes))
	}
	if _, ok := pd.TileStates[2]; ok {
		t.Error("_TRANSIT must not carry a tile state either")
	}
}

// TestGetNodesPageData_RobotAtNode pins which robots a node tile names: a
// connected robot whose CurrentStation is the node's own name (the
// simulator's case) or a scene point that aliases exactly one node (the
// plant's case). Not a robot between points, not a disconnected one, not an
// ambiguous alias, and never on a synthetic node.
func TestGetNodesPageData_RobotAtNode(t *testing.T) {
	t.Parallel()
	grp := int64(1)
	stub := &stubNodesPageDataStore{
		nodes: []*nodes.Node{
			{ID: 1, Name: "GRP", IsSynthetic: true},
			{ID: 2, Name: "UTN_013", ParentID: &grp},
			{ID: 3, Name: "SLOT-B", Enabled: false},
			{ID: 4, Name: "SLOT-C"},
			{ID: 5, Name: "SLOT-D"},
		},
		scenePoints: []*scene.Point{
			{ClassName: "GeneralLocation", InstanceName: "SLOT-B", PointName: "AP102"},
			{ClassName: "GeneralLocation", InstanceName: "SLOT-C", PointName: "AP200"},
			{ClassName: "GeneralLocation", InstanceName: "SLOT-D", PointName: "AP200"},
		},
		robots: []fleet.RobotStatus{
			{VehicleID: "AMR-17", Connected: true, CurrentStation: "UTN_013", JackState: 1},
			{VehicleID: "AMR-05", Connected: true, CurrentStation: "UTN_013", JackState: 3},
			{VehicleID: "AMR-02", Connected: true, CurrentStation: "AP102"},
			{VehicleID: "AMR-03", Connected: true, CurrentStation: "AP200"},
			{VehicleID: "AMR-04", Connected: true, CurrentStation: "", LastStation: "SLOT-C"},
			{VehicleID: "AMR-06", Connected: false, CurrentStation: "SLOT-C"},
			{VehicleID: "AMR-07", Connected: true, CurrentStation: "GRP"},
		},
	}
	pd, err := getNodesPageData(stub)
	if err != nil {
		t.Fatalf("getNodesPageData: %v", err)
	}
	want := map[int64]nodeRobots{
		2: {Label: "AMR-05 +1", Title: "At this node: AMR-05, AMR-17 (carrying a bin)"},
		3: {Label: "AMR-02", Title: "At this node: AMR-02"},
	}
	if len(pd.RobotsAt) != len(want) {
		t.Errorf("RobotsAt = %+v, want %+v", pd.RobotsAt, want)
	}
	for id, w := range want {
		if got := pd.RobotsAt[id]; got != w {
			t.Errorf("RobotsAt[%d] = %+v, want %+v", id, got, w)
		}
	}
}

// The tile draws the marker the page data built, and only where a robot is.
func TestNodesPage_TileShowsTheRobotStandingOnIt(t *testing.T) {
	t.Parallel()
	pd, err := getNodesPageData(&stubNodesPageDataStore{
		nodes: []*nodes.Node{{ID: 2, Name: "UTN_013", Enabled: true}, {ID: 3, Name: "UTN_014", Enabled: true}},
		robots: []fleet.RobotStatus{
			{VehicleID: "AMR-17", Connected: true, CurrentStation: "UTN_013", JackState: 1},
		},
	})
	if err != nil {
		t.Fatalf("getNodesPageData: %v", err)
	}
	html := renderPageWithNamer(t, "nodes.html", &fakeNamer{byUID: map[string]string{}}, map[string]any{
		"Page": "nodes", "Nodes": pd.Nodes, "Counts": pd.Counts, "TileStates": pd.TileStates,
		"Zones": pd.Zones, "NodeLabels": pd.NodeLabels, "NodeInfo": pd.NodeInfo, "MapGroups": pd.MapGroups,
		"BinTypes": pd.BinTypes, "Edges": pd.Edges, "ChildCounts": pd.ChildCounts, "Depths": pd.Depths,
		"RobotsAt": pd.RobotsAt,
	})
	want := `<span class="tile-robot" title="At this node: AMR-17 (carrying a bin)">AMR-17</span>`
	if strings.Count(html, `class="tile-robot"`) != 1 || !strings.Contains(html, want) {
		t.Errorf("want exactly one robot marker, on UTN_013: %s", want)
	}
}
