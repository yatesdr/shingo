//go:build docker

package store

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

func TestSetNodeBinTypes_Replaces(t *testing.T) {
	db := testDB(t)

	node := &nodes.Node{Name: "NBT-NODE-1", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	btA := &bins.BinType{Code: "NBT-A"}
	db.CreateBinType(btA)
	btB := &bins.BinType{Code: "NBT-B"}
	db.CreateBinType(btB)
	btC := &bins.BinType{Code: "NBT-C"}
	db.CreateBinType(btC)

	// First set: [A, B]
	if err := db.SetNodeBinTypes(node.ID, []int64{btA.ID, btB.ID}); err != nil {
		t.Fatalf("SetNodeBinTypes [A,B]: %v", err)
	}
	first, err := db.ListBinTypesForNode(node.ID)
	if err != nil {
		t.Fatalf("ListBinTypesForNode after [A,B]: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("after [A,B] len = %d, want 2", len(first))
	}

	codes := map[string]bool{}
	for _, bt := range first {
		codes[bt.Code] = true
	}
	if !codes["NBT-A"] || !codes["NBT-B"] {
		t.Errorf("after [A,B] codes = %+v, want NBT-A and NBT-B", codes)
	}

	// Replace with [C] — A and B should be gone
	if err := db.SetNodeBinTypes(node.ID, []int64{btC.ID}); err != nil {
		t.Fatalf("SetNodeBinTypes [C]: %v", err)
	}
	second, err := db.ListBinTypesForNode(node.ID)
	if err != nil {
		t.Fatalf("ListBinTypesForNode after [C]: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("after [C] len = %d, want 1", len(second))
	}
	if second[0].Code != "NBT-C" {
		t.Errorf("after [C] code = %q, want %q", second[0].Code, "NBT-C")
	}

	// Clear — empty list
	if err := db.SetNodeBinTypes(node.ID, nil); err != nil {
		t.Fatalf("SetNodeBinTypes nil: %v", err)
	}
	empty, _ := db.ListBinTypesForNode(node.ID)
	if len(empty) != 0 {
		t.Errorf("after clear len = %d, want 0", len(empty))
	}
}

func TestGetEffectiveBinTypes_Modes(t *testing.T) {
	db := testDB(t)

	node := &nodes.Node{Name: "NBT-EFF-1", Enabled: true}
	db.CreateNode(node)

	bt := &bins.BinType{Code: "NBT-EFF-T"}
	db.CreateBinType(bt)

	// "specific" mode — direct assignments
	db.SetNodeBinTypes(node.ID, []int64{bt.ID})
	db.SetNodeProperty(node.ID, "bin_type_mode", "specific")

	specific, err := db.GetEffectiveBinTypes(node.ID)
	if err != nil {
		t.Fatalf("GetEffectiveBinTypes specific: %v", err)
	}
	if len(specific) != 1 {
		t.Fatalf("specific len = %d, want 1", len(specific))
	}
	if specific[0].Code != "NBT-EFF-T" {
		t.Errorf("specific[0].Code = %q, want %q", specific[0].Code, "NBT-EFF-T")
	}

	// "all" mode — returns nil
	db.SetNodeProperty(node.ID, "bin_type_mode", "all")
	allRes, err := db.GetEffectiveBinTypes(node.ID)
	if err != nil {
		t.Fatalf("GetEffectiveBinTypes all: %v", err)
	}
	if allRes != nil {
		t.Errorf("all mode should return nil, got %d items", len(allRes))
	}

	// inherit mode — node has its own assignments, should find them
	db.SetNodeProperty(node.ID, "bin_type_mode", "inherit")
	inherited, err := db.GetEffectiveBinTypes(node.ID)
	if err != nil {
		t.Fatalf("GetEffectiveBinTypes inherit: %v", err)
	}
	if len(inherited) != 1 {
		t.Fatalf("inherit len = %d, want 1 (own assignment)", len(inherited))
	}
	if inherited[0].Code != "NBT-EFF-T" {
		t.Errorf("inherit[0].Code = %q, want %q", inherited[0].Code, "NBT-EFF-T")
	}
}

func TestGetEffectiveBinTypes_InheritFromParent(t *testing.T) {
	db := testDB(t)

	parent := &nodes.Node{Name: "NBT-PARENT", Enabled: true}
	db.CreateNode(parent)

	child := &nodes.Node{Name: "NBT-CHILD", Enabled: true, ParentID: &parent.ID}
	db.CreateNode(child)

	bt := &bins.BinType{Code: "NBT-INH"}
	db.CreateBinType(bt)

	// Assign at parent only, child should inherit
	db.SetNodeBinTypes(parent.ID, []int64{bt.ID})

	// Child has no direct assignments, no mode set => default "inherit"
	got, err := db.GetEffectiveBinTypes(child.ID)
	if err != nil {
		t.Fatalf("GetEffectiveBinTypes child: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("inherited from parent len = %d, want 1", len(got))
	}
	if got[0].Code != "NBT-INH" {
		t.Errorf("inherited code = %q, want %q", got[0].Code, "NBT-INH")
	}
}
