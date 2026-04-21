//go:build docker

package store

import "testing"

func TestBinTypeRoundTrip(t *testing.T) {
	db := testDB(t)

	bt := &BinType{
		Code:        "BT-RT-1",
		Description: "Round trip type",
		WidthIn:     10.0,
		HeightIn:    6.0,
	}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create: %v", err)
	}
	if bt.ID == 0 {
		t.Fatal("ID should be assigned by Create")
	}

	// Get by ID — verifies columns persisted
	got, err := db.GetBinType(bt.ID)
	if err != nil {
		t.Fatalf("GetBinType: %v", err)
	}
	if got.Code != "BT-RT-1" {
		t.Errorf("Code = %q, want %q", got.Code, "BT-RT-1")
	}
	if got.Description != "Round trip type" {
		t.Errorf("Description = %q, want %q", got.Description, "Round trip type")
	}
	if got.WidthIn != 10.0 || got.HeightIn != 6.0 {
		t.Errorf("dims = (%v, %v), want (10, 6)", got.WidthIn, got.HeightIn)
	}

	// Get by code — verifies the by-code reader sees the same row
	byCode, err := db.GetBinTypeByCode("BT-RT-1")
	if err != nil {
		t.Fatalf("GetBinTypeByCode: %v", err)
	}
	if byCode.ID != bt.ID {
		t.Errorf("ByCode ID = %d, want %d", byCode.ID, bt.ID)
	}

	// List — should contain exactly one row
	all, err := db.ListBinTypes()
	if err != nil {
		t.Fatalf("ListBinTypes: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list len = %d, want 1", len(all))
	}
	if all[0].Code != "BT-RT-1" {
		t.Errorf("list[0].Code = %q, want %q", all[0].Code, "BT-RT-1")
	}

	// Update — read back to confirm new values stuck
	got.Code = "BT-RT-1-V2"
	got.Description = "renamed"
	got.WidthIn = 20.0
	if err := db.UpdateBinType(got); err != nil {
		t.Fatalf("UpdateBinType: %v", err)
	}
	updated, err := db.GetBinType(bt.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if updated.Code != "BT-RT-1-V2" {
		t.Errorf("Code after update = %q, want %q", updated.Code, "BT-RT-1-V2")
	}
	if updated.Description != "renamed" {
		t.Errorf("Description after update = %q, want %q", updated.Description, "renamed")
	}
	if updated.WidthIn != 20.0 {
		t.Errorf("WidthIn after update = %v, want 20", updated.WidthIn)
	}

	// Delete — list should be empty
	if err := db.DeleteBinType(bt.ID); err != nil {
		t.Fatalf("DeleteBinType: %v", err)
	}
	empty, err := db.ListBinTypes()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("list after delete len = %d, want 0", len(empty))
	}
	if _, err := db.GetBinType(bt.ID); err == nil {
		t.Error("GetBinType after delete should return error")
	}
}
