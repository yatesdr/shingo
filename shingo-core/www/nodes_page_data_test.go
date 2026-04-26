package www

import (
	"sort"
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/registry"
	"shingocore/store/scene"
)

// stubNodesPageDataStore is a canned in-memory implementation of
// nodesPageDataStore used to unit-test getNodesPageData without a DB or
// the docker build tag. It records the slot-depth IDs it was asked
// about so tests can also assert the dispatch pattern.
type stubNodesPageDataStore struct {
	nodes       []*nodes.Node
	counts      map[int64]int
	tileStates  map[int64]bins.NodeTileState
	scenePoints []*scene.Point
	binTypes    []*bins.BinType
	edges       []registry.Edge
	slotDepths  map[int64]int

	// depthQueries records the IDs getNodesPageData asked for GetSlotDepth.
	depthQueries []int64
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
func (s *stubNodesPageDataStore) GetSlotDepth(nodeID int64) (int, error) {
	s.depthQueries = append(s.depthQueries, nodeID)
	d, ok := s.slotDepths[nodeID]
	if !ok {
		return 0, errNotFound{}
	}
	return d, nil
}

type errNotFound struct{}

func (errNotFound) Error() string { return "not found" }

// TestGetNodesPageData_ComposesOutput drives getNodesPageData with a
// canned store and asserts on the composition: uniqued zones, child
// counts driven by ParentID, and depths populated only for children
// whose GetSlotDepth succeeds.
func TestGetNodesPageData_ComposesOutput(t *testing.T) {
	parentID := int64(1)
	otherParentID := int64(99) // parent that does not exist in the node list
	stub := &stubNodesPageDataStore{
		nodes: []*nodes.Node{
			{ID: 1, Name: "root-a", Zone: "zone-A"},
			{ID: 2, Name: "child-a1", Zone: "zone-A", ParentID: &parentID},
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
		// Node 2 has a depth; node 3 and node 4 do not (GetSlotDepth returns error).
		slotDepths: map[int64]int{2: 1},
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

	// 4. Depths is populated only for child nodes whose GetSlotDepth returns no error.
	//    Node 2 has a depth; nodes 3 and 4 do not.
	if got, ok := pd.Depths[2]; !ok || got != 1 {
		t.Errorf("Depths[2] = (%d, ok=%v), want (1, true)", got, ok)
	}
	if _, ok := pd.Depths[3]; ok {
		t.Errorf("Depths[3] unexpectedly present (GetSlotDepth returned error)")
	}
	if _, ok := pd.Depths[4]; ok {
		t.Errorf("Depths[4] unexpectedly present (GetSlotDepth returned error)")
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

	// 7. GetSlotDepth was only called for nodes with parents (3 of 5).
	if len(stub.depthQueries) != 3 {
		t.Errorf("len(depthQueries) = %d, want 3", len(stub.depthQueries))
	}
}
