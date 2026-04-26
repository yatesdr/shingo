//go:build docker

package service

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// makePayload inserts a payload directly through the service and returns it.
func makePayload(t *testing.T, svc *PayloadService, code, desc string, uop int) *payloads.Payload {
	t.Helper()
	p := &payloads.Payload{Code: code, Description: desc, UOPCapacity: uop}
	if err := svc.Create(p); err != nil {
		t.Fatalf("Create payload %s: %v", code, err)
	}
	return p
}

func TestPayloadService_Create_PersistsRow(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)

	p := &payloads.Payload{Code: "PL-CR-1", Description: "first", UOPCapacity: 50}
	if err := svc.Create(p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.ID == 0 {
		t.Fatal("expected ID populated")
	}

	got, err := db.GetPayload(p.ID)
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	if got.Code != "PL-CR-1" || got.UOPCapacity != 50 {
		t.Errorf("row = %+v, want PL-CR-1/50", got)
	}
}

func TestPayloadService_GetByCode_ReturnsRow(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-GBC", "by code", 10)

	got, err := svc.GetByCode("PL-GBC")
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("GetByCode ID = %d, want %d", got.ID, p.ID)
	}
}

func TestPayloadService_Update_PersistsChanges(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-UPD", "old desc", 10)

	p.Description = "new desc"
	p.UOPCapacity = 25
	if err := svc.Update(p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := db.GetPayload(p.ID)
	if got.Description != "new desc" || got.UOPCapacity != 25 {
		t.Errorf("row = %+v, want new desc/25", got)
	}
}

func TestPayloadService_Delete_RemovesRow(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-DEL", "", 1)

	if err := svc.Delete(p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.GetPayload(p.ID); err == nil {
		t.Error("GetPayload after Delete should error")
	}
}

func TestPayloadService_List_ReturnsAll(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)

	makePayload(t, svc, "PL-L-A", "", 1)
	makePayload(t, svc, "PL-L-B", "", 1)
	makePayload(t, svc, "PL-L-C", "", 1)

	rows, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) < 3 {
		t.Errorf("len(rows) = %d, want >= 3", len(rows))
	}

	// Sanity check against direct db.
	dbRows, _ := db.ListPayloads()
	if len(dbRows) != len(rows) {
		t.Errorf("db rows = %d, svc rows = %d, should match", len(dbRows), len(rows))
	}
}

func TestPayloadService_CreateAndListManifest(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-MAN", "manifest", 10)

	item := &payloads.ManifestItem{
		PayloadID:  p.ID,
		PartNumber: "PART-1",
		Quantity:   3,
	}
	if err := svc.CreateManifestItem(item); err != nil {
		t.Fatalf("CreateManifestItem: %v", err)
	}
	if item.ID == 0 {
		t.Fatal("expected manifest item ID populated")
	}

	items, err := svc.ListManifest(p.ID)
	if err != nil {
		t.Fatalf("ListManifest: %v", err)
	}
	if len(items) != 1 || items[0].PartNumber != "PART-1" || items[0].Quantity != 3 {
		t.Errorf("items = %+v, want one PART-1/3", items)
	}
}

func TestPayloadService_UpdateManifestItem_PersistsChanges(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-UMI", "", 10)

	item := &payloads.ManifestItem{PayloadID: p.ID, PartNumber: "P-OLD", Quantity: 1}
	if err := svc.CreateManifestItem(item); err != nil {
		t.Fatalf("CreateManifestItem: %v", err)
	}

	if err := svc.UpdateManifestItem(item.ID, "P-NEW", 9); err != nil {
		t.Fatalf("UpdateManifestItem: %v", err)
	}
	rows, _ := db.ListPayloadManifest(p.ID)
	if len(rows) != 1 || rows[0].PartNumber != "P-NEW" || rows[0].Quantity != 9 {
		t.Errorf("rows = %+v, want one P-NEW/9", rows)
	}
}

func TestPayloadService_DeleteManifestItem_RemovesRow(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-DMI", "", 10)

	item := &payloads.ManifestItem{PayloadID: p.ID, PartNumber: "P", Quantity: 1}
	if err := svc.CreateManifestItem(item); err != nil {
		t.Fatalf("CreateManifestItem: %v", err)
	}
	if err := svc.DeleteManifestItem(item.ID); err != nil {
		t.Fatalf("DeleteManifestItem: %v", err)
	}
	rows, _ := db.ListPayloadManifest(p.ID)
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want 0 after delete", rows)
	}
}

func TestPayloadService_ReplaceManifest_SwapsList(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-RPL", "", 10)

	// Seed with one item, then replace with two.
	if err := svc.CreateManifestItem(&payloads.ManifestItem{PayloadID: p.ID, PartNumber: "OLD", Quantity: 1}); err != nil {
		t.Fatalf("seed CreateManifestItem: %v", err)
	}

	newItems := []*payloads.ManifestItem{
		{PayloadID: p.ID, PartNumber: "N1", Quantity: 4},
		{PayloadID: p.ID, PartNumber: "N2", Quantity: 5},
	}
	if err := svc.ReplaceManifest(p.ID, newItems); err != nil {
		t.Fatalf("ReplaceManifest: %v", err)
	}
	rows, _ := db.ListPayloadManifest(p.ID)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	parts := map[string]int64{rows[0].PartNumber: rows[0].Quantity, rows[1].PartNumber: rows[1].Quantity}
	if parts["N1"] != 4 || parts["N2"] != 5 {
		t.Errorf("parts = %+v, want N1:4 N2:5", parts)
	}
}

func TestPayloadService_SetAndListBinTypes(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-BTY", "", 10)

	bt1 := &bins.BinType{Code: "BT-A", Description: "a"}
	bt2 := &bins.BinType{Code: "BT-B", Description: "b"}
	if err := db.CreateBinType(bt1); err != nil {
		t.Fatalf("create bt1: %v", err)
	}
	if err := db.CreateBinType(bt2); err != nil {
		t.Fatalf("create bt2: %v", err)
	}

	if err := svc.SetBinTypes(p.ID, []int64{bt1.ID, bt2.ID}); err != nil {
		t.Fatalf("SetBinTypes: %v", err)
	}
	got, err := svc.ListBinTypes(p.ID)
	if err != nil {
		t.Fatalf("ListBinTypes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}

	// Replacing with empty list clears.
	if err := svc.SetBinTypes(p.ID, nil); err != nil {
		t.Fatalf("SetBinTypes(nil): %v", err)
	}
	got, _ = svc.ListBinTypes(p.ID)
	if len(got) != 0 {
		t.Errorf("after clear len(got) = %d, want 0", len(got))
	}
}

func TestPayloadService_ListCompatibleNodes_EmptyByDefault(t *testing.T) {
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-CNODES", "", 10)

	rows, err := svc.ListCompatibleNodes(p.ID)
	if err != nil {
		t.Fatalf("ListCompatibleNodes: %v", err)
	}
	// Sanity: matches direct *store.DB call.
	dbRows, _ := db.ListNodesForPayload(p.ID)
	if len(dbRows) != len(rows) {
		t.Errorf("db rows = %d, svc rows = %d, should match", len(dbRows), len(rows))
	}
}
