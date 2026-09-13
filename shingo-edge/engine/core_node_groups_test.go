package engine

import (
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
)

// TestSetCoreNodes_RetainsGroupMembership: Core qualifies a group child as
// "Group.Child" and SetCoreNodes strips that to the bare name the runtime
// keys on. The membership is kept beside the map — it is what the cell
// picture's dock strip lists under "Supermarket Empty Totes" — and it is
// rebuilt on every sync, so a child that left its group leaves the list.
func TestSetCoreNodes_RetainsGroupMembership(t *testing.T) {
	t.Parallel()
	e := &Engine{Events: NewEventBus()}
	if got := e.CoreNodeGroups(); len(got) != 0 {
		t.Fatalf("a fresh engine knows groups: %v", got)
	}
	e.SetCoreNodes([]protocol.NodeInfo{
		{Name: "Supermarket Empty Totes", NodeType: protocol.NodeClassNGRP},
		{Name: "Supermarket Empty Totes.SMN_05", NodeType: "SMN"},
		{Name: "Supermarket Empty Totes.SMN_06", NodeType: "SMN"},
		{Name: "Supermarket Area.SMN_01", NodeType: "SMN"},
		{Name: "PLN_01", NodeType: "PLN"},
	})
	groups := e.CoreNodeGroups()
	for _, m := range groups {
		sort.Strings(m)
	}
	want := map[string][]string{
		"Supermarket Empty Totes": {"SMN_05", "SMN_06"},
		"Supermarket Area":        {"SMN_01"},
	}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("groups = %v, want %v", groups, want)
	}
	// The bare names still key the node map, exactly as before.
	if _, ok := e.CoreNodes()["SMN_05"]; !ok {
		t.Error("SMN_05 lost its bare-name entry")
	}
	// A later sync without SMN_06 drops it from the group.
	e.SetCoreNodes([]protocol.NodeInfo{
		{Name: "Supermarket Empty Totes.SMN_05", NodeType: "SMN"},
	})
	if got := e.CoreNodeGroups()["Supermarket Empty Totes"]; !reflect.DeepEqual(got, []string{"SMN_05"}) {
		t.Errorf("after a resync the group is %v, want [SMN_05] — membership is rebuilt, not accumulated", got)
	}
	// The copy handed out is a copy.
	e.CoreNodeGroups()["Supermarket Empty Totes"][0] = "MUTATED"
	if e.CoreNodeGroups()["Supermarket Empty Totes"][0] != "SMN_05" {
		t.Error("CoreNodeGroups handed out its own slice")
	}
}
