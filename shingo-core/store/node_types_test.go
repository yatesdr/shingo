//go:build docker

package store

import (
	"testing"

	"shingocore/store/nodes"
)

func TestNodeTypeRoundTrip(t *testing.T) {
	db := testDB(t)

	nt := &nodes.NodeType{
		Code:        "STOR-RT",
		Name:        "Storage Slot",
		Description: "Physical storage slot",
		IsSynthetic: false,
	}
	if err := db.CreateNodeType(nt); err != nil {
		t.Fatalf("CreateNodeType: %v", err)
	}
	if nt.ID == 0 {
		t.Fatal("ID should be assigned by Create")
	}

	got, err := db.GetNodeType(nt.ID)
	if err != nil {
		t.Fatalf("GetNodeType: %v", err)
	}
	if got.Code != "STOR-RT" {
		t.Errorf("Code = %q, want %q", got.Code, "STOR-RT")
	}
	if got.Name != "Storage Slot" {
		t.Errorf("Name = %q, want %q", got.Name, "Storage Slot")
	}
	if got.Description != "Physical storage slot" {
		t.Errorf("Description = %q, want %q", got.Description, "Physical storage slot")
	}
	if got.IsSynthetic {
		t.Error("IsSynthetic = true, want false")
	}

	byCode, err := db.GetNodeTypeByCode("STOR-RT")
	if err != nil {
		t.Fatalf("GetNodeTypeByCode: %v", err)
	}
	if byCode.ID != nt.ID {
		t.Errorf("ByCode ID = %d, want %d", byCode.ID, nt.ID)
	}

	all, err := db.ListNodeTypes()
	if err != nil {
		t.Fatalf("ListNodeTypes: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list len = %d, want 3", len(all))
	}
	found := false
	for _, nt := range all {
		if nt.Code == "STOR-RT" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("STOR-RT not found in list: %+v", all)
	}

	// Update
	got.Code = "STOR-RT-V2"
	got.Name = "Storage Slot V2"
	got.IsSynthetic = true
	if err := db.UpdateNodeType(got); err != nil {
		t.Fatalf("UpdateNodeType: %v", err)
	}
	updated, err := db.GetNodeType(nt.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if updated.Code != "STOR-RT-V2" {
		t.Errorf("Code after update = %q, want %q", updated.Code, "STOR-RT-V2")
	}
	if updated.Name != "Storage Slot V2" {
		t.Errorf("Name after update = %q, want %q", updated.Name, "Storage Slot V2")
	}
	if !updated.IsSynthetic {
		t.Error("IsSynthetic after update should be true")
	}

	// Delete
	if err := db.DeleteNodeType(nt.ID); err != nil {
		t.Fatalf("DeleteNodeType: %v", err)
	}
	empty, err := db.ListNodeTypes()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(empty) != 2 {
		t.Errorf("list after delete len = %d, want 2 (seeded types)", len(empty))
	}
	if _, err := db.GetNodeType(nt.ID); err == nil {
		t.Error("GetNodeType after delete should return error")
	}
}
