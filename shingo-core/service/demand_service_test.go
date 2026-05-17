//go:build docker

package service

import (
	"shingo/protocol/testutil"
	"testing"
)

func TestDemandService_Create_PersistsRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	id, err := svc.Create("CAT-DEM-1", "demand one", 100)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id from Create")
	}

	got, err := db.GetDemand(id)
	if err != nil {
		t.Fatalf("GetDemand: %v", err)
	}
	if got.CatID != "CAT-DEM-1" || got.DemandQty != 100 || got.Description != "demand one" {
		t.Errorf("row = %+v, want CAT-DEM-1/100/demand one", got)
	}
}

func TestDemandService_Get_ReturnsRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	id, err := svc.Create("CAT-GET", "the widget", 50)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := svc.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != id || got.CatID != "CAT-GET" {
		t.Errorf("Get = %+v, want id=%d CatID=CAT-GET", got, id)
	}
}

func TestDemandService_Update_ChangesFields(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	id, _ := svc.Create("CAT-UPD", "first", 10)

	testutil.MustNoErr(t, svc.Update(id, "CAT-UPD", "second", 25, 7), "Update")
	got, _ := db.GetDemand(id)
	if got.Description != "second" || got.DemandQty != 25 || got.ProducedQty != 7 {
		t.Errorf("row = %+v, want second/25/7", got)
	}
}

func TestDemandService_UpdateAndResetProduced_ResetsToZero(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	id, _ := svc.Create("CAT-RST", "before", 10)
	testutil.MustNoErr(t, svc.SetProduced(id, 9), "SetProduced")

	testutil.MustNoErr(t, svc.UpdateAndResetProduced(id, "after", 40), "UpdateAndResetProduced")
	got, _ := db.GetDemand(id)
	if got.Description != "after" || got.DemandQty != 40 || got.ProducedQty != 0 {
		t.Errorf("row = %+v, want after/40/0", got)
	}
}

func TestDemandService_Delete_RemovesRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	id, _ := svc.Create("CAT-DEL", "", 10)

	testutil.MustNoErr(t, svc.Delete(id), "Delete")
	if _, err := db.GetDemand(id); err == nil {
		t.Error("GetDemand after Delete should error")
	}
}

func TestDemandService_List_ReturnsAll(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	for _, cat := range []string{"LA", "LB", "LC"} {
		if _, err := svc.Create(cat, cat, 1); err != nil {
			t.Fatalf("Create %s: %v", cat, err)
		}
	}
	rows, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("len(rows) = %d, want 3", len(rows))
	}
}

func TestDemandService_SetProduced_OverwritesValue(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	id, _ := svc.Create("CAT-SP", "", 10)
	testutil.MustNoErr(t, svc.SetProduced(id, 5), "SetProduced")
	got, _ := db.GetDemand(id)
	if got.ProducedQty != 5 {
		t.Errorf("ProducedQty = %d, want 5", got.ProducedQty)
	}
}

func TestDemandService_ClearProduced_ZeroesSingle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	a, _ := svc.Create("CAT-A", "", 10)
	b, _ := svc.Create("CAT-B", "", 10)
	_ = svc.SetProduced(a, 4)
	_ = svc.SetProduced(b, 6)

	testutil.MustNoErr(t, svc.ClearProduced(a), "ClearProduced")
	gotA, _ := db.GetDemand(a)
	gotB, _ := db.GetDemand(b)
	if gotA.ProducedQty != 0 {
		t.Errorf("A.ProducedQty = %d, want 0", gotA.ProducedQty)
	}
	if gotB.ProducedQty != 6 {
		t.Errorf("B.ProducedQty = %d, want 6 (unchanged)", gotB.ProducedQty)
	}
}

func TestDemandService_ClearAllProduced_ZeroesEverything(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	a, _ := svc.Create("CAT-AA", "", 10)
	b, _ := svc.Create("CAT-BB", "", 10)
	_ = svc.SetProduced(a, 4)
	_ = svc.SetProduced(b, 6)

	testutil.MustNoErr(t, svc.ClearAllProduced(), "ClearAllProduced")
	gotA, _ := db.GetDemand(a)
	gotB, _ := db.GetDemand(b)
	if gotA.ProducedQty != 0 || gotB.ProducedQty != 0 {
		t.Errorf("produced = %d/%d, want 0/0", gotA.ProducedQty, gotB.ProducedQty)
	}
}

func TestDemandService_ListProductionLog_EmptyWhenNoEntries(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewDemandService(db)

	// Fresh cat_id with no production log entries inserted.
	rows, err := svc.ListProductionLog("CAT-PLOG-EMPTY", 10)
	if err != nil {
		t.Fatalf("ListProductionLog: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0", len(rows))
	}

	// Sanity: database-side query returns the same zero result.
	dbRows, _ := db.ListProductionLog("CAT-PLOG-EMPTY", 10)
	if len(dbRows) != len(rows) {
		t.Errorf("db rows = %d, svc rows = %d, should match", len(dbRows), len(rows))
	}
}
