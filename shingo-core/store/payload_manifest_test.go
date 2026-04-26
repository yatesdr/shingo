//go:build docker

package store

import (
	"testing"

	"shingocore/store/payloads"
)

func TestPayloadManifestCRUD(t *testing.T) {
	db := testDB(t)

	p := &payloads.Payload{Code: "PM-TMPL-1", UOPCapacity: 100}
	if err := db.CreatePayload(p); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	// Create one item
	item := &payloads.ManifestItem{
		PayloadID:   p.ID,
		PartNumber:  "PART-A",
		Quantity:    3,
		Description: "first part",
	}
	if err := db.CreatePayloadManifestItem(item); err != nil {
		t.Fatalf("CreatePayloadManifestItem: %v", err)
	}
	if item.ID == 0 {
		t.Fatal("item.ID should be set")
	}

	// Read back via ListPayloadManifest
	list, err := db.ListPayloadManifest(p.ID)
	if err != nil {
		t.Fatalf("ListPayloadManifest: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
	if list[0].PartNumber != "PART-A" || list[0].Quantity != 3 {
		t.Errorf("list[0] = (%q, %d), want (PART-A, 3)", list[0].PartNumber, list[0].Quantity)
	}
	if list[0].Description != "first part" {
		t.Errorf("Description = %q, want %q", list[0].Description, "first part")
	}

	// Update
	if err := db.UpdatePayloadManifestItem(item.ID, "PART-A-V2", 9); err != nil {
		t.Fatalf("UpdatePayloadManifestItem: %v", err)
	}
	afterUpdate, _ := db.ListPayloadManifest(p.ID)
	if afterUpdate[0].PartNumber != "PART-A-V2" {
		t.Errorf("PartNumber after update = %q", afterUpdate[0].PartNumber)
	}
	if afterUpdate[0].Quantity != 9 {
		t.Errorf("Quantity after update = %d, want 9", afterUpdate[0].Quantity)
	}

	// Delete
	if err := db.DeletePayloadManifestItem(item.ID); err != nil {
		t.Fatalf("DeletePayloadManifestItem: %v", err)
	}
	afterDelete, _ := db.ListPayloadManifest(p.ID)
	if len(afterDelete) != 0 {
		t.Errorf("after delete len = %d, want 0", len(afterDelete))
	}
}

func TestReplacePayloadManifest(t *testing.T) {
	db := testDB(t)

	p := &payloads.Payload{Code: "PM-TMPL-REPL", UOPCapacity: 50}
	db.CreatePayload(p)

	// Start with [A, B]
	initial := []*payloads.ManifestItem{
		{PayloadID: p.ID, PartNumber: "PART-A", Quantity: 1},
		{PayloadID: p.ID, PartNumber: "PART-B", Quantity: 2},
	}
	if err := db.ReplacePayloadManifest(p.ID, initial); err != nil {
		t.Fatalf("initial Replace: %v", err)
	}
	list1, _ := db.ListPayloadManifest(p.ID)
	if len(list1) != 2 {
		t.Fatalf("after initial replace len = %d, want 2", len(list1))
	}

	// Replace with [C] — A and B should be gone
	replacement := []*payloads.ManifestItem{
		{PayloadID: p.ID, PartNumber: "PART-C", Quantity: 5},
	}
	if err := db.ReplacePayloadManifest(p.ID, replacement); err != nil {
		t.Fatalf("replacement Replace: %v", err)
	}

	list2, err := db.ListPayloadManifest(p.ID)
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if len(list2) != 1 {
		t.Fatalf("after replace len = %d, want 1", len(list2))
	}
	if list2[0].PartNumber != "PART-C" {
		t.Errorf("PartNumber after replace = %q, want PART-C", list2[0].PartNumber)
	}
	if list2[0].Quantity != 5 {
		t.Errorf("Quantity after replace = %d, want 5", list2[0].Quantity)
	}
	// Ensure the replaced item got an ID
	if replacement[0].ID == 0 {
		t.Error("replaced item should have ID set")
	}

	// Replace with empty — list should be empty
	if err := db.ReplacePayloadManifest(p.ID, nil); err != nil {
		t.Fatalf("empty replace: %v", err)
	}
	list3, _ := db.ListPayloadManifest(p.ID)
	if len(list3) != 0 {
		t.Errorf("empty replace len = %d, want 0", len(list3))
	}
}
